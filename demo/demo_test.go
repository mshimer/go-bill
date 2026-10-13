package demo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"comfy/go-bill/internal/dockertest"
	"comfy/go-bill/lago"
	devserver "comfy/go-bill/temporal"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
)

// The usage each tier's customer reports, chosen so the three outcomes differ:
// over the allowance, exactly on it, and well under.
var demoScenarios = map[string]struct {
	perEvent int64
	events   int
}{
	"tier-1": {perEvent: 400, events: 6},  // 2400 of 2000 - overage
	"tier-2": {perEvent: 1000, events: 5}, // 5000 of 5000 - exact
	"tier-3": {perEvent: 500, events: 6},  // 3000 of 10000 - under
}

// TestDemo_EndToEnd runs the whole system: a real Lago, a real Temporal, the
// worker, the HTTP gateway, and an HTTP client driving it from the outside.
//
// The client only ever speaks HTTP to the gateway. That three customers on three
// tiers end up correctly billed - one over their allowance, two inside it - is
// the point of the demo: the orchestration between the gateway, Temporal and
// Lago is invisible from outside.
func TestDemo_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the containerized end-to-end demo in -short mode")
	}
	dockertest.SkipIfUnavailable(t)

	// Five containers between the two stacks, plus Lago's migrations.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// --- the backing services ---

	start := time.Now()
	lagoStack, err := lago.StartStack(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stopCancel()
		if stopErr := lagoStack.Stop(stopCtx); stopErr != nil {
			t.Logf("stopping lago: %v", stopErr)
		}
	})
	if err != nil {
		if dockertest.IsUnavailable(err) {
			t.Skipf("Docker unavailable, skipping: %v", err)
		}
		require.NoError(t, err, "starting lago")
	}
	t.Logf("lago ready at %s after %s", lagoStack.BaseURL, time.Since(start).Round(time.Millisecond))

	temporalServer, err := devserver.StartDevServer(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
		defer stopCancel()
		if stopErr := temporalServer.Stop(stopCtx); stopErr != nil {
			t.Logf("stopping temporal: %v", stopErr)
		}
	})
	require.NoError(t, err, "starting temporal")
	t.Logf("temporal ready at %s", temporalServer.HostPort)

	// --- the billing configuration: three tiers, three customers ---

	lagoClient := lagoStack.NewClient()
	accounts, err := ProvisionAccounts(ctx, lagoClient, randomSuffix(t))
	require.NoError(t, err, "provisioning the lago accounts")
	require.Len(t, accounts, len(lago.Tiers), "one account per tier")

	// --- the backend: worker plus gateway ---

	billingWorker := worker.New(temporalServer.Client, TaskQueue, worker.Options{})
	billingWorker.RegisterWorkflow(BillingWorkflow)
	billingWorker.RegisterActivity(&Activities{Lago: lagoClient, MetricCode: accounts[0].MetricCode})
	require.NoError(t, billingWorker.Start(), "starting the billing worker")
	t.Cleanup(billingWorker.Stop)

	gateway := httptest.NewServer(NewGateway(temporalServer.Client, accounts, slog.Default()).Handler())
	t.Cleanup(gateway.Close)

	httpClient := &http.Client{Timeout: 5 * time.Minute}

	t.Run("the gateway publishes an account per tier", func(t *testing.T) {
		var listing struct {
			Accounts []Account `json:"accounts"`
		}
		callJSON(t, httpClient, http.MethodGet, gateway.URL+"/v1/accounts", nil, http.StatusOK, &listing)
		require.Len(t, listing.Accounts, len(lago.Tiers), "accounts published")

		for i, got := range listing.Accounts {
			tier, ok := lago.TierByCode(got.Tier)
			require.True(t, ok, "account %d is on an unknown tier %q", i, got.Tier)
			require.Equal(t, tier.Credits, got.Credits, "%s: credits included", got.Tier)
			require.Equal(t, tier.MonthlyFeeCents, got.MonthlyFeeCents, "%s: monthly fee", got.Tier)
		}
	})

	t.Run("an unknown account is rejected", func(t *testing.T) {
		callJSON(t, httpClient, http.MethodGet, gateway.URL+"/v1/accounts/tier-99", nil, http.StatusNotFound, nil)
	})

	// Report the usage for every account.
	transactionIDs := make(map[string][]string, len(accounts))
	for _, account := range accounts {
		sc := demoScenarios[account.Tier]
		for i := 0; i < sc.events; i++ {
			var accepted UsageAccepted
			// 202, not 200: durable in Temporal, not necessarily in Lago yet.
			callJSON(t, httpClient, http.MethodPost,
				gateway.URL+"/v1/accounts/"+account.ID+"/usage",
				UsageRequest{Credits: sc.perEvent, SessionID: account.Tier + "-session"},
				http.StatusAccepted, &accepted)
			require.NotEmpty(t, accepted.TransactionID, "%s: the gateway should mint a key", account.Tier)
			require.Equal(t, WorkflowID(account.SubscriptionID), accepted.WorkflowID, "%s: workflow id", account.Tier)
			transactionIDs[account.Tier] = append(transactionIDs[account.Tier], accepted.TransactionID)
		}
	}

	t.Run("the gateway serves the live credit balance", func(t *testing.T) {
		// Answered from Temporal's own state. It has to be: Lago's real-time
		// credit balance is premium, and its settled balance does not move until
		// an invoice is finalized.
		for _, account := range accounts {
			sc := demoScenarios[account.Tier]
			tier, _ := lago.TierByCode(account.Tier)
			used := sc.perEvent * int64(sc.events)

			state := waitForGatewayUsage(t, httpClient, gateway.URL, account.ID, sc.events)
			require.Equal(t, used, state.CreditsConsumed, "%s: credits consumed", account.Tier)
			require.Equal(t, lago.CreditsRemaining(tier, used), state.CreditsRemaining,
				"%s: credits remaining", account.Tier)
			require.Zero(t, state.EventsRejected, "%s: nothing should be rejected", account.Tier)

			t.Logf("%s: %d of %d credits consumed, %d remaining",
				account.Tier, state.CreditsConsumed, tier.Credits, state.CreditsRemaining)
		}
	})

	t.Run("usage reached lago", func(t *testing.T) {
		// Looking past the gateway, to confirm the running total is not just the
		// workflow believing its own bookkeeping.
		for _, account := range accounts {
			sc := demoScenarios[account.Tier]
			tier, _ := lago.TierByCode(account.Tier)
			used := sc.perEvent * int64(sc.events)

			usage, err := lago.WaitForUsage(ctx, lagoClient,
				account.CustomerID, account.SubscriptionID, used, sc.events)
			require.NoError(t, err, "%s: waiting for lago to aggregate", account.Tier)
			require.Equal(t, int(lago.UsageChargeCents(tier, used)), usage.AmountCents,
				"%s: %d credits at %s", account.Tier, used, tier.RateAmount)
		}
	})

	t.Run("a replayed report does not spend credits twice", func(t *testing.T) {
		// What an at-least-once client does. The transaction id makes it safe,
		// and the gateway accepts it rather than erroring.
		account := accounts[0]
		sc := demoScenarios[account.Tier]
		used := sc.perEvent * int64(sc.events)

		var accepted UsageAccepted
		callJSON(t, httpClient, http.MethodPost,
			gateway.URL+"/v1/accounts/"+account.ID+"/usage",
			UsageRequest{
				Credits:       sc.perEvent,
				SessionID:     account.Tier + "-session",
				TransactionID: transactionIDs[account.Tier][0], // already delivered
			},
			http.StatusAccepted, &accepted)
		require.Equal(t, transactionIDs[account.Tier][0], accepted.TransactionID,
			"the supplied key should be honoured")

		// The workflow counts the signal, but Lago must not count the usage.
		state := waitForGatewayUsage(t, httpClient, gateway.URL, account.ID, sc.events+1)
		require.Equal(t, sc.events+1, state.EventsReported, "the replay was accepted")

		usage, err := lago.WaitForUsage(ctx, lagoClient,
			account.CustomerID, account.SubscriptionID, used, sc.events)
		require.NoError(t, err, "lago should still hold exactly the original usage")
		require.Equal(t, sc.events, usage.EventsCount, "lago must not double-count a replay")
	})

	t.Run("closing a period bills the fee plus any overage", func(t *testing.T) {
		for _, account := range accounts {
			sc := demoScenarios[account.Tier]
			tier, _ := lago.TierByCode(account.Tier)
			used := sc.perEvent * int64(sc.events)

			var invoice InvoiceSummary
			callJSON(t, httpClient, http.MethodPost,
				gateway.URL+"/v1/accounts/"+account.ID+"/billing/close",
				map[string]any{}, http.StatusOK, &invoice)

			require.Equal(t, int(lago.UsageChargeCents(tier, used)), invoice.UsageCents,
				"%s: usage before credits", account.Tier)
			require.Equal(t, int(lago.PrepaidCreditCents(tier, used)), invoice.PrepaidCreditCents,
				"%s: credits applied", account.Tier)
			require.Equal(t, int(lago.OverageCents(tier, used)), invoice.AmountDueCents,
				"%s: only overage should be left to pay", account.Tier)
			require.Equal(t, int(lago.TotalBilledCents(tier, used)), invoice.TotalBilledCents,
				"%s: fee plus overage for the period", account.Tier)
			require.NotEmpty(t, invoice.Number, "%s: invoice number", account.Tier)

			t.Logf("%s: invoice %s - %d cents of usage, %d on credits, %d to pay; %d billed for the period",
				account.Tier, invoice.Number, invoice.UsageCents,
				invoice.PrepaidCreditCents, invoice.AmountDueCents, invoice.TotalBilledCents)
		}
	})
}

// waitForGatewayUsage polls the gateway until it reports at least wantEvents.
// The gateway answers from workflow state, which settles once Temporal has
// processed the signals.
func waitForGatewayUsage(t *testing.T, httpClient *http.Client, baseURL, accountID string, wantEvents int) UsageState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var state UsageState
	for {
		callJSON(t, httpClient, http.MethodGet,
			baseURL+"/v1/accounts/"+accountID+"/usage", nil, http.StatusOK, &state)
		if state.EventsReported+state.EventsRejected >= wantEvents {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: gateway reported %d of %d events before the deadline",
				accountID, state.EventsReported+state.EventsRejected, wantEvents)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// callJSON makes one JSON request against the gateway and insists on a status.
func callJSON(t *testing.T, httpClient *http.Client, method, url string, body any, wantStatus int, out any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err, "encoding request body")
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err, "building the request")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	require.NoError(t, err, "%s %s", method, url)
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading the response body")
	require.Equal(t, wantStatus, resp.StatusCode, "%s %s returned: %s", method, url, payload)

	if out != nil {
		require.NoError(t, json.Unmarshal(payload, out), "decoding %s", payload)
	}
}

// randomSuffix keeps Lago codes unique across runs. Hex rather than a timestamp:
// Lago codes are picky about punctuation, and two runs in the same second would
// otherwise collide on a uniqueness constraint.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [4]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err, "reading random bytes")
	return hex.EncodeToString(b[:])
}
