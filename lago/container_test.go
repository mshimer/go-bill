package lago

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	lagoapi "github.com/getlago/lago-go-client"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

// The three customers the test bills, one per tier, chosen so each demonstrates
// a different outcome: one goes over their allowance, one lands exactly on it,
// and one stays well under.
var scenarios = []struct {
	name     string
	tierCode string
	perEvent int64
	events   int
}{
	{name: "over-spender", tierCode: "tier-1", perEvent: 400, events: 6},   // 2400 of 2000
	{name: "exact-spender", tierCode: "tier-2", perEvent: 1000, events: 5}, // 5000 of 5000
	{name: "under-spender", tierCode: "tier-3", perEvent: 500, events: 6},  // 3000 of 10000
}

// account is one provisioned customer under test.
type account struct {
	name           string
	tier           Tier
	customerID     string
	subscriptionID string
	perEvent       int64
	events         int
	creditsUsed    int64
	transactionIDs []string
}

// TestCreditTiers_AgainstContainers drives the whole credit-tier model on a real
// self-hosted Lago: three plans, three customers on three tiers, each with a
// granted credit allowance, usage events drawing it down, and the invoices that
// result.
//
// Gating matches temporal/container_test.go: this runs by default and skips
// under -short, so `go test -short ./lago/...` exercises only the tier
// arithmetic while the default run uses Docker.
func TestCreditTiers_AgainstContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping containerized Lago test in -short mode")
	}
	skipIfNoDocker(t)

	// Generous: four cold image pulls, a full Rails migration run, and the
	// Sidekiq round trips that follow.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	start := time.Now()
	stack, err := StartStack(ctx)
	t.Cleanup(func() {
		// A fresh context: the test's own may already be cancelled, and the
		// network cannot be removed while its containers are still attached.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stopCancel()
		if stopErr := stack.Stop(stopCtx); stopErr != nil {
			t.Logf("stopping lago stack: %v", stopErr)
		}
	})
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker unavailable, skipping: %v", err)
		}
		require.NoError(t, err, "starting the lago stack")
	}
	t.Logf("lago ready at %s after %s", stack.BaseURL, time.Since(start).Round(time.Millisecond))
	t.Logf("tier catalog:\n%s", Ladder())

	client := stack.NewClient()
	suffix := randomSuffix()

	catalog, err := SeedTierPlans(ctx, client, suffix)
	require.NoError(t, err, "seeding the tier plans")
	require.Len(t, catalog.Plans, len(Tiers), "one plan per tier")

	// Provision one customer per tier.
	accounts := make([]*account, 0, len(scenarios))
	for _, sc := range scenarios {
		tier, ok := TierByCode(sc.tierCode)
		require.True(t, ok, "tier %q should be in the catalog", sc.tierCode)

		planCode, err := catalog.PlanCode(tier.Code)
		require.NoError(t, err, "plan code for tier %q", tier.Code)

		acct := &account{
			name:           sc.name,
			tier:           tier,
			customerID:     sc.name + "-" + suffix,
			subscriptionID: sc.name + "-sub-" + suffix,
			perEvent:       sc.perEvent,
			events:         sc.events,
			creditsUsed:    sc.perEvent * int64(sc.events),
		}

		_, err = CreateCustomer(ctx, client, acct.customerID,
			"Comfy Compute ("+sc.name+")", sc.name+"@comfy.example")
		require.NoError(t, err, "creating customer %s", acct.customerID)

		_, err = Subscribe(ctx, client, acct.customerID, acct.subscriptionID, planCode)
		require.NoError(t, err, "subscribing %s to %s", acct.customerID, planCode)

		_, err = GrantCredits(ctx, client, acct.customerID, tier)
		require.NoError(t, err, "granting credits to %s", acct.customerID)

		accounts = append(accounts, acct)
	}

	t.Run("each wallet is granted its tier allowance", func(t *testing.T) {
		for _, acct := range accounts {
			// Polled, not read once: a wallet's granted credits arrive as a
			// wallet transaction that Sidekiq settles, so a wallet created a
			// moment ago still reports a zero balance.
			wallet, err := WaitForWalletBalance(ctx, client, acct.customerID, acct.tier.Credits)
			require.NoError(t, err, "%s: waiting for the granted credits", acct.name)

			require.Equal(t, lagoapi.Active, wallet.Status, "%s: wallet status", acct.name)
			// The rate has to match the charge price, or a credit of usage would
			// not consume a credit of allowance.
			require.Equal(t, acct.tier.MonthlyFeeCents, int64(wallet.BalanceCents),
				"%s: the allowance should be worth the monthly fee", acct.name)
			// Scoped to charge fees, so credits never pay the subscription fee.
			require.Equal(t, []string{"charge"}, wallet.AppliesTo.FeeTypes,
				"%s: wallet should apply to charge fees only", acct.name)
		}
	})

	t.Run("the tier fee is invoiced in advance", func(t *testing.T) {
		// The plan is pay-in-advance, so subscribing bills the whole monthly fee
		// immediately rather than prorating it at the end of the period.
		for _, acct := range accounts {
			invoice, err := WaitForInvoiceMatching(ctx, client, acct.customerID,
				acct.name+" advance invoice", HasSubscriptionFee())
			require.NoError(t, err, "%s: waiting for the advance invoice", acct.name)

			fee, err := SubscriptionFee(invoice)
			require.NoError(t, err, "%s: finding the subscription fee", acct.name)
			require.Equal(t, int(acct.tier.MonthlyFeeCents), fee.AmountCents,
				"%s: %s monthly fee", acct.name, acct.tier.Code)
			require.Equal(t, int(acct.tier.MonthlyFeeCents), invoice.TotalAmountCents,
				"%s: the advance invoice should be the fee alone", acct.name)
			require.Zero(t, invoice.PrepaidCreditAmountCents,
				"%s: credits must not pay the subscription fee", acct.name)

			t.Logf("%s (%s): advance invoice %s for %d cents",
				acct.name, acct.tier.Code, invoice.Number, invoice.TotalAmountCents)
		}
	})

	// Report the usage.
	for _, acct := range accounts {
		ids, err := SendCredits(ctx, client, acct.subscriptionID, catalog.MetricCode, acct.perEvent, acct.events)
		require.NoError(t, err, "%s: reporting usage", acct.name)
		require.Len(t, ids, acct.events, "%s: one transaction id per event", acct.name)
		acct.transactionIDs = ids
	}

	t.Run("usage draws against the allowance", func(t *testing.T) {
		for _, acct := range accounts {
			usage, err := WaitForUsage(ctx, client, acct.customerID, acct.subscriptionID,
				acct.creditsUsed, acct.events)
			require.NoError(t, err, "%s: waiting for usage to aggregate", acct.name)

			require.Equal(t, acct.events, usage.EventsCount, "%s: events counted", acct.name)
			require.Equal(t, catalog.MetricCode, usage.BillableMetric.Code,
				"%s: metric behind the usage", acct.name)
			require.Equal(t, int(UsageChargeCents(acct.tier, acct.creditsUsed)), usage.AmountCents,
				"%s: %d credits at %s", acct.name, acct.creditsUsed, acct.tier.RateAmount)

			t.Logf("%s (%s): used %s of %d credits, %d credits remaining",
				acct.name, acct.tier.Code, usage.Units, acct.tier.Credits,
				CreditsRemaining(acct.tier, acct.creditsUsed))
		}
	})

	t.Run("a duplicate transaction id is rejected", func(t *testing.T) {
		// Replaying an event is how an at-least-once producer behaves, and the
		// transaction id is what makes that safe: Lago rejects the duplicate
		// rather than double-spending the customer's credits.
		acct := accounts[0]
		_, lerr := client.Event().Create(ctx, &lagoapi.EventInput{
			TransactionID:          acct.transactionIDs[0],
			ExternalSubscriptionID: acct.subscriptionID,
			Code:                   catalog.MetricCode,
			Properties:             map[string]any{CreditsField: acct.perEvent},
		})
		require.NotNil(t, lerr, "replaying a transaction id should be rejected")
		require.Equal(t, 422, lerr.HTTPStatusCode, "duplicate event status: %s", lerr.Error())
		require.Contains(t, lerr.Error(), string(lagoapi.ErrorCodeAlreadyExist),
			"duplicate event should report value_already_exist")
	})

	// Close every billing period.
	for _, acct := range accounts {
		terminated, err := TerminateSubscription(ctx, client, acct.subscriptionID)
		require.NoError(t, err, "%s: terminating the subscription", acct.name)
		require.Equal(t, "terminated", string(terminated.Status), "%s: status", acct.name)
	}

	t.Run("credits absorb the usage and only overage is billed", func(t *testing.T) {
		for _, acct := range accounts {
			invoice, err := WaitForInvoiceMatching(ctx, client, acct.customerID,
				acct.name+" usage invoice", HasChargeFee(catalog.MetricCode))
			require.NoError(t, err, "%s: waiting for the usage invoice", acct.name)

			fee, err := ChargeFee(invoice, catalog.MetricCode)
			require.NoError(t, err, "%s: finding the usage fee", acct.name)

			require.Equal(t, int(UsageChargeCents(acct.tier, acct.creditsUsed)), fee.AmountCents,
				"%s: usage charged before credits", acct.name)
			require.Equal(t, int(PrepaidCreditCents(acct.tier, acct.creditsUsed)), invoice.PrepaidCreditAmountCents,
				"%s: credits applied", acct.name)
			require.Equal(t, int(OverageCents(acct.tier, acct.creditsUsed)), invoice.TotalAmountCents,
				"%s: only overage should be left to pay", acct.name)
			require.Equal(t, acct.events, fee.EventsCount, "%s: events billed", acct.name)

			t.Logf("%s (%s): usage invoice %s - %d cents of usage, %d covered by credits, %d to pay",
				acct.name, acct.tier.Code, invoice.Number,
				fee.AmountCents, invoice.PrepaidCreditAmountCents, invoice.TotalAmountCents)
		}
	})

	t.Run("the wallet balance is drawn down at settlement", func(t *testing.T) {
		// This is where credits actually move in self-hosted Lago: the settled
		// balance changes when an invoice is finalized and prepaid credits are
		// drawn down. The real-time ongoing balance is a premium feature.
		for _, acct := range accounts {
			want := CreditsRemaining(acct.tier, acct.creditsUsed)
			wallet, err := WaitForWalletBalance(ctx, client, acct.customerID, want)
			require.NoError(t, err, "%s: waiting for the wallet to settle", acct.name)

			require.Equal(t, CreditsConsumed(acct.tier, acct.creditsUsed),
				parseCredits(t, wallet.ConsumedCredits), "%s: credits consumed", acct.name)

			t.Logf("%s (%s): wallet settled at %s credits, %s consumed",
				acct.name, acct.tier.Code, wallet.CreditsBalance, wallet.ConsumedCredits)
		}
	})

	t.Run("each customer is billed their tier total", func(t *testing.T) {
		// The headline figure. The fee and the usage are on separate invoices, so
		// the sum across them is what the customer actually owes.
		for _, acct := range accounts {
			total, err := TotalInvoicedCents(ctx, client, acct.customerID)
			require.NoError(t, err, "%s: totalling invoices", acct.name)
			require.Equal(t, int(TotalBilledCents(acct.tier, acct.creditsUsed)), total,
				"%s: %s billed for %d credits", acct.name, acct.tier.Code, acct.creditsUsed)

			t.Logf("%s (%s): billed %d cents in total", acct.name, acct.tier.Code, total)
		}
	})
}

// parseCredits reads Lago's decimal-string credit counts. They are strings
// because credits can be fractional, and the exact formatting ("0" or "0.0") is
// the server's choice, so comparing the strings would be brittle.
func parseCredits(t *testing.T, credits string) int64 {
	t.Helper()
	if credits == "" {
		return 0
	}
	f, err := strconv.ParseFloat(credits, 64)
	require.NoError(t, err, "parsing credits %q", credits)
	return int64(f)
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

// isDockerUnavailable reports whether err is the daemon being absent rather than
// a genuine failure to start the stack.
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
