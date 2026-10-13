// Package demo wires the other packages in this module into one small system:
// an HTTP payment gateway that clients report usage to, Temporal orchestrating
// delivery and the billing lifecycle, and Lago metering and invoicing it.
//
// See README.md for the architecture and how to run it.
package demo

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"comfy/go-bill/lago"

	lagoapi "github.com/getlago/lago-go-client"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// TaskQueue is the Temporal task queue the billing worker listens on.
const TaskQueue = "go-bill-billing-tq"

// UsageReport is one piece of reported usage, as it travels from the gateway
// through Temporal to Lago.
type UsageReport struct {
	// TransactionID is the **idempotency key**. The gateway mints it once, and it
	// survives every retry, which is what makes redelivery safe: Lago rejects a
	// transaction id it has already seen.
	TransactionID string

	// SubscriptionID is the Lago subscription the usage belongs to. Usage
	// attaches to a subscription rather than a customer, since one customer can
	// hold several.
	SubscriptionID string

	// SessionID identifies the GPU session this usage came from. It rides along
	// as an event property for traceability and is not billed on.
	SessionID string

	// Credits is how many credits this piece of usage consumes.
	Credits int64
}

// CloseBillingInput asks for a subscription to be billed and closed out.
type CloseBillingInput struct {
	SubscriptionID string
	CustomerID     string
	Tier           string
}

// InvoiceSummary is what the gateway hands back when a billing period closes.
//
// It reports the usage invoice in three parts rather than one total, because
// that is what makes the credit model legible: what the usage cost, how much of
// it the allowance absorbed, and what is actually left to pay. TotalBilledCents
// then covers the whole period, since the tier fee was invoiced separately when
// the subscription started.
type InvoiceSummary struct {
	Number   string `json:"number"`
	Status   string `json:"status"`
	Currency string `json:"currency"`

	// UsageCents is the metered usage before credits are applied.
	UsageCents int `json:"usage_cents"`

	// PrepaidCreditCents is how much of that the granted credits covered.
	PrepaidCreditCents int `json:"prepaid_credit_cents"`

	// AmountDueCents is what is left on this invoice - the overage, if any.
	AmountDueCents int `json:"amount_due_cents"`

	// TotalBilledCents is everything the customer was invoiced this period: the
	// tier fee charged in advance plus any overage.
	TotalBilledCents int `json:"total_billed_cents"`

	// CreditsRemaining is the allowance left once the invoice settled.
	CreditsRemaining string `json:"credits_remaining"`

	// Units is the credits billed, as Lago reported them.
	Units string `json:"units"`
}

// Activities are the Temporal activities that talk to Lago. They hold the client
// rather than building one per call, so a worker dials Lago once.
type Activities struct {
	Lago       *lagoapi.Client
	MetricCode string
}

// ReportUsage forwards one usage report to Lago as a usage event.
//
// This is the activity Temporal retries, and it is safe to retry precisely
// because the transaction id is stable: a replay that Lago has already recorded
// comes back as 422 value_already_exist, which this treats as success. Without
// that, an at-least-once delivery guarantee would mean at-least-once *billing*.
func (a *Activities) ReportUsage(ctx context.Context, report UsageReport) error {
	activity.GetLogger(ctx).Info("reporting usage to lago",
		"transaction_id", report.TransactionID,
		"subscription_id", report.SubscriptionID,
		"credits", report.Credits)

	if report.TransactionID == "" || report.SubscriptionID == "" {
		// Retrying will never fix a malformed report, so fail it permanently
		// rather than letting it block the queue forever.
		return temporal.NewNonRetryableApplicationError(
			"usage report needs a transaction id and a subscription id", "InvalidUsageReport", nil)
	}

	_, lerr := a.Lago.Event().Create(ctx, &lagoapi.EventInput{
		TransactionID:          report.TransactionID,
		ExternalSubscriptionID: report.SubscriptionID,
		Code:                   a.MetricCode,
		// No timestamp: Lago stamps reception time. An event timestamped outside
		// the subscription's active window is silently dropped, and on a retry
		// the original client-side time could easily fall outside it.
		Properties: map[string]any{
			lago.CreditsField: report.Credits,
			"session_id":      report.SessionID,
		},
	})
	if isDuplicateEvent(lerr) {
		activity.GetLogger(ctx).Info("lago already had this transaction id, treating as delivered",
			"transaction_id", report.TransactionID)
		return nil
	}
	if lerr != nil {
		return fmt.Errorf("reporting usage event %s: %w", report.TransactionID, error(lerr))
	}
	return nil
}

// CloseBilling ends the subscription's billing period and returns the invoice.
//
// Terminating a subscription is what makes Lago bill a partial period: there is
// no "invoice now" endpoint, one-off invoices cannot carry metered usage, and
// progressive billing is a premium feature.
//
// Lago builds the invoice on its own background queue, so this polls. It is also
// written to survive being retried: a second attempt finds the subscription
// already terminated, which is not a failure if the invoice it was asked for
// exists.
func (a *Activities) CloseBilling(ctx context.Context, in CloseBillingInput) (InvoiceSummary, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("closing billing period", "subscription_id", in.SubscriptionID)

	if _, err := lago.TerminateSubscription(ctx, a.Lago, in.SubscriptionID); err != nil {
		// Do not give up here. On a retry the subscription is already terminated
		// and this call fails, but the invoice the first attempt triggered may
		// be on its way or already issued.
		logger.Warn("terminating subscription failed, still looking for the invoice",
			"subscription_id", in.SubscriptionID, "error", err)
	}

	// Matched on carrying a usage fee, not on invoice type: the tier fee invoice
	// raised at subscribe time and this usage invoice are both of type
	// "subscription", so only their fees tell them apart.
	invoice, err := lago.WaitForInvoiceMatching(ctx, a.Lago, in.CustomerID,
		"usage invoice", lago.HasChargeFee(a.MetricCode))
	if err != nil {
		return InvoiceSummary{}, fmt.Errorf("waiting for the invoice for %s: %w", in.CustomerID, err)
	}
	fee, err := lago.ChargeFee(invoice, a.MetricCode)
	if err != nil {
		return InvoiceSummary{}, err
	}

	summary := InvoiceSummary{
		Number:             invoice.Number,
		Status:             string(invoice.Status),
		Currency:           string(invoice.Currency),
		UsageCents:         fee.AmountCents,
		PrepaidCreditCents: invoice.PrepaidCreditAmountCents,
		AmountDueCents:     invoice.TotalAmountCents,
		Units:              fee.Units,
	}

	// The tier fee was invoiced separately when the subscription started, so the
	// period's real cost is the sum across the customer's invoices.
	if total, err := lago.TotalInvoicedCents(ctx, a.Lago, in.CustomerID); err != nil {
		logger.Warn("could not total the customer's invoices", "error", err)
	} else {
		summary.TotalBilledCents = total
	}

	// Credits move at settlement, so the balance is only meaningful now.
	if wallet, err := lago.Wallet(ctx, a.Lago, in.CustomerID); err != nil {
		logger.Warn("could not read the wallet", "error", err)
	} else {
		summary.CreditsRemaining = wallet.CreditsBalance
	}

	logger.Info("invoice issued", "number", summary.Number,
		"usage_cents", summary.UsageCents, "due_cents", summary.AmountDueCents)
	return summary, nil
}

// isDuplicateEvent reports whether err is Lago rejecting a transaction id it has
// already recorded, which for an idempotent producer means the work is done.
func isDuplicateEvent(err *lagoapi.Error) bool {
	if err == nil {
		return false
	}
	if err.HTTPStatusCode != 422 {
		return false
	}
	// Lago reports this as a per-field validation detail. Fall back to the raw
	// message, since the detail shape differs between single and batch calls.
	if err.ErrorDetail != nil {
		if details, derr := err.ErrorDetail.Details(); derr == nil {
			for _, messages := range details {
				if slices.Contains(messages, string(lagoapi.ErrorCodeAlreadyExist)) {
					return true
				}
			}
		}
	}
	return strings.Contains(err.Error(), string(lagoapi.ErrorCodeAlreadyExist))
}
