// Command lago-seed populates a running Lago with the credit-tier billing
// configuration and walks all three tiers to their invoices.
//
// The containerized test does this against a throwaway stack. This does it
// against the long-running compose stack in lago/compose, so the web UI has
// something in it to look at: three plans, three customers each on a different
// tier with a drawn-down credit wallet, and the invoices that resulted.
//
//	cd lago/compose && docker compose up -d --wait
//	go run ./lago/cmd/lago-seed
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"comfy/go-bill/lago"
)

// Each tier gets one customer, with usage chosen to show a different outcome:
// over the allowance, exactly on it, and well under.
var scenarios = []struct {
	name     string
	tierCode string
	perEvent int64
	events   int
}{
	{name: "over-spender", tierCode: "tier-1", perEvent: 400, events: 6},
	{name: "exact-spender", tierCode: "tier-2", perEvent: 1000, events: 5},
	{name: "under-spender", tierCode: "tier-3", perEvent: 500, events: 6},
}

func main() {
	baseURL := flag.String("url", "http://localhost:3000", "Lago API base URL, without /api/v1")
	apiKey := flag.String("api-key", lago.APIKey, "Lago API key (LAGO_ORG_API_KEY)")
	flag.Parse()

	if err := run(*baseURL, *apiKey); err != nil {
		log.Fatal(err)
	}
}

func run(baseURL, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := lago.NewClientWithKey(baseURL, apiKey)
	suffix := time.Now().Format("150405")

	fmt.Printf("tier catalog:\n%s\n", lago.Ladder())

	catalog, err := lago.SeedTierPlans(ctx, client, suffix)
	if err != nil {
		return err
	}
	fmt.Printf("metric %s and %d tier plans created\n\n", catalog.MetricCode, len(catalog.Plans))

	for _, sc := range scenarios {
		tier, ok := lago.TierByCode(sc.tierCode)
		if !ok {
			return fmt.Errorf("unknown tier %q", sc.tierCode)
		}
		planCode, err := catalog.PlanCode(tier.Code)
		if err != nil {
			return err
		}

		customerID := sc.name + "-" + suffix
		subscriptionID := sc.name + "-sub-" + suffix
		used := sc.perEvent * int64(sc.events)

		if _, err := lago.CreateCustomer(ctx, client, customerID,
			"Comfy Compute ("+sc.name+")", sc.name+"@comfy.example"); err != nil {
			return err
		}
		if _, err := lago.Subscribe(ctx, client, customerID, subscriptionID, planCode); err != nil {
			return err
		}
		if _, err := lago.GrantCredits(ctx, client, customerID, tier); err != nil {
			return err
		}
		fmt.Printf("%-14s %s: $%.2f/month, %d credits granted\n",
			sc.name, tier.Code, float64(tier.MonthlyFeeCents)/100, tier.Credits)

		// The fee is charged in advance, so subscribing already billed it.
		advance, err := lago.WaitForInvoiceMatching(ctx, client, customerID,
			"advance invoice", lago.HasSubscriptionFee())
		if err != nil {
			return err
		}
		fmt.Printf("               advance invoice %s: %d cents\n", advance.Number, advance.TotalAmountCents)

		if _, err := lago.SendCredits(ctx, client, subscriptionID, catalog.MetricCode, sc.perEvent, sc.events); err != nil {
			return err
		}
		// Ingestion is asynchronous, so wait for the usage to be attributed.
		usage, err := lago.WaitForUsage(ctx, client, customerID, subscriptionID, used, sc.events)
		if err != nil {
			return err
		}
		fmt.Printf("               used %s credits over %d events, %d remaining\n",
			usage.Units, usage.EventsCount, lago.CreditsRemaining(tier, used))

		// Terminating is what forces Lago to bill the partial period.
		if _, err := lago.TerminateSubscription(ctx, client, subscriptionID); err != nil {
			return err
		}
		invoice, err := lago.WaitForInvoiceMatching(ctx, client, customerID,
			"usage invoice", lago.HasChargeFee(catalog.MetricCode))
		if err != nil {
			return err
		}
		fee, err := lago.ChargeFee(invoice, catalog.MetricCode)
		if err != nil {
			return err
		}
		fmt.Printf("               usage invoice %s: %d cents of usage, %d on credits, %d to pay\n",
			invoice.Number, fee.AmountCents, invoice.PrepaidCreditAmountCents, invoice.TotalAmountCents)

		wallet, err := lago.WaitForWalletBalance(ctx, client, customerID, lago.CreditsRemaining(tier, used))
		if err != nil {
			return err
		}
		total, err := lago.TotalInvoicedCents(ctx, client, customerID)
		if err != nil {
			return err
		}
		fmt.Printf("               wallet now %s credits; billed %d cents total (expected %d)\n\n",
			wallet.CreditsBalance, total, lago.TotalBilledCents(tier, used))
	}

	fmt.Println("Open the UI to see it: http://localhost:8088")
	return nil
}
