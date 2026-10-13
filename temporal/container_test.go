package temporal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Guarding strategy (documented in README.md): this test is gated on
// testing.Short() rather than a build tag, so the default `go test ./temporal/...`
// exercises the real container while `go test -short ./temporal/...` runs only
// the in-memory suite. If Docker is reachable but broken, or simply absent, the
// test skips with the underlying error instead of failing.
func TestGreetingWorkflow_AgainstContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping containerized Temporal test in -short mode")
	}
	skipIfNoDocker(t)

	// Generous: a cold image pull plus server boot plus the namespace poll.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	start := time.Now()
	srv, err := StartDevServer(ctx)
	t.Cleanup(func() {
		// Terminate with a fresh context: the test context may already be done.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
		defer stopCancel()
		if stopErr := srv.Stop(stopCtx); stopErr != nil {
			t.Logf("stopping dev server: %v", stopErr)
		}
	})
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker unavailable, skipping: %v", err)
		}
		require.NoError(t, err, "starting containerized Temporal dev server")
	}
	t.Logf("Temporal dev server ready at %s after %s", srv.HostPort, time.Since(start).Round(time.Millisecond))

	// One worker, in-process, polling the containerized server.
	w := worker.New(srv.Client, TaskQueue, worker.Options{})
	w.RegisterWorkflow(GreetingWorkflow)
	w.RegisterActivity(ComposeGreeting)
	require.NoError(t, w.Start(), "starting worker")
	t.Cleanup(w.Stop)

	t.Run("activity result flows back through the workflow", func(t *testing.T) {
		run, err := srv.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:        "greeting-" + uuid.NewString(),
			TaskQueue: TaskQueue,
		}, GreetingWorkflow, "Temporal")
		require.NoError(t, err, "ExecuteWorkflow")

		getCtx, getCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer getCancel()

		var greeting string
		require.NoError(t, run.Get(getCtx, &greeting), "waiting for workflow result")
		require.Equal(t, "Hello, Temporal!", greeting)

		// The run really went through the server: it has a durable run ID.
		require.NotEmpty(t, run.GetRunID())
	})

	t.Run("non-retryable activity error fails the workflow", func(t *testing.T) {
		run, err := srv.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:        "greeting-empty-" + uuid.NewString(),
			TaskQueue: TaskQueue,
		}, GreetingWorkflow, "")
		require.NoError(t, err, "ExecuteWorkflow")

		getCtx, getCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer getCancel()

		var greeting string
		err = run.Get(getCtx, &greeting)
		require.Error(t, err, "expected the workflow to fail on an empty name")
		require.Contains(t, err.Error(), "name must not be empty")
	})
}

// skipIfNoDocker probes the Docker daemon up front so a missing daemon produces
// a clean skip rather than a container-start failure deep in the test.
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("GO_BILL_REQUIRE_DOCKER") != "" {
		return // caller insists Docker is present: let any failure surface.
	}
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("Docker provider unavailable, skipping: %v", err)
	}
	defer func() { _ = provider.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		t.Skipf("Docker daemon not healthy, skipping: %v", err)
	}
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false // a timeout is a real failure, not a missing daemon.
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"cannot connect to the docker daemon",
		"docker daemon is not running",
		"rootless docker not found",
		"failed to find a working container runtime",
		"permission denied while trying to connect to the docker daemon",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
