// Package temporal contains a small but real Temporal workflow plus the
// integration-test harness that runs it against a containerized Temporal
// dev server (see container_test.go).
package temporal

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the task queue used by both the worker and the client in tests.
const TaskQueue = "go-bill-greeting-tq"

// ComposeGreeting is the single activity exercised by GreetingWorkflow. It is a
// plain function so it can be registered directly on a worker and mocked in the
// SDK's test environment.
func ComposeGreeting(ctx context.Context, name string) (string, error) {
	if name == "" {
		// A non-retryable application error: there is no point retrying a bad input.
		return "", temporal.NewNonRetryableApplicationError(
			"name must not be empty", "InvalidGreetingName", nil,
		)
	}
	return fmt.Sprintf("Hello, %s!", name), nil
}

// GreetingWorkflow is a deliberately minimal workflow: it calls one activity and
// returns its result. Everything interesting here is in the plumbing around it.
func GreetingWorkflow(ctx workflow.Context, name string) (string, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    100 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Second,
			MaximumAttempts:    3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	logger := workflow.GetLogger(ctx)
	logger.Info("GreetingWorkflow started", "name", name)

	var greeting string
	if err := workflow.ExecuteActivity(ctx, ComposeGreeting, name).Get(ctx, &greeting); err != nil {
		return "", fmt.Errorf("ComposeGreeting activity failed: %w", err)
	}

	logger.Info("GreetingWorkflow completed", "greeting", greeting)
	return greeting, nil
}
