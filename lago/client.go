package lago

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	lagoapi "github.com/getlago/lago-go-client"
)

// NewClient builds a Lago API client pointed at this stack.
//
// Two things about the client are easy to get wrong. SetBaseURL appends /api/v1/
// itself, so baseURL must not already carry it. And SetUseIngestService must be
// left alone: it repoints event ingestion at Lago's hosted ingest service, which
// is not where a self-hosted instance lives. New() already initialises the ingest
// client to the same address, and SetBaseURL updates both.
func NewClient(baseURL string) *lagoapi.Client {
	return NewClientWithKey(baseURL, APIKey)
}

// NewClientWithKey is NewClient for an instance seeded with a different API key,
// such as a compose stack whose .env was edited.
func NewClientWithKey(baseURL, apiKey string) *lagoapi.Client {
	return lagoapi.New().SetApiKey(apiKey).SetBaseURL(baseURL)
}

// NewClient builds a client for this stack's API.
func (s *Stack) NewClient() *lagoapi.Client {
	return NewClient(s.BaseURL)
}

// asError converts the client's *lagoapi.Error into a plain error.
//
// This exists because every method on the Lago client returns *lagoapi.Error
// rather than error. Assigning a nil *lagoapi.Error straight into an error
// variable yields a non-nil interface holding a nil pointer, so the usual
// `if err != nil` reads as a failure on success. Funnel every client error
// through here, and never widen a *lagoapi.Error to error any other way.
func asError(e *lagoapi.Error) error {
	if e == nil {
		return nil
	}
	return e
}

// poll retries probe until it succeeds or timeout elapses.
//
// Lago does most of its real work in Sidekiq jobs, so almost every interesting
// assertion has to be made against eventual state: an accepted event is not yet
// aggregated usage, and a terminated subscription is not yet an invoice. Each
// attempt's error is kept so a timeout explains what was still wrong rather than
// just that time ran out.
func poll(ctx context.Context, timeout time.Duration, what string, probe func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, lastErr)
		}

		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := probe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%s attempt %d: %w", what, attempt, err)

		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready within %s: %w", what, timeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// waitForSeededOrg polls an authenticated endpoint until the seeded organization
// answers for its API key.
//
// This is the second readiness phase. A 200 from /health only means Rails is
// serving; with the default command it also implies migrations and seeding
// finished, but that is an inference about startup ordering rather than a fact
// about the organization. Asking a real authenticated endpoint turns it into one.
func waitForSeededOrg(ctx context.Context, baseURL string) error {
	client := NewClient(baseURL)
	return poll(ctx, readyTimeout, "lago api authenticated readiness", func(ctx context.Context) error {
		_, err := client.BillableMetric().GetList(ctx, &lagoapi.BillableMetricListInput{})
		return asError(err)
	})
}

// WaitForUsage polls current usage until the subscription has aggregated exactly
// wantUnits credits across wantEvents events.
//
// POST /events stores the event synchronously but resolves it against a
// subscription in a background job, so usage trails ingestion. Waiting for the
// exact expected totals rather than for "some usage" means the test cannot
// accidentally assert against a half-processed batch.
func WaitForUsage(
	ctx context.Context,
	client *lagoapi.Client,
	externalCustomerID, externalSubscriptionID string,
	wantUnits int64,
	wantEvents int,
) (*lagoapi.CustomerChargeUsage, error) {
	var found *lagoapi.CustomerChargeUsage

	err := poll(ctx, readyTimeout, "aggregated usage", func(ctx context.Context) error {
		usage, lerr := client.Customer().CurrentUsage(ctx, externalCustomerID, &lagoapi.CustomerUsageInput{
			ExternalSubscriptionID: externalSubscriptionID,
		})
		if err := asError(lerr); err != nil {
			return err
		}
		if len(usage.ChargesUsage) == 0 {
			// Lago reports no charge usage at all until the first event has been
			// attributed to the subscription.
			return errors.New("no charge usage reported yet")
		}
		// Index zero is safe while every tier plan carries exactly one charge.
		charge := usage.ChargesUsage[0]
		units, err := strconv.ParseFloat(charge.Units, 64)
		if err != nil {
			return fmt.Errorf("parsing reported units %q: %w", charge.Units, err)
		}
		if int64(units) != wantUnits || charge.EventsCount != wantEvents {
			return fmt.Errorf("have %s units over %d events, want %d over %d",
				charge.Units, charge.EventsCount, wantUnits, wantEvents)
		}
		found = &charge
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// InvoiceMatcher decides whether an invoice is the one a caller is waiting for.
type InvoiceMatcher func(*lagoapi.Invoice) bool

// HasChargeFee matches an invoice carrying a usage fee for a billable metric -
// in other words, the invoice that bills metered consumption.
func HasChargeFee(metricCode string) InvoiceMatcher {
	return func(invoice *lagoapi.Invoice) bool {
		_, err := ChargeFee(invoice, metricCode)
		return err == nil
	}
}

// HasSubscriptionFee matches an invoice carrying a plan's recurring fee - the
// one issued up front when a pay-in-advance subscription starts.
func HasSubscriptionFee() InvoiceMatcher {
	return func(invoice *lagoapi.Invoice) bool {
		_, err := SubscriptionFee(invoice)
		return err == nil
	}
}

// WaitForInvoiceMatching polls until the customer has an invoice satisfying
// match, and returns it fully populated.
//
// Matching on fees rather than on invoice type is deliberate. A customer here
// ends up with two invoices - the tier fee charged in advance when they
// subscribe, and the metered usage when the period closes - and **both carry
// invoice_type "subscription"**, so nothing in Lago's list filters tells them
// apart. What distinguishes them is which fees they hold.
//
// The two-step lookup is also required rather than defensive: Lago's invoice
// index renders without fees, so a listed invoice has no line items on it and
// only the show endpoint includes them. That is why every candidate has to be
// fetched before it can be matched.
func WaitForInvoiceMatching(
	ctx context.Context,
	client *lagoapi.Client,
	externalCustomerID, what string,
	match InvoiceMatcher,
) (*lagoapi.Invoice, error) {
	var found *lagoapi.Invoice

	err := poll(ctx, readyTimeout, what, func(ctx context.Context) error {
		invoices, err := fetchInvoices(ctx, client, externalCustomerID)
		if err != nil {
			return err
		}
		for i := range invoices {
			if match(&invoices[i]) {
				found = &invoices[i]
				return nil
			}
		}
		return fmt.Errorf("none of the customer's %d invoice(s) match yet", len(invoices))
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// TotalInvoicedCents sums everything a customer has been invoiced.
//
// This is the figure worth asserting on. The tier fee and the metered usage land
// on separate invoices, and a usage invoice that comes to zero may or may not be
// finalized depending on Lago's zero-amount settings, so the sum is stable where
// any single invoice is not.
func TotalInvoicedCents(ctx context.Context, client *lagoapi.Client, externalCustomerID string) (int, error) {
	invoices, err := fetchInvoices(ctx, client, externalCustomerID)
	if err != nil {
		return 0, err
	}
	var total int
	for i := range invoices {
		if invoices[i].Status == lagoapi.InvoiceStatusVoided {
			continue // a voided invoice is not owed.
		}
		total += invoices[i].TotalAmountCents
	}
	return total, nil
}

// fetchInvoices lists a customer's invoices and fetches each one in full, since
// the list endpoint omits fees.
func fetchInvoices(ctx context.Context, client *lagoapi.Client, externalCustomerID string) ([]lagoapi.Invoice, error) {
	list, lerr := client.Invoice().GetList(ctx, &lagoapi.InvoiceListInput{
		ExternalCustomerID: externalCustomerID,
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("listing invoices for %q: %w", externalCustomerID, err)
	}

	invoices := make([]lagoapi.Invoice, 0, len(list.Invoices))
	for _, summary := range list.Invoices {
		full, lerr := client.Invoice().Get(ctx, summary.LagoID.String())
		if err := asError(lerr); err != nil {
			return nil, fmt.Errorf("fetching invoice %s: %w", summary.LagoID, err)
		}
		invoices = append(invoices, *full)
	}
	return invoices, nil
}

// WaitForWalletBalance polls the customer's wallet until its settled credit
// balance reaches wantCredits.
//
// It reads credits_balance, the settled figure, which moves when an invoice is
// finalized and prepaid credits are actually drawn down. It deliberately does not
// read credits_ongoing_balance, the real-time uninvoiced figure: that is
// recomputed by Clock::RefreshWalletsOngoingBalanceJob, which returns early
// without a premium license, so on self-hosted OSS it never moves at all.
func WaitForWalletBalance(
	ctx context.Context,
	client *lagoapi.Client,
	externalCustomerID string,
	wantCredits int64,
) (*lagoapi.Wallet, error) {
	var found *lagoapi.Wallet

	what := fmt.Sprintf("wallet balance of %d credits for %s", wantCredits, externalCustomerID)
	err := poll(ctx, readyTimeout, what, func(ctx context.Context) error {
		wallet, lerr := client.CustomerWallet().Get(ctx, externalCustomerID, WalletCode)
		if err := asError(lerr); err != nil {
			return err
		}
		// Credits are decimal strings, and the server chooses the formatting -
		// "0" and "0.0" are both possible - so parse rather than compare text.
		balance, err := strconv.ParseFloat(wallet.CreditsBalance, 64)
		if err != nil {
			return fmt.Errorf("parsing credits balance %q: %w", wallet.CreditsBalance, err)
		}
		if int64(balance) != wantCredits {
			return fmt.Errorf("balance is %s credits, want %d", wallet.CreditsBalance, wantCredits)
		}
		found = wallet
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// Wallet reads a customer's credit wallet.
func Wallet(ctx context.Context, client *lagoapi.Client, externalCustomerID string) (*lagoapi.Wallet, error) {
	wallet, lerr := client.CustomerWallet().Get(ctx, externalCustomerID, WalletCode)
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("reading wallet for %q: %w", externalCustomerID, err)
	}
	return wallet, nil
}

// ChargeFee picks the usage fee for a billable metric out of an invoice.
//
// Indexing into Fees would be wrong twice over: these plans carry a monthly fee
// and so emit a subscription fee alongside the usage one, and Lago emits no fee
// at all for a charge that aggregated zero units.
func ChargeFee(invoice *lagoapi.Invoice, metricCode string) (*lagoapi.Fee, error) {
	return findFee(invoice, lagoapi.FeeItemCharge, metricCode)
}

// SubscriptionFee picks a plan's recurring fee out of an invoice.
func SubscriptionFee(invoice *lagoapi.Invoice) (*lagoapi.Fee, error) {
	return findFee(invoice, lagoapi.FeeItemSubscription, "")
}

// findFee searches an invoice's fees by item type, and by code when given one.
func findFee(invoice *lagoapi.Invoice, feeType lagoapi.FeeType, code string) (*lagoapi.Fee, error) {
	if invoice == nil {
		return nil, errors.New("no invoice")
	}
	for i := range invoice.Fees {
		fee := &invoice.Fees[i]
		if fee.Item.Type != feeType {
			continue
		}
		if code != "" && fee.Item.Code != code {
			continue
		}
		return fee, nil
	}
	if code != "" {
		return nil, fmt.Errorf("invoice %s has no %s fee for %q (%d fee(s) present)",
			invoice.LagoID, feeType, code, len(invoice.Fees))
	}
	return nil, fmt.Errorf("invoice %s has no %s fee (%d fee(s) present)",
		invoice.LagoID, feeType, len(invoice.Fees))
}
