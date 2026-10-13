package demo

import (
	"errors"
	"testing"

	"comfy/go-bill/lago"
	mockh "comfy/go-bill/mock-h"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// These tests run the real activity code against a fake Lago built with this
// module's own mock-h package, so the HTTP contract and - more importantly - the
// idempotency behavior are checked without Docker, without Rails and in
// milliseconds. The containerized test in demo_test.go then proves the same code
// works against the real thing.

// fakeLago starts a mock-h server standing in for Lago and returns it alongside
// activities pointed at it.
func fakeLago(t *testing.T) (*mockh.MockServer, *Activities) {
	t.Helper()
	server := mockh.MustStart(0)
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("closing fake lago: %v", err)
		}
	})
	return server, &Activities{
		Lago:       lago.NewClientWithKey(server.BaseURL(), "fake-api-key"),
		MetricCode: "credits",
	}
}

// runActivity executes an activity inside Temporal's activity test environment,
// which supplies the activity context the code expects for logging.
func runActivity(t *testing.T, activities *Activities, fn, arg any) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities)
	_, err := env.ExecuteActivity(fn, arg)
	return err
}

// TestReportUsage_SendsTheRightEvent pins the request the activity makes: the
// metric code, the idempotency key and the metered property all have to land
// where Lago expects them.
func TestReportUsage_SendsTheRightEvent(t *testing.T) {
	server, activities := fakeLago(t)
	server.MockResponse(mockh.Post("/api/v1/events"), mockh.JSON(`{"event":{"lago_id":"fake"}}`))

	err := runActivity(t, activities, activities.ReportUsage, UsageReport{
		TransactionID:  "txn-1",
		SubscriptionID: "sub-1",
		SessionID:      "session-1",
		Credits:        50,
	})
	require.NoError(t, err, "reporting usage against a healthy lago")

	requests := server.Requests()
	require.Len(t, requests, 1, "exactly one event should be posted")
	body := requests[0].BodyText()
	require.Contains(t, body, `"transaction_id":"txn-1"`, "idempotency key")
	require.Contains(t, body, `"external_subscription_id":"sub-1"`, "subscription the usage attaches to")
	require.Contains(t, body, `"code":"credits"`, "the billable metric's code")
	require.Contains(t, body, `"credits":50`, "the metered property")
	require.Equal(t, "Bearer fake-api-key", requests[0].Header("Authorization"), "api key")
}

// TestReportUsage_DuplicateIsSuccess is the important one. Temporal delivers
// at least once, so this activity will be retried after an ambiguous failure.
// Lago answering "I already have that transaction id" has to count as delivered,
// or at-least-once delivery would become at-least-once billing.
func TestReportUsage_DuplicateIsSuccess(t *testing.T) {
	server, activities := fakeLago(t)
	server.MockResponse(mockh.Post("/api/v1/events"),
		mockh.JSON(`{"status":422,"error":"Unprocessable Entity","code":"validation_errors",`+
			`"error_details":{"transaction_id":["value_already_exist"]}}`).WithStatus(422))

	err := runActivity(t, activities, activities.ReportUsage, UsageReport{
		TransactionID:  "txn-replayed",
		SubscriptionID: "sub-1",
		Credits:        50,
	})
	require.NoError(t, err, "a replayed transaction id should be treated as already delivered")
	require.Equal(t, 1, server.ReceivedCount(mockh.Post("/api/v1/events")), "the attempt still happened")
}

// TestReportUsage_ServerErrorFails checks the opposite: a genuine failure must
// surface so Temporal retries it rather than silently losing revenue.
func TestReportUsage_ServerErrorFails(t *testing.T) {
	server, activities := fakeLago(t)
	server.MockResponse(mockh.Post("/api/v1/events"), mockh.Text(500, "boom"))

	err := runActivity(t, activities, activities.ReportUsage, UsageReport{
		TransactionID:  "txn-2",
		SubscriptionID: "sub-1",
		Credits:        50,
	})
	require.Error(t, err, "a 500 from lago must not be swallowed")
}

// TestReportUsage_InvalidReportIsNonRetryable proves a malformed report fails
// permanently. With unlimited retries on this activity, a report that can never
// succeed would otherwise block the subscription's billing forever.
func TestReportUsage_InvalidReportIsNonRetryable(t *testing.T) {
	server, activities := fakeLago(t)
	server.MockResponse(mockh.Post("/api/v1/events"), mockh.JSON(`{"event":{}}`))

	err := runActivity(t, activities, activities.ReportUsage, UsageReport{
		SubscriptionID: "sub-1", // no transaction id
		Credits:        50,
	})
	require.Error(t, err, "a report with no transaction id should fail")

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr), "expected a temporal application error, got %T: %v", err, err)
	require.True(t, appErr.NonRetryable(), "an unfixable report must not be retried")
	require.Empty(t, server.Requests(), "an invalid report should never reach lago")
}
