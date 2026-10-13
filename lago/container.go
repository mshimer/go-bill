package lago

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// APIImage is Lago's Rails API. Its default CMD is scripts/start.sh, which
	// runs db:migrate, seeds the predefined roles, seeds our organization and
	// only then execs Puma - all under set -e. We deliberately do not override
	// the command, which is what lets one container both migrate and serve and
	// makes a 200 from /health imply the organization already exists.
	//
	// Pinned to v1.55.0 and no lower: v1.54.0 shipped an inherited ENTRYPOINT
	// that demoted `command` to arguments, which silently breaks the worker
	// container below, and had no curl on PATH for healthchecks.
	APIImage = "getlago/api:v1.55.0"

	// PostgresImage is Lago's database, and it has to be this one rather than a
	// stock postgres.
	//
	// Lago needs the pg_partman extension. Its partman *migrations* do check
	// whether the extension exists and skip when it does not, which makes plain
	// Postgres look workable - but a fresh database is not built by replaying
	// migrations. db:migrate loads db/structure.sql through psql, and that file
	// contains an unguarded CREATE EXTENSION pg_partman, so on stock postgres:17
	// the API container dies during its first boot with:
	//
	//	psql:/app/db/structure.sql: ERROR: extension "pg_partman" is not available
	//
	// Upstream publishes this image for exactly that reason. It is Postgres 15;
	// no partman-carrying 16 or 17 tag is published.
	PostgresImage = "getlago/postgres-partman:15.0-alpine"

	// RedisImage backs both Sidekiq's queues and the Rails cache.
	RedisImage = "redis:7-alpine"

	// APIPort is the Rails API's port inside the container.
	APIPort = "3000/tcp"

	// workerPort is Sidekiq's embedded health port. It is not published by
	// upstream's compose, but exposing it gives the worker a real readiness
	// signal instead of a sleep.
	workerPort = "8080/tcp"

	// The seeded organization. All four of these are mandatory together: with
	// LAGO_CREATE_ORG=true and any of the others missing, signup.rake raises and
	// the container exits.
	orgName     = "go-bill"
	orgEmail    = "go-bill@example.local"
	orgPassword = "Go-Bill-Local-1!"

	// APIKey is the organization's API key, fixed rather than generated.
	// signup.rake overwrites ApiKey's generated UUID with this exact value, and
	// the model validates only presence and uniqueness, so a test can hold the
	// bearer token before the server has even booted.
	APIKey = "go-bill-local-api-key"

	// startupTimeout bounds a cold image pull plus Rails boot plus the full
	// migration run. Upstream allows 900s for migrations alone on an empty
	// database, so this is not as generous as it looks.
	startupTimeout = 5 * time.Minute

	// readyTimeout bounds the post-start poll that proves the seeded
	// organization and its API key are live.
	readyTimeout = 2 * time.Minute
)

// Stack is a running self-hosted Lago: a private network carrying Postgres,
// Redis, the API and a Sidekiq worker.
//
// The worker is not optional. Lago builds invoices in background jobs, and the
// REST layer gives no way to run them inline - Subscriptions::TerminateService
// is called without an async argument, so it defaults to async and enqueues
// BillSubscriptionJob. With no worker, events are still ingested and usage still
// aggregates, but no invoice is ever produced.
type Stack struct {
	Network  *testcontainers.DockerNetwork
	Postgres testcontainers.Container
	Redis    testcontainers.Container
	API      testcontainers.Container
	Worker   testcontainers.Container

	// BaseURL is the API's address from the host, e.g. http://localhost:54321.
	// It carries no /api/v1 suffix: the Lago Go client appends that itself.
	BaseURL string
}

// StartStack boots Lago and returns once the API is serving and the seeded
// organization's API key works.
//
// Readiness is two-phase, as in temporal/container.go. The testcontainers wait
// strategies only establish that each process is up; GET /health additionally
// proves the database round-trips. Phase two then polls an authenticated
// endpoint, because a serving API is not the same thing as a seeded one.
//
// On failure the partially built Stack is still returned so the caller's cleanup
// can reap whatever did start.
func StartStack(ctx context.Context) (*Stack, error) {
	stack := &Stack{}

	net, err := network.New(ctx)
	if err != nil {
		return stack, fmt.Errorf("creating docker network: %w", err)
	}
	stack.Network = net
	netName := net.Name

	// Generated per stack rather than hard-coded: Lago signs webhooks with it,
	// has no default for it, and generating it here avoids shelling out to
	// openssl the way upstream's scripts do.
	rsaKey, err := generateRSAPrivateKeyBase64()
	if err != nil {
		return stack, fmt.Errorf("generating lago rsa private key: %w", err)
	}

	if stack.Postgres, err = startPostgres(ctx, netName); err != nil {
		return stack, err
	}
	if stack.Redis, err = startRedis(ctx, netName); err != nil {
		return stack, err
	}

	env := backendEnv(rsaKey)
	if stack.API, err = startAPI(ctx, netName, env); err != nil {
		return stack, err
	}
	if stack.Worker, err = startWorker(ctx, netName, env); err != nil {
		return stack, err
	}

	host, err := stack.API.Host(ctx)
	if err != nil {
		return stack, fmt.Errorf("resolving container host: %w", err)
	}
	mapped, err := stack.API.MappedPort(ctx, APIPort)
	if err != nil {
		return stack, fmt.Errorf("resolving mapped api port: %w", err)
	}
	stack.BaseURL = fmt.Sprintf("http://%s:%s", host, mapped.Port())

	// Phase two: /health answering does not prove signup.rake has run.
	if err := waitForSeededOrg(ctx, stack.BaseURL); err != nil {
		return stack, err
	}
	return stack, nil
}

// Stop tears the stack down, newest first, and removes the network last so no
// container is still attached to it. Safe to call on a partially built Stack.
func (s *Stack) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, c := range []testcontainers.Container{s.Worker, s.API, s.Redis, s.Postgres} {
		if c == nil {
			continue
		}
		if err := testcontainers.TerminateContainer(c); err != nil {
			errs = append(errs, err)
		}
	}
	if s.Network != nil {
		if err := s.Network.Remove(ctx); err != nil {
			errs = append(errs, fmt.Errorf("removing docker network: %w", err))
		}
	}
	return errors.Join(errs...)
}

// backendEnv is the environment shared by the API and the worker. They must agree
// on it: the worker builds invoices with the same encryption keys and database the
// API wrote the events with.
//
// Note what is absent. LAGO_CLICKHOUSE_ENABLED and LAGO_DEFAULT_EVENT_STORE are
// unset, so events land in Postgres and aggregate from the raw events table.
// The Kafka variables are unset too, which makes Lago's Kafka producer a no-op.
// Leaving them out is how those subsystems are disabled - there is no flag to set.
func backendEnv(rsaPrivateKeyBase64 string) map[string]string {
	redisURL := "redis://redis:6379"
	return map[string]string{
		"DATABASE_URL":         "postgresql://lago:lago@postgres:5432/lago?search_path=public",
		"REDIS_URL":            redisURL,
		"LAGO_REDIS_CACHE_URL": redisURL,

		"RAILS_ENV":           "production",
		"RAILS_LOG_TO_STDOUT": "true",

		// Lago enforces no format on these; its own CI passes literals.
		"SECRET_KEY_BASE":                     "go-bill-local-secret-key-base",
		"LAGO_ENCRYPTION_PRIMARY_KEY":         "go-bill-local-encryption-primary-key",
		"LAGO_ENCRYPTION_DETERMINISTIC_KEY":   "go-bill-local-encryption-deterministic-key",
		"LAGO_ENCRYPTION_KEY_DERIVATION_SALT": "go-bill-local-encryption-derivation-salt",
		"LAGO_RSA_PRIVATE_KEY":                rsaPrivateKeyBase64,

		"LAGO_API_URL":   "http://localhost:3000",
		"LAGO_FRONT_URL": "http://localhost",

		// No gotenberg container, so PDF rendering has to be off or invoice
		// finalization will try to reach a service that is not there.
		"LAGO_DISABLE_PDF_GENERATION": "true",
		"LAGO_DISABLE_SEGMENT":        "true",
		"LAGO_SIDEKIQ_WEB":            "false",

		"LAGO_CREATE_ORG":        "true",
		"LAGO_ORG_NAME":          orgName,
		"LAGO_ORG_USER_EMAIL":    orgEmail,
		"LAGO_ORG_USER_PASSWORD": orgPassword,
		"LAGO_ORG_API_KEY":       APIKey,
	}
}

func startPostgres(ctx context.Context, netName string) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:          PostgresImage,
		Networks:       []string{netName},
		NetworkAliases: map[string][]string{netName: {"postgres"}},
		Env: map[string]string{
			// POSTGRES_USER becomes a superuser in the official image, which
			// Lago needs: its migrations CREATE EXTENSION.
			"POSTGRES_USER":     "lago",
			"POSTGRES_PASSWORD": "lago",
			// Lago's start.sh migrates but never creates the database, so it has
			// to exist before the API boots.
			"POSTGRES_DB": "lago",
		},
		WaitingFor: wait.ForAll(
			// Twice: the entrypoint starts Postgres once to run its init scripts
			// and logs this line then too, before the real listener is up.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute),
		).WithStartupTimeoutDefault(2 * time.Minute),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return ctr, fmt.Errorf("starting %s: %w", PostgresImage, err)
	}
	return ctr, nil
}

func startRedis(ctx context.Context, netName string) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:          RedisImage,
		Networks:       []string{netName},
		NetworkAliases: map[string][]string{netName: {"redis"}},
		WaitingFor: wait.ForAll(
			wait.ForLog("Ready to accept connections").WithStartupTimeout(time.Minute),
			wait.ForListeningPort("6379/tcp").WithStartupTimeout(time.Minute),
		).WithStartupTimeoutDefault(time.Minute),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return ctr, fmt.Errorf("starting %s: %w", RedisImage, err)
	}
	return ctr, nil
}

func startAPI(ctx context.Context, netName string, env map[string]string) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:          APIImage,
		Networks:       []string{netName},
		NetworkAliases: map[string][]string{netName: {"api"}},
		Env:            env,
		ExposedPorts:   []string{APIPort},
		// No Cmd on purpose. See APIImage: the default command migrates and seeds.
		WaitingFor: wait.ForHTTP("/health").
			WithPort(APIPort).
			// /health does a real database round-trip, so a 200 covers Postgres
			// as well as Rails.
			WithStatusCodeMatcher(func(status int) bool { return status == 200 }).
			WithPollInterval(2 * time.Second).
			WithStartupTimeout(startupTimeout),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return ctr, fmt.Errorf("starting lago api (%s): %w", APIImage, err)
	}
	return ctr, nil
}

func startWorker(ctx context.Context, netName string, env map[string]string) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:          APIImage,
		Networks:       []string{netName},
		NetworkAliases: map[string][]string{netName: {"worker"}},
		Env:            env,
		ExposedPorts:   []string{workerPort},
		// Overriding the command is what makes this the worker rather than a
		// second API. start.worker.sh skips migrations and runs Sidekiq against
		// every default queue, which is where invoice jobs land.
		Cmd: []string{"./scripts/start.worker.sh"},
		WaitingFor: wait.ForListeningPort(workerPort).
			WithStartupTimeout(startupTimeout),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return ctr, fmt.Errorf("starting lago worker (%s): %w", APIImage, err)
	}
	return ctr, nil
}

// generateRSAPrivateKeyBase64 produces LAGO_RSA_PRIVATE_KEY: a PEM-encoded
// PKCS#1 RSA private key, base64'd on a single line. It is the Go equivalent of
// upstream's `openssl genrsa 2048 | openssl base64 -A`, done in-process so the
// stack has no dependency on an openssl binary.
func generateRSAPrivateKeyBase64() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generating rsa key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return base64.StdEncoding.EncodeToString(pemBytes), nil
}
