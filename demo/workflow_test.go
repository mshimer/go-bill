package demo

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// These exercise the orchestration with Lago mocked out entirely, so they say
// something about the workflow's logic rather than about billing: what it does
// with signals, when it closes, and what happens when a report cannot be
// delivered. No Docker, no network.

func testEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *Activities) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := &Activities{MetricCode: "credits"}
	env.RegisterActivity(activities)
	return env, activities
}

// tier-1 in the real catalog: $20 a month including 2000 credits at $0.01.
const testAllowance = 2000

var sampleInvoice = InvoiceSummary{
	Number:             "GO--TEST-001",
	Status:             "finalized",
	Currency:           "USD",
	UsageCents:         2400,
	PrepaidCreditCents: 2000,
	AmountDueCents:     400,
	TotalBilledCents:   2400,
	CreditsRemaining:   "0.0",
	Units:              "2400.0",
}

// tierInput is the workflow argument used throughout these tests.
func tierInput() BillingWorkflowInput {
	return BillingWorkflowInput{
		SubscriptionID:  "sub-1",
		CustomerID:      "cust-1",
		Tier:            "tier-1",
		CreditAllowance: testAllowance,
	}
}

// TestBillingWorkflow_AccumulatesThenInvoices is the main path: usage arrives as
// signals, the workflow draws it against the allowance, and closing the period
// produces an invoice.
func TestBillingWorkflow_AccumulatesThenInvoices(t *testing.T) {
	env, activities := testEnv(t)

	env.OnActivity(activities.ReportUsage, mock.Anything, mock.Anything).Return(nil).Times(6)
	env.OnActivity(activities.CloseBilling, mock.Anything, CloseBillingInput{
		SubscriptionID: "sub-1",
		CustomerID:     "cust-1",
		Tier:           "tier-1",
	}).Return(sampleInvoice, nil).Once()

	// Signals are delivered on the workflow's own clock, which the test
	// environment fast-forwards.
	for i := 0; i < 6; i++ {
		delay := time.Duration(i+1) * time.Second
		report := UsageReport{
			TransactionID: "txn-" + string(rune('a'+i)),
			Credits:       250,
		}
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(ReportUsageSignal, report)
		}, delay)
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CloseBillingSignal, nil)
	}, 10*time.Second)

	env.ExecuteWorkflow(BillingWorkflow, tierInput())

	require.True(t, env.IsWorkflowCompleted(), "workflow should have completed")
	require.NoError(t, env.GetWorkflowError(), "workflow error")

	var got InvoiceSummary
	require.NoError(t, env.GetWorkflowResult(&got), "decoding the workflow result")
	require.Equal(t, sampleInvoice, got, "the invoice the workflow returned")

	// The live credit balance the gateway serves comes from this query.
	state := queryUsage(t, env)
	require.EqualValues(t, 1500, state.CreditsConsumed, "credits consumed")
	require.EqualValues(t, 500, state.CreditsRemaining, "credits remaining of the 2000 allowance")
	require.Zero(t, state.CreditsOverdrawn, "1500 of 2000 is not overdrawn")
	require.Equal(t, 6, state.EventsReported, "events reported")
	require.Zero(t, state.EventsRejected, "nothing should have been rejected")
	require.True(t, state.Closed, "period should be closed")

	env.AssertExpectations(t)
}

// TestBillingWorkflow_ClosesWhenPeriodElapses covers the other way a period
// ends: a timer rather than a signal. This is how real monthly billing would
// work, and the test environment skips the clock forward rather than waiting.
func TestBillingWorkflow_ClosesWhenPeriodElapses(t *testing.T) {
	env, activities := testEnv(t)

	env.OnActivity(activities.ReportUsage, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(activities.CloseBilling, mock.Anything, mock.Anything).Return(sampleInvoice, nil).Once()

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(ReportUsageSignal, UsageReport{TransactionID: "txn-1", Credits: 120})
	}, time.Minute)

	input := tierInput()
	input.BillingPeriod = time.Hour // no real waiting: the clock is skipped
	env.ExecuteWorkflow(BillingWorkflow, input)

	require.True(t, env.IsWorkflowCompleted(), "the elapsed period should close the workflow")
	require.NoError(t, env.GetWorkflowError(), "workflow error")

	state := queryUsage(t, env)
	require.EqualValues(t, 120, state.CreditsConsumed, "usage reported before the period elapsed")
	require.EqualValues(t, 1880, state.CreditsRemaining, "credits remaining")
	require.True(t, state.Closed, "period should be closed")

	env.AssertExpectations(t)
}

// TestBillingWorkflow_UndeliverableReportIsRejected checks the workflow survives
// a report that can never be delivered. One bad report must not take the
// subscription's whole billing period down with it, nor be counted as usage.
func TestBillingWorkflow_UndeliverableReportIsRejected(t *testing.T) {
	env, activities := testEnv(t)

	fatal := temporal.NewNonRetryableApplicationError("bad report", "InvalidUsageReport", nil)
	env.OnActivity(activities.ReportUsage, mock.Anything, mock.MatchedBy(func(r UsageReport) bool {
		return r.TransactionID == "txn-bad"
	})).Return(fatal).Once()
	env.OnActivity(activities.ReportUsage, mock.Anything, mock.MatchedBy(func(r UsageReport) bool {
		return r.TransactionID == "txn-good"
	})).Return(nil).Once()
	env.OnActivity(activities.CloseBilling, mock.Anything, mock.Anything).Return(sampleInvoice, nil).Once()

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(ReportUsageSignal, UsageReport{TransactionID: "txn-bad", Credits: 999})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(ReportUsageSignal, UsageReport{TransactionID: "txn-good", Credits: 50})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CloseBillingSignal, nil)
	}, 3*time.Second)

	env.ExecuteWorkflow(BillingWorkflow, tierInput())

	require.True(t, env.IsWorkflowCompleted(), "a bad report should not kill the workflow")
	require.NoError(t, env.GetWorkflowError(), "workflow error")

	state := queryUsage(t, env)
	require.EqualValues(t, 50, state.CreditsConsumed, "the undeliverable report must not spend credits")
	require.Equal(t, 1, state.EventsReported, "only the good report counts as reported")
	require.Equal(t, 1, state.EventsRejected, "the bad report is counted as rejected")

	env.AssertExpectations(t)
}

// TestBillingWorkflow_InvoicingFailureFailsTheWorkflow states the deliberate
// asymmetry: a single bad usage report is tolerated, but failing to invoice at
// all is not something to paper over.
func TestBillingWorkflow_InvoicingFailureFailsTheWorkflow(t *testing.T) {
	env, activities := testEnv(t)

	env.OnActivity(activities.CloseBilling, mock.Anything, mock.Anything).
		Return(InvoiceSummary{}, errors.New("lago never produced an invoice"))

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CloseBillingSignal, nil)
	}, time.Second)

	env.ExecuteWorkflow(BillingWorkflow, tierInput())

	require.True(t, env.IsWorkflowCompleted(), "workflow should have finished")
	require.Error(t, env.GetWorkflowError(), "a failure to invoice should surface")
}

func queryUsage(t *testing.T, env *testsuite.TestWorkflowEnvironment) UsageState {
	t.Helper()
	encoded, err := env.QueryWorkflow(UsageQuery)
	require.NoError(t, err, "querying usage")
	var state UsageState
	require.NoError(t, encoded.Get(&state), "decoding usage state")
	return state
}

// TestBillingWorkflow_TracksOverage covers usage past the allowance. The credit
// balance floors at zero rather than going negative - overage is billed in
// currency - and the overdrawn amount is tracked separately so the gateway can
// show a customer they have gone over.
func TestBillingWorkflow_TracksOverage(t *testing.T) {
	env, activities := testEnv(t)

	env.OnActivity(activities.ReportUsage, mock.Anything, mock.Anything).Return(nil).Times(6)
	env.OnActivity(activities.CloseBilling, mock.Anything, mock.Anything).Return(sampleInvoice, nil).Once()

	// Six reports of 400 credits is 2400 against a 2000 allowance.
	for i := 0; i < 6; i++ {
		report := UsageReport{
			TransactionID: "txn-over-" + string(rune('a'+i)),
			Credits:       400,
		}
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(ReportUsageSignal, report)
		}, time.Duration(i+1)*time.Second)
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CloseBillingSignal, nil)
	}, 10*time.Second)

	env.ExecuteWorkflow(BillingWorkflow, tierInput())

	require.True(t, env.IsWorkflowCompleted(), "workflow should have completed")
	require.NoError(t, env.GetWorkflowError(), "workflow error")

	state := queryUsage(t, env)
	require.EqualValues(t, 2400, state.CreditsConsumed, "credits consumed")
	require.Zero(t, state.CreditsRemaining, "an exhausted allowance should floor at zero")
	require.EqualValues(t, 400, state.CreditsOverdrawn, "credits past the allowance")

	env.AssertExpectations(t)
}
