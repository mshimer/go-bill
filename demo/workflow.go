package demo

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Signal and query names the gateway uses to drive a billing workflow.
const (
	// ReportUsageSignal delivers one UsageReport.
	ReportUsageSignal = "report-usage"

	// CloseBillingSignal ends the billing period and triggers invoicing.
	CloseBillingSignal = "close-billing"

	// UsageQuery reads the running total without touching Lago.
	UsageQuery = "usage"
)

// BillingWorkflowInput starts a billing period for one subscription.
type BillingWorkflowInput struct {
	SubscriptionID string
	CustomerID     string

	// Tier and CreditAllowance describe the plan the customer chose. The
	// allowance lets the workflow answer "how many credits are left" from its
	// own state, without asking Lago.
	Tier            string
	CreditAllowance int64

	// BillingPeriod closes the period automatically once it elapses. Zero means
	// the period only ends when CloseBillingSignal arrives, which is what the
	// demo does so it does not have to wait out a real month.
	BillingPeriod time.Duration
}

// UsageState is what UsageQuery returns: the running credit balance, straight
// out of workflow state.
//
// This is a bigger deal than it looks. Lago's own real-time credit balance
// (credits_ongoing_balance) is recomputed by a job that returns early without a
// premium license, so on self-hosted OSS it never moves - Lago only settles
// credits when an invoice is finalized. Temporal holding the running total
// durably is therefore what makes a live "credits remaining" readout possible at
// all, with Lago remaining the authority at settlement.
type UsageState struct {
	SubscriptionID string `json:"subscription_id"`
	CustomerID     string `json:"customer_id"`
	Tier           string `json:"tier"`

	// CreditAllowance is what the tier included this period.
	CreditAllowance int64 `json:"credit_allowance"`

	// CreditsConsumed is everything reported, including usage past the
	// allowance - that usage is billed rather than drawn from the balance.
	CreditsConsumed int64 `json:"credits_consumed"`

	// CreditsRemaining floors at zero: an exhausted allowance means overage
	// billed in currency, not a negative balance.
	CreditsRemaining int64 `json:"credits_remaining"`

	// CreditsOverdrawn is how far past the allowance usage went.
	CreditsOverdrawn int64 `json:"credits_overdrawn"`

	EventsReported int  `json:"events_reported"`
	EventsRejected int  `json:"events_rejected"`
	Closed         bool `json:"closed"`
}

// activityStub exists only so workflow code can name activities in a way the
// compiler checks. It is always nil: the SDK resolves the method to its
// registered name by reflection and never calls through the pointer.
var activityStub *Activities

// BillingWorkflow owns one subscription's billing period.
//
// It is the reason Temporal is in this design rather than the gateway posting
// straight to Lago. Three things fall out of running the period as a workflow:
//
//   - Durable delivery. The gateway's job ends once Temporal has the signal, so
//     it can answer in milliseconds and stay up while Lago is down. Delivery is
//     then Temporal's problem, and the activity retries indefinitely.
//   - Per-subscription state. The workflow id is derived from the subscription,
//     so Temporal gives one serialised, durable, queryable accumulator per
//     subscription with no database of our own. That accumulator is what serves
//     the live credit balance, which self-hosted Lago cannot: its real-time
//     ongoing balance is a premium feature, and its settled balance only moves
//     when an invoice is finalized.
//   - A lifecycle. "Collect usage until the period ends, then invoice" is
//     exactly a long-running workflow with a timer and a signal.
//
// One workflow runs per subscription, started on demand by the gateway with
// signal-with-start, so neither side has to track whether it already exists.
func BillingWorkflow(ctx workflow.Context, in BillingWorkflowInput) (InvoiceSummary, error) {
	logger := workflow.GetLogger(ctx)
	state := UsageState{
		SubscriptionID:   in.SubscriptionID,
		CustomerID:       in.CustomerID,
		Tier:             in.Tier,
		CreditAllowance:  in.CreditAllowance,
		CreditsRemaining: in.CreditAllowance,
	}

	if err := workflow.SetQueryHandler(ctx, UsageQuery, func() (UsageState, error) {
		return state, nil
	}); err != nil {
		return InvoiceSummary{}, err
	}

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			// No MaximumAttempts on purpose: usage must not be dropped because
			// Lago happened to be restarting. The workflow waits it out, and the
			// stable transaction id keeps the retries safe.
		},
	})

	reports := workflow.GetSignalChannel(ctx, ReportUsageSignal)
	closes := workflow.GetSignalChannel(ctx, CloseBillingSignal)

	// An optional deadline for the whole period. Cancelled on the way out so a
	// closed workflow does not leave a timer pending.
	var periodTimer workflow.Future
	if in.BillingPeriod > 0 {
		timerCtx, cancelTimer := workflow.WithCancel(ctx)
		defer cancelTimer()
		periodTimer = workflow.NewTimer(timerCtx, in.BillingPeriod)
	}

	for !state.Closed {
		selector := workflow.NewSelector(ctx)

		selector.AddReceive(reports, func(c workflow.ReceiveChannel, _ bool) {
			var report UsageReport
			c.Receive(ctx, &report)
			// Trust the workflow's own identity over the signal payload's.
			report.SubscriptionID = in.SubscriptionID

			// Deliberately synchronous: further signals queue in the channel
			// while this one is in flight, so usage reaches Lago in the order it
			// was reported and the running total never races.
			if err := workflow.ExecuteActivity(ctx, activityStub.ReportUsage, report).Get(ctx, nil); err != nil {
				// Retries are unlimited, so reaching here means the report is
				// permanently bad - malformed, or rejected outright. Count it
				// and keep the period alive rather than failing the workflow.
				logger.Error("dropping usage report", "transaction_id", report.TransactionID, "error", err)
				state.EventsRejected++
				return
			}
			state.CreditsConsumed += report.Credits
			state.EventsReported++
			state.CreditsRemaining = max(0, in.CreditAllowance-state.CreditsConsumed)
			state.CreditsOverdrawn = max(0, state.CreditsConsumed-in.CreditAllowance)
		})

		selector.AddReceive(closes, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			logger.Info("billing period closed by signal", "subscription_id", in.SubscriptionID)
			state.Closed = true
		})

		if periodTimer != nil {
			selector.AddFuture(periodTimer, func(workflow.Future) {
				logger.Info("billing period elapsed", "subscription_id", in.SubscriptionID)
				state.Closed = true
			})
		}

		selector.Select(ctx)
	}

	var summary InvoiceSummary
	err := workflow.ExecuteActivity(ctx, activityStub.CloseBilling, CloseBillingInput{
		SubscriptionID: in.SubscriptionID,
		CustomerID:     in.CustomerID,
		Tier:           in.Tier,
	}).Get(ctx, &summary)
	if err != nil {
		return InvoiceSummary{}, err
	}
	return summary, nil
}

// WorkflowID is the workflow that owns a subscription's billing period.
//
// Deriving it from the subscription is what makes signal-with-start work: the
// gateway can report usage without knowing whether a period is already open,
// and Temporal guarantees there is only ever one.
func WorkflowID(subscriptionID string) string {
	return "billing-" + subscriptionID
}
