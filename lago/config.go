package lago

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"

	lagoapi "github.com/getlago/lago-go-client"
	"github.com/google/uuid"
)

// Currency for everything this package creates.
const Currency = lagoapi.USD

// WalletCode is the code given to every customer's credit wallet. Setting it
// explicitly means a wallet can be fetched back by customer and code rather than
// by Lago's generated UUID.
const WalletCode = "credits"

// CreditsField is the event property carrying how many credits a usage event
// consumes, and the field the billable metric aggregates.
const CreditsField = "credits"

// Catalog is the seeded billing configuration: the credit meter, and the plan
// backing each tier.
type Catalog struct {
	// MetricCode is the billable metric's code, which is also the code events
	// carry and the code the resulting invoice fee is labelled with.
	MetricCode string

	// MetricID is Lago's UUID for the metric. A charge references its metric by
	// this id and not by code, so it has to be captured at creation time.
	MetricID uuid.UUID

	// Plans maps a tier code to the plan code created for it.
	Plans map[string]string
}

// PlanCode returns the Lago plan code for a tier.
func (c *Catalog) PlanCode(tierCode string) (string, error) {
	code, ok := c.Plans[tierCode]
	if !ok {
		return "", fmt.Errorf("no plan seeded for tier %q", tierCode)
	}
	return code, nil
}

// SeedTierPlans creates the credit meter and one plan per tier.
//
// Order matters: a plan's charges reference a billable metric by UUID, so the
// metric has to exist first. Everything is suffixed so repeated runs against a
// surviving instance do not collide on Lago's uniqueness constraints.
//
// All three plans share a single metric. A billable metric defines how to measure
// and carries no price at all - the price lives on the charge - so one meter
// priced three different ways is the natural shape rather than a shortcut.
func SeedTierPlans(ctx context.Context, client *lagoapi.Client, suffix string) (*Catalog, error) {
	metricCode := CreditsField + "_" + suffix

	metric, lerr := client.BillableMetric().Create(ctx, &lagoapi.BillableMetricInput{
		Name:        "Credits",
		Code:        metricCode,
		Description: "Credits consumed by usage events, summed over the billing period.",
		// sum_agg is the right meter here: each event reports how many credits
		// it consumed and we want the arithmetic total for the period.
		AggregationType: lagoapi.SumAggregation,
		// Names the event property to read. Required for every aggregation
		// except count_agg, which counts events rather than a value on them.
		FieldName: CreditsField,
		// Usage resets each billing period rather than accumulating forever.
		Recurring: false,
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("creating billable metric %q: %w", metricCode, err)
	}

	catalog := &Catalog{
		MetricCode: metricCode,
		MetricID:   metric.LagoID,
		Plans:      make(map[string]string, len(Tiers)),
	}

	for _, tier := range Tiers {
		planCode := tier.Code + "-" + suffix
		if _, err := createTierPlan(ctx, client, catalog, tier, planCode); err != nil {
			return nil, err
		}
		catalog.Plans[tier.Code] = planCode
	}
	return catalog, nil
}

// createTierPlan builds the monthly plan for one tier: the fee, plus a single
// standard charge pricing each credit of usage.
//
// The fee is charged in advance, which is not a stylistic choice. A plan billed
// in arrears and terminated mid-period has its monthly fee **prorated**, so a
// subscription opened and closed minutes apart would bill a few cents instead of
// the tier price. Charging in advance invoices the whole fee up front, at the
// cost of the fee and the usage landing on separate invoices.
func createTierPlan(
	ctx context.Context,
	client *lagoapi.Client,
	catalog *Catalog,
	tier Tier,
	planCode string,
) (*lagoapi.Plan, error) {
	plan, lerr := client.Plan().Create(ctx, &lagoapi.PlanInput{
		Name: tier.Name,
		Code: planCode,
		Description: fmt.Sprintf("%s: %d credits included for $%.2f per month, then $%s per credit.",
			tier.Name, tier.Credits, float64(tier.MonthlyFeeCents)/100, tier.RateAmount),
		Interval:       lagoapi.PlanMonthly,
		AmountCents:    int(tier.MonthlyFeeCents),
		AmountCurrency: Currency,
		PayInAdvance:   true,
		Charges: []lagoapi.PlanChargeInput{{
			BillableMetricID: catalog.MetricID,
			AmountCurrency:   Currency,
			// standard: one flat price per credit. The tier ladder lives in the
			// catalog of plans, not inside a single charge, so there is nothing
			// graduated or tiered to express here.
			ChargeModel: lagoapi.StandardChargeModel,
			// The Go client has no typed representation of charge properties.
			// The price has to match the wallet's rate_amount exactly, or a
			// credit of usage would not cost one credit of allowance.
			Properties: map[string]any{
				"amount": tier.RateAmount,
			},
		}},
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("creating plan %q for tier %q: %w", planCode, tier.Code, err)
	}
	return plan, nil
}

// CreateCustomer registers a customer keyed by the caller's own external id.
func CreateCustomer(ctx context.Context, client *lagoapi.Client, externalID, name, email string) (*lagoapi.Customer, error) {
	customer, lerr := client.Customer().Create(ctx, &lagoapi.CustomerInput{
		ExternalID: externalID,
		Name:       name,
		Email:      email,
		Currency:   Currency,
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("creating customer %q: %w", externalID, err)
	}
	return customer, nil
}

// Subscribe puts a customer on a tier's plan.
//
// The subscription's external id is the one events reference, and it is separate
// from the customer's: a customer can hold several subscriptions, and usage has
// to attach to exactly one of them.
//
// Anniversary billing starts the period now, rather than at the start of the
// calendar month, which keeps the window a test's events land in unambiguous.
func Subscribe(ctx context.Context, client *lagoapi.Client, customerExternalID, subscriptionExternalID, planCode string) (*lagoapi.Subscription, error) {
	sub, lerr := client.Subscription().Create(ctx, &lagoapi.SubscriptionInput{
		ExternalCustomerID: customerExternalID,
		ExternalID:         subscriptionExternalID,
		PlanCode:           planCode,
		Name:               planCode,
		BillingTime:        lagoapi.Anniversary,
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("subscribing %q to plan %q: %w", customerExternalID, planCode, err)
	}
	return sub, nil
}

// GrantCredits gives a customer their tier's credit allowance as a wallet.
//
// Two details carry the whole credit model:
//
//   - RateAmount is the currency value of one credit, and it must match the
//     tier's charge price. That equality is what makes one credit of usage
//     consume exactly one credit of allowance.
//   - AppliesTo restricts the wallet to charge fees. Without it a wallet pays
//     every fee on an invoice, the monthly subscription fee included, and a
//     customer who stayed inside their allowance would have their tier fee paid
//     by their own credits - billing $18 instead of $60.
//
// The credits are granted, not purchased. GrantedCredits adds balance and issues
// no invoice; PaidCredits would bill for them as a separate `credit` invoice.
func GrantCredits(ctx context.Context, client *lagoapi.Client, customerExternalID string, tier Tier) (*lagoapi.Wallet, error) {
	code := WalletCode
	wallet, lerr := client.Wallet().Create(ctx, &lagoapi.WalletInput{
		ExternalCustomerID: customerExternalID,
		Name:               tier.Name + " credits",
		Code:               &code,
		Currency:           Currency,
		RateAmount:         tier.RateAmount,
		GrantedCredits:     strconv.FormatInt(tier.Credits, 10),
		AppliesTo: lagoapi.AppliesTo{
			FeeTypes: []string{"charge"},
		},
		// Deliberately not setting RecurringTransactionRules. A monthly top-up
		// rule is a premium feature and is dropped silently without a license -
		// Wallets::RecurringTransactionRules::CreateService returns before doing
		// anything - so relying on it here would look like it worked. The field
		// also lacks omitempty in the Go client, so it always marshals as null.
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("granting %d credits to %q: %w", tier.Credits, customerExternalID, err)
	}
	return wallet, nil
}

// SendCredits reports count usage events, each consuming perEvent credits, and
// returns the transaction ids it used.
//
// No timestamp is set, so Lago stamps each event at reception. That is the safe
// default: an event timestamped outside the subscription's active window matches
// no subscription and is silently dropped rather than rejected.
//
// Transaction ids are the idempotency key, so each one is generated fresh.
func SendCredits(
	ctx context.Context,
	client *lagoapi.Client,
	subscriptionExternalID, metricCode string,
	perEvent int64,
	count int,
) ([]string, error) {
	events := make([]lagoapi.EventInput, 0, count)
	ids := make([]string, 0, count)

	for i := 0; i < count; i++ {
		id := fmt.Sprintf("credits-%s-%d", randomSuffix(), i)
		ids = append(ids, id)
		events = append(events, lagoapi.EventInput{
			TransactionID:          id,
			ExternalSubscriptionID: subscriptionExternalID,
			Code:                   metricCode,
			Properties:             map[string]any{CreditsField: perEvent},
		})
	}

	if _, lerr := client.Event().Batch(ctx, events); lerr != nil {
		return nil, fmt.Errorf("sending %d usage events: %w", count, asError(lerr))
	}
	return ids, nil
}

// TerminateSubscription ends a subscription and asks Lago to bill the usage
// accrued so far.
//
// This is how a test gets a usage invoice without waiting for a billing period
// to close. Lago offers no way to force period billing on demand: one-off
// invoices can only carry add-ons, progressive billing is a premium add-on, and
// there is no time-travel endpoint.
//
// The invoice does not exist when this returns. Termination enqueues
// BillSubscriptionJob on Sidekiq, and the REST layer offers no synchronous
// alternative, so the caller has to poll - see WaitForInvoiceMatching.
func TerminateSubscription(ctx context.Context, client *lagoapi.Client, subscriptionExternalID string) (*lagoapi.Subscription, error) {
	sub, lerr := client.Subscription().Terminate(ctx, lagoapi.SubscriptionTerminateInput{
		ExternalID: subscriptionExternalID,
		// Already the default, but stating it documents the intent: bill the
		// unbilled usage rather than discarding it.
		OnTerminationInvoice: lagoapi.OnTerminationInvoiceGenerate,
		// Skip the credit note Lago would otherwise issue for the unused part of
		// a prepaid month. These plans are billed in advance, so terminating
		// mid-period defaults to refunding the unconsumed time - which both
		// contradicts "the month is paid for" and quietly breaks the credit
		// arithmetic, because a credit note reduces how much of an invoice
		// prepaid credits are allowed to cover.
		OnTerminationCreditNote: lagoapi.OnTerminationCreditNoteSkip,
	})
	if err := asError(lerr); err != nil {
		return nil, fmt.Errorf("terminating subscription %q: %w", subscriptionExternalID, err)
	}
	return sub, nil
}

// randomSuffix returns a short random hex string for building unique codes.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return hex.EncodeToString(b[:])
}
