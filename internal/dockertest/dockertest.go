// Package dockertest holds the guards that let container-backed tests skip
// cleanly on a machine without a working Docker daemon.
//
// It is a plain package rather than a _test.go file so that several test
// packages can share it: the same three-layer guard was otherwise copied into
// every package that starts a container.
package dockertest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// RequireEnv is the environment variable that turns a skip into a failure.
// Set it in CI, where an absent Docker daemon is a broken runner rather than a
// reason to quietly pass.
const RequireEnv = "GO_BILL_REQUIRE_DOCKER"

// SkipIfUnavailable probes the Docker daemon and skips the test when it cannot
// be reached, so a missing daemon produces a clear skip rather than a container
// start failure deep inside a test.
func SkipIfUnavailable(t testing.TB) {
	t.Helper()
	if os.Getenv(RequireEnv) != "" {
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

// IsUnavailable reports whether err is the Docker daemon being absent rather
// than a genuine failure to start a container.
//
// The probe in SkipIfUnavailable catches the common case up front; this catches
// a daemon that disappears between the probe and the container start.
func IsUnavailable(err error) bool {
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
