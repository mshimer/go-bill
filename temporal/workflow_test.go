package temporal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// TestGreetingWorkflow_Unit runs the workflow in the SDK's in-memory test
// environment. No Docker, no server, no network - this is the fast feedback loop
// and it is the test that runs under `go test -short ./temporal/...`.
func TestGreetingWorkflow_Unit(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(ComposeGreeting)

	env.ExecuteWorkflow(GreetingWorkflow, "World")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var greeting string
	require.NoError(t, env.GetWorkflowResult(&greeting))
	require.Equal(t, "Hello, World!", greeting)
}

// TestGreetingWorkflow_MockedActivity shows the same workflow with the activity
// mocked out, proving the workflow only depends on the activity's contract.
func TestGreetingWorkflow_MockedActivity(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(ComposeGreeting)
	env.OnActivity(ComposeGreeting, mock.Anything, "World").Return("Howdy, World!", nil)

	env.ExecuteWorkflow(GreetingWorkflow, "World")

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var greeting string
	require.NoError(t, env.GetWorkflowResult(&greeting))
	require.Equal(t, "Howdy, World!", greeting)
}

// TestComposeGreeting_EmptyName covers the activity's non-retryable error path
// directly, without a workflow around it.
func TestComposeGreeting_EmptyName(t *testing.T) {
	_, err := ComposeGreeting(context.Background(), "")
	require.Error(t, err)
}
