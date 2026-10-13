package demo

import (
	"context"
	"fmt"

	"comfy/go-bill/lago"
	lagoapi "github.com/getlago/lago-go-client"
)

// Account is one provisioned customer on one tier: the plan they subscribed to,
// and the credit wallet their usage draws down.
type Account struct {
	// ID identifies the account in the gateway's routes. It is the tier code,
	// since the demo provisions exactly one customer per tier.
	ID string `json:"id"`

	Tier            string `json:"tier"`
	TierName        string `json:"tier_name"`
	MonthlyFeeCents int64  `json:"monthly_fee_cents"`
	Credits         int64  `json:"credits"`
	RateAmount      string `json:"rate_amount"`

	MetricCode     string `json:"metric_code"`
	PlanCode       string `json:"plan_code"`
	CustomerID     string `json:"customer_id"`
	SubscriptionID string `json:"subscription_id"`
}

// ProvisionAccounts creates everything in Lago that has to exist before usage
// can be reported: the credit meter, a plan per tier, and then one customer per
// tier with a subscription and a granted credit allowance.
//
// This is setup, not part of the request path. A real deployment would do it per
// customer from an admin flow; the demo does it at startup so the gateway comes
// up with three accounts to bill.
func ProvisionAccounts(ctx context.Context, client *lagoapi.Client, suffix string) ([]*Account, error) {
	catalog, err := lago.SeedTierPlans(ctx, client, suffix)
	if err != nil {
		return nil, fmt.Errorf("seeding the tier plans: %w", err)
	}

	accounts := make([]*Account, 0, len(lago.Tiers))
	for _, tier := range lago.Tiers {
		planCode, err := catalog.PlanCode(tier.Code)
		if err != nil {
			return nil, err
		}

		account := &Account{
			ID:              tier.Code,
			Tier:            tier.Code,
			TierName:        tier.Name,
			MonthlyFeeCents: tier.MonthlyFeeCents,
			Credits:         tier.Credits,
			RateAmount:      tier.RateAmount,
			MetricCode:      catalog.MetricCode,
			PlanCode:        planCode,
			CustomerID:      tier.Code + "-customer-" + suffix,
			SubscriptionID:  tier.Code + "-subscription-" + suffix,
		}

		if _, err := lago.CreateCustomer(ctx, client, account.CustomerID,
			"Comfy Compute ("+tier.Name+")", tier.Code+"@comfy.example"); err != nil {
			return nil, err
		}
		// Subscribing bills the monthly fee immediately: these plans are
		// pay-in-advance, so the tier invoice exists before any usage does.
		if _, err := lago.Subscribe(ctx, client, account.CustomerID, account.SubscriptionID, planCode); err != nil {
			return nil, err
		}
		if _, err := lago.GrantCredits(ctx, client, account.CustomerID, tier); err != nil {
			return nil, err
		}

		accounts = append(accounts, account)
	}
	return accounts, nil
}
