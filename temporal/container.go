package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

const (
	// DevServerImage is the Temporal CLI image. Its entrypoint is the `temporal`
	// binary, so we drive it with `server start-dev`, which runs a full
	// single-process Temporal server backed by in-memory SQLite. This is far
	// lighter than temporalio/auto-setup, which needs an external
	// Cassandra/Postgres/MySQL plus Elasticsearch to come up first.
	DevServerImage = "temporalio/temporal:1.9.1"

	// FrontendPort is the gRPC frontend port inside the container.
	FrontendPort = "7233/tcp"

	// DefaultNamespace is the namespace `start-dev` auto-registers.
	DefaultNamespace = "default"

	// startupTimeout bounds image pull plus server boot. A cold pull of the
	// image can take a while, hence the generous budget.
	startupTimeout = 5 * time.Minute

	// readyTimeout bounds the post-start "is the namespace usable yet" poll.
	readyTimeout = 90 * time.Second
)

// DevServer is a running containerized Temporal dev server together with a
// connected SDK client.
type DevServer struct {
	Container testcontainers.Container
	Client    client.Client // Temporal Client
	HostPort  string
}

// Stop closes the client and terminates the container. Safe to call on a
// partially constructed DevServer.
func (d *DevServer) Stop(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if d.Client != nil {
		d.Client.Close()
	}
	if d.Container != nil {
		return testcontainers.TerminateContainer(d.Container)
	}
	return nil
}

// StartDevServer boots DevServerImage via testcontainers, maps the frontend
// port to an ephemeral host port, and returns a client that has been proven to
// work against the default namespace.
//
// Two-phase readiness is deliberate. The testcontainers wait strategy only gets
// us as far as "the frontend is listening and has logged its banner" - the gRPC
// listener accepts connections before the default namespace has been registered,
// so a plain port-open (or even log-line) wait is not sufficient. Phase two
// polls client.Dial + DescribeNamespace until the namespace actually resolves.
func StartDevServer(ctx context.Context, opts ...client.Options) (*DevServer, error) {
	clientOpts := client.Options{}
	if len(opts) > 0 {
		clientOpts = opts[0]
	}
	if clientOpts.Namespace == "" {
		clientOpts.Namespace = DefaultNamespace
	}

	req := testcontainers.ContainerRequest{
		Image:        DevServerImage,
		ExposedPorts: []string{FrontendPort},
		Cmd: []string{
			"server", "start-dev",
			// Bind to all interfaces so the mapped port reaches the frontend.
			"--ip", "0.0.0.0",
			"--port", "7233",
			"--namespace", clientOpts.Namespace,
			// No UI server: we only talk gRPC, and this shaves startup time.
			"--headless",
			// The dev server phones home for a version check otherwise.
			"--dynamic-config-value", "frontend.enableServerVersionCheck=false",
			"--log-level", "warn",
		},
		WaitingFor: wait.ForAll(
			// The banner is only printed once the frontend has bound its port.
			wait.ForLog("Temporal Server:").WithStartupTimeout(startupTimeout),
			wait.ForListeningPort(FrontendPort).WithStartupTimeout(startupTimeout),
		).WithStartupTimeoutDefault(startupTimeout),
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		// Hand back the container (possibly non-nil) so the caller can reap it.
		return &DevServer{Container: ctr}, fmt.Errorf("starting %s: %w", DevServerImage, err)
	}

	srv := &DevServer{Container: ctr}

	host, err := ctr.Host(ctx)
	if err != nil {
		return srv, fmt.Errorf("resolving container host: %w", err)
	}
	mapped, err := ctr.MappedPort(ctx, FrontendPort)
	if err != nil {
		return srv, fmt.Errorf("resolving mapped frontend port: %w", err)
	}
	srv.HostPort = fmt.Sprintf("%s:%s", host, mapped.Port())

	clientOpts.HostPort = srv.HostPort
	if clientOpts.Logger == nil {
		// The SDK is chatty on connection retries; keep test output readable.
		clientOpts.Logger = discardLogger{}
	}

	c, err := dialUntilNamespaceReady(ctx, clientOpts)
	if err != nil {
		return srv, err
	}
	srv.Client = c
	return srv, nil
}

// dialUntilNamespaceReady retries Dial + DescribeNamespace until the namespace
// is registered and serving, or readyTimeout elapses.
func dialUntilNamespaceReady(ctx context.Context, opts client.Options) (client.Client, error) {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, lastErr)
		}

		c, err := client.DialContext(ctx, opts)
		if err != nil {
			lastErr = fmt.Errorf("dial attempt %d: %w", attempt, err)
		} else {
			// Dial succeeding is not proof of readiness: confirm the namespace
			// resolves through the frontend before handing the client back.
			describeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err = c.WorkflowService().DescribeNamespace(describeCtx,
				&workflowservice.DescribeNamespaceRequest{Namespace: opts.Namespace})
			cancel()
			if err == nil {
				return c, nil
			}
			c.Close()
			lastErr = fmt.Errorf("describe namespace %q attempt %d: %w", opts.Namespace, attempt, err)
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("temporal frontend at %s not ready within %s: %w", opts.HostPort, readyTimeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// discardLogger silences the SDK's retry chatter during startup polling.
type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}
