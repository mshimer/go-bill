// Command gateway runs the demo backend: the HTTP usage gateway plus the
// Temporal worker that drives billing.
//
// One process runs both for the sake of a short demo. They are separate
// concerns and would be separate deployments in earnest - the gateway scales
// with client traffic, the worker with billing work.
//
//	# Lago (see lago/compose) and a Temporal dev server must be running:
//	cd lago/compose && docker compose up -d --wait
//	temporal server start-dev
//
//	go run ./demo/cmd/gateway
//	go run ./demo/cmd/usage-client
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"comfy/go-bill/demo"
	"comfy/go-bill/lago"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	addr := flag.String("addr", ":8090", "address for the HTTP gateway to listen on")
	temporalAddr := flag.String("temporal", client.DefaultHostPort, "Temporal frontend host:port")
	lagoURL := flag.String("lago-url", "http://localhost:3000", "Lago API base URL, without /api/v1")
	lagoKey := flag.String("lago-key", lago.APIKey, "Lago API key (LAGO_ORG_API_KEY)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*addr, *temporalAddr, *lagoURL, *lagoKey, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(addr, temporalAddr, lagoURL, lagoKey string, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	temporalClient, err := client.DialContext(ctx, client.Options{HostPort: temporalAddr})
	if err != nil {
		return fmt.Errorf("dialing temporal at %s: %w", temporalAddr, err)
	}
	defer temporalClient.Close()
	logger.Info("connected to temporal", "host_port", temporalAddr)

	lagoClient := lago.NewClientWithKey(lagoURL, lagoKey)

	// Provision a fresh account each run, so repeated demos do not collide on
	// Lago's uniqueness constraints.
	provisionCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	accounts, err := demo.ProvisionAccounts(provisionCtx, lagoClient, time.Now().Format("150405"))
	if err != nil {
		return fmt.Errorf("provisioning lago: %w", err)
	}
	for _, account := range accounts {
		logger.Info("provisioned lago account",
			"account", account.ID,
			"plan", account.PlanCode,
			"customer", account.CustomerID,
			"credits", account.Credits)
	}
	fmt.Printf("\ntier catalog:\n%s\n", lago.Ladder())

	// All three accounts share one billable metric, so one worker serves them
	// all: a metric carries no price, and the tier ladder lives in the plans.
	w := worker.New(temporalClient, demo.TaskQueue, worker.Options{})
	w.RegisterWorkflow(demo.BillingWorkflow)
	w.RegisterActivity(&demo.Activities{Lago: lagoClient, MetricCode: accounts[0].MetricCode})
	if err := w.Start(); err != nil {
		return fmt.Errorf("starting the temporal worker: %w", err)
	}
	defer w.Stop()
	logger.Info("temporal worker started", "task_queue", demo.TaskQueue)

	server := &http.Server{
		Addr:              addr,
		Handler:           demo.NewGateway(temporalClient, accounts, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return server.Shutdown(shutdownCtx)
	}
}
