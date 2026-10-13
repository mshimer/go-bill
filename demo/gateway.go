package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// Gateway is the HTTP front door clients report usage to.
//
// It is intentionally thin. It does not talk to Lago, does not price anything
// and keeps no state of its own beyond the accounts it was provisioned with: it
// validates a request, hands it to Temporal and answers. That is the whole point
// of the split - the gateway's availability is decoupled from the billing
// system's, because the durability boundary is Temporal rather than Lago.
type Gateway struct {
	temporal client.Client
	accounts map[string]*Account
	order    []string // accounts in catalog order, for a stable listing
	logger   *slog.Logger

	// billingPeriod is passed to each workflow it starts. Zero means a period
	// ends only when something closes it.
	billingPeriod time.Duration

	// closeTimeout bounds how long closing a period waits for the invoice.
	closeTimeout time.Duration
}

// NewGateway builds a gateway serving the given accounts, keyed by Account.ID.
func NewGateway(temporalClient client.Client, accounts []*Account, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	g := &Gateway{
		temporal:      temporalClient,
		accounts:      make(map[string]*Account, len(accounts)),
		order:         make([]string, 0, len(accounts)),
		logger:        logger,
		billingPeriod: 0,
		closeTimeout:  3 * time.Minute,
	}
	for _, account := range accounts {
		g.accounts[account.ID] = account
		g.order = append(g.order, account.ID)
	}
	return g
}

// Handler is the gateway's routes.
//
//	GET  /healthz                            liveness
//	GET  /v1/accounts                        every provisioned account and its tier
//	GET  /v1/accounts/{id}                   one account
//	POST /v1/accounts/{id}/usage             report usage    -> 202, Temporal has it
//	GET  /v1/accounts/{id}/usage             credits consumed and remaining
//	POST /v1/accounts/{id}/billing/close     close the period -> 200 with the invoice
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/accounts", g.handleListAccounts)
	mux.HandleFunc("GET /v1/accounts/{id}", g.handleGetAccount)
	mux.HandleFunc("POST /v1/accounts/{id}/usage", g.handleReportUsage)
	mux.HandleFunc("GET /v1/accounts/{id}/usage", g.handleGetUsage)
	mux.HandleFunc("POST /v1/accounts/{id}/billing/close", g.handleCloseBilling)
	return mux
}

// UsageRequest is the body clients POST to report usage.
type UsageRequest struct {
	// Credits is how many credits this piece of usage consumes.
	Credits   int64  `json:"credits"`
	SessionID string `json:"session_id,omitempty"`

	// TransactionID is optional. Supply one to make a client-side retry
	// idempotent end to end; leave it out and the gateway mints one.
	TransactionID string `json:"transaction_id,omitempty"`
}

// UsageAccepted is the gateway's answer to accepted usage.
//
// It is a 202 rather than a 200, and the distinction is real: the usage is
// durably recorded in Temporal and guaranteed to reach Lago, but it has not
// necessarily reached Lago yet.
type UsageAccepted struct {
	TransactionID string `json:"transaction_id"`
	WorkflowID    string `json:"workflow_id"`
	RunID         string `json:"run_id"`
}

// account resolves the {id} path parameter, writing a 404 if it is unknown.
func (g *Gateway) account(w http.ResponseWriter, r *http.Request) (*Account, bool) {
	id := r.PathValue("id")
	account, ok := g.accounts[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such account %q", id))
		return nil, false
	}
	return account, true
}

func (g *Gateway) handleListAccounts(w http.ResponseWriter, _ *http.Request) {
	accounts := make([]*Account, 0, len(g.order))
	for _, id := range g.order {
		accounts = append(accounts, g.accounts[id])
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (g *Gateway) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := g.account(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (g *Gateway) handleReportUsage(w http.ResponseWriter, r *http.Request) {
	account, ok := g.account(w, r)
	if !ok {
		return
	}

	var req UsageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decoding request body: %w", err))
		return
	}
	if req.Credits <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("credits must be greater than zero"))
		return
	}

	report := UsageReport{
		TransactionID:  req.TransactionID,
		SubscriptionID: account.SubscriptionID,
		SessionID:      req.SessionID,
		Credits:        req.Credits,
	}
	if report.TransactionID == "" {
		// Minted here, once, and then stable across every retry inside Temporal.
		// This is the key that stops at-least-once delivery becoming
		// at-least-once billing.
		report.TransactionID = uuid.NewString()
	}

	// Signal-with-start: the gateway neither knows nor cares whether a billing
	// period is already open for this subscription. Temporal either delivers the
	// signal to the running workflow or starts one and then delivers it.
	run, err := g.temporal.SignalWithStartWorkflow(
		r.Context(),
		WorkflowID(account.SubscriptionID),
		ReportUsageSignal,
		report,
		client.StartWorkflowOptions{
			ID:        WorkflowID(account.SubscriptionID),
			TaskQueue: TaskQueue,
		},
		BillingWorkflow,
		g.workflowInput(account),
	)
	if err != nil {
		// Temporal being unreachable is the one failure the gateway cannot
		// absorb, because it is where the durability guarantee comes from.
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("accepting usage: %w", err))
		return
	}

	g.logger.Info("usage accepted",
		"account", account.ID,
		"transaction_id", report.TransactionID,
		"credits", report.Credits,
		"workflow_id", run.GetID())

	writeJSON(w, http.StatusAccepted, UsageAccepted{
		TransactionID: report.TransactionID,
		WorkflowID:    run.GetID(),
		RunID:         run.GetRunID(),
	})
}

func (g *Gateway) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	account, ok := g.account(w, r)
	if !ok {
		return
	}

	// Answered entirely from Temporal's own durable state - no Lago call, and no
	// database of the gateway's own. It has to be: Lago's real-time credit
	// balance is a premium feature, and its settled balance does not move until
	// an invoice is finalized, so Temporal is the only live source of
	// "credits remaining" here.
	response, err := g.temporal.QueryWorkflow(r.Context(),
		WorkflowID(account.SubscriptionID), "", UsageQuery)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("querying usage: %w", err))
		return
	}
	var state UsageState
	if err := response.Get(&state); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("decoding usage state: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (g *Gateway) handleCloseBilling(w http.ResponseWriter, r *http.Request) {
	account, ok := g.account(w, r)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(r, g.closeTimeout)
	defer cancel()

	run, err := g.temporal.SignalWithStartWorkflow(
		ctx,
		WorkflowID(account.SubscriptionID),
		CloseBillingSignal,
		nil,
		client.StartWorkflowOptions{
			ID:        WorkflowID(account.SubscriptionID),
			TaskQueue: TaskQueue,
		},
		BillingWorkflow,
		g.workflowInput(account),
	)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("closing the billing period: %w", err))
		return
	}

	// This endpoint blocks, unlike reporting usage: the caller asked for an
	// invoice, so waiting for the workflow to finish producing one is the useful
	// behaviour. Lago builds it asynchronously, so this covers that wait too.
	var summary InvoiceSummary
	if err := run.Get(ctx, &summary); err != nil {
		writeError(w, http.StatusGatewayTimeout, fmt.Errorf("waiting for the invoice: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// workflowInput is the start argument for an account's billing period. It is
// only used when no period is open yet - signal-with-start ignores it otherwise.
func (g *Gateway) workflowInput(account *Account) BillingWorkflowInput {
	return BillingWorkflowInput{
		SubscriptionID:  account.SubscriptionID,
		CustomerID:      account.CustomerID,
		Tier:            account.Tier,
		CreditAllowance: account.Credits,
		BillingPeriod:   g.billingPeriod,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already written, so there is nothing to do but note it.
		slog.Default().Error("writing json response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
