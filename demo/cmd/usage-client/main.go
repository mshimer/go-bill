// Command usage-client is the client side of the demo: it reports credit usage
// to the gateway for each tier, reads the remaining balance back, and then closes
// each billing period to get the invoices.
//
// It knows nothing about Temporal or Lago. It speaks HTTP to the gateway, which
// is the point - the orchestration behind the gateway is an implementation
// detail of the backend.
//
//	go run ./demo/cmd/usage-client
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// account mirrors the gateway's account payload.
type account struct {
	ID              string `json:"id"`
	Tier            string `json:"tier"`
	TierName        string `json:"tier_name"`
	MonthlyFeeCents int64  `json:"monthly_fee_cents"`
	Credits         int64  `json:"credits"`
	RateAmount      string `json:"rate_amount"`
}

// usageState mirrors the gateway's usage payload, which comes from Temporal.
type usageState struct {
	CreditAllowance  int64 `json:"credit_allowance"`
	CreditsConsumed  int64 `json:"credits_consumed"`
	CreditsRemaining int64 `json:"credits_remaining"`
	CreditsOverdrawn int64 `json:"credits_overdrawn"`
	EventsReported   int   `json:"events_reported"`
	EventsRejected   int   `json:"events_rejected"`
}

// invoice mirrors the gateway's close-billing payload.
type invoice struct {
	Number             string `json:"number"`
	Status             string `json:"status"`
	Currency           string `json:"currency"`
	UsageCents         int    `json:"usage_cents"`
	PrepaidCreditCents int    `json:"prepaid_credit_cents"`
	AmountDueCents     int    `json:"amount_due_cents"`
	TotalBilledCents   int    `json:"total_billed_cents"`
	CreditsRemaining   string `json:"credits_remaining"`
	Units              string `json:"units"`
}

// How much of its allowance each tier's customer consumes, as a fraction. The
// three cases are the point of the demo: over the allowance, exactly on it, and
// comfortably under.
var consumption = map[string]struct {
	fraction float64
	label    string
}{
	"tier-1": {1.2, "over the allowance"},
	"tier-2": {1.0, "exactly the allowance"},
	"tier-3": {0.3, "well under the allowance"},
}

func main() {
	gateway := flag.String("gateway", "http://localhost:8090", "gateway base URL")
	events := flag.Int("events", 6, "how many usage reports to send per account")
	closePeriod := flag.Bool("close", true, "close each billing period and print the invoice")
	flag.Parse()

	if err := run(*gateway, *events, *closePeriod); err != nil {
		log.Fatal(err)
	}
}

func run(gateway string, events int, closePeriod bool) error {
	httpClient := &http.Client{Timeout: 5 * time.Minute}

	var listing struct {
		Accounts []account `json:"accounts"`
	}
	if err := call(httpClient, http.MethodGet, gateway+"/v1/accounts", nil, &listing); err != nil {
		return fmt.Errorf("listing the gateway's accounts: %w", err)
	}

	for _, acct := range listing.Accounts {
		plan := consumption[acct.Tier]
		if plan.fraction == 0 {
			plan = struct {
				fraction float64
				label    string
			}{1.0, "the full allowance"}
		}

		// Split the target consumption into whole credits across the events. The
		// remainder is spread over the first few rather than dropped, so a tier
		// meant to land exactly on its allowance actually does.
		total := int64(float64(acct.Credits) * plan.fraction)
		perEvent := total / int64(events)
		remainder := total % int64(events)

		fmt.Printf("%s (%s): $%.2f/month, %d credits included\n",
			acct.TierName, acct.Tier, float64(acct.MonthlyFeeCents)/100, acct.Credits)
		fmt.Printf("  reporting %d credits - %s\n", total, plan.label)

		for i := 0; i < events; i++ {
			credits := perEvent
			if int64(i) < remainder {
				credits++
			}
			var accepted struct {
				TransactionID string `json:"transaction_id"`
			}
			body := map[string]any{"credits": credits, "session_id": acct.Tier + "-session"}
			url := gateway + "/v1/accounts/" + acct.ID + "/usage"
			if err := call(httpClient, http.MethodPost, url, body, &accepted); err != nil {
				return fmt.Errorf("reporting usage for %s: %w", acct.ID, err)
			}
		}

		// The running balance comes from Temporal's workflow state, so it settles
		// as soon as the signals are processed.
		state, err := waitForUsage(httpClient, gateway, acct.ID, events)
		if err != nil {
			return err
		}
		fmt.Printf("  gateway reports %d credits consumed, %d remaining",
			state.CreditsConsumed, state.CreditsRemaining)
		if state.CreditsOverdrawn > 0 {
			fmt.Printf(" (%d over)", state.CreditsOverdrawn)
		}
		fmt.Println()

		if !closePeriod {
			fmt.Println()
			continue
		}

		var inv invoice
		url := gateway + "/v1/accounts/" + acct.ID + "/billing/close"
		if err := call(httpClient, http.MethodPost, url, map[string]any{}, &inv); err != nil {
			return fmt.Errorf("closing the period for %s: %w", acct.ID, err)
		}
		fmt.Printf("  invoice %s (%s): %d cents of usage, %d on credits, %d to pay\n",
			inv.Number, inv.Status, inv.UsageCents, inv.PrepaidCreditCents, inv.AmountDueCents)
		fmt.Printf("  billed %d cents for the period; wallet now %s credits\n\n",
			inv.TotalBilledCents, inv.CreditsRemaining)
	}
	return nil
}

// waitForUsage polls until the gateway has seen every report.
func waitForUsage(httpClient *http.Client, gateway, accountID string, events int) (usageState, error) {
	url := gateway + "/v1/accounts/" + accountID + "/usage"
	deadline := time.Now().Add(time.Minute)
	var state usageState
	for {
		if err := call(httpClient, http.MethodGet, url, nil, &state); err != nil {
			return state, fmt.Errorf("reading usage for %s: %w", accountID, err)
		}
		if state.EventsReported+state.EventsRejected >= events || time.Now().After(deadline) {
			return state, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// call does one JSON request, treating any non-2xx as an error.
func call(httpClient *http.Client, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, bytes.TrimSpace(payload))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}
