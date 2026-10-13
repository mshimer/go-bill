// Package lago stands up a self-hosted Lago billing instance in Docker and drives
// it from Go: three subscription tiers, each bundling a monthly allowance of
// credits that usage events draw down, and the invoices that fall out the far end.
//
// TUTORIAL.md explains Lago's domain model; README.md covers running the stack.
package lago

import (
	"fmt"
	"strings"
)

// Tier is one of the three subscription plans a customer chooses between.
//
// Each tier is a monthly fee that bundles an allowance of credits. Usage events
// consume credits; once the allowance is gone, further usage is billed at the
// tier's own rate. A larger tier costs more per month but less per credit.
//
// The rate is carried twice on purpose. RateAmount is the decimal string Lago
// wants - in a charge's properties and as the wallet's credit conversion rate -
// and RatePerCreditMilliCents is the same number as an exact integer so expected
// invoice amounts can be computed without floating point. TestTierRatesAgree
// keeps the two honest.
type Tier struct {
	// Code is the plan code in Lago, and the id the demo routes on.
	Code string

	// Name is the human label.
	Name string

	// MonthlyFeeCents is what the tier costs per month, charged in advance.
	MonthlyFeeCents int64

	// Credits is the allowance included in the monthly fee, granted to the
	// customer's wallet.
	Credits int64

	// RateAmount is the currency value of one credit, as the decimal string
	// Lago expects. Lago rejects a bare number here.
	RateAmount string

	// RatePerCreditMilliCents is RateAmount in thousandths of a cent, which
	// keeps the arithmetic exact: $0.0060 is 0.6 of a cent, which no integer
	// number of cents can represent.
	RatePerCreditMilliCents int64
}

// Tiers is the catalog a customer picks one of.
//
// The numbers are not arbitrary: in every tier the allowance is worth exactly
// what the tier costs, so MonthlyFeeCents == Credits x rate. That is what makes
// in-allowance usage net to zero against the granted credits and leaves the bill
// equal to the fee. TestAllowanceIsWorthTheFee asserts it.
var Tiers = []Tier{
	{Code: "tier-1", Name: "Tier 1", MonthlyFeeCents: 2000, Credits: 2000, RateAmount: "0.0100", RatePerCreditMilliCents: 1000},
	{Code: "tier-2", Name: "Tier 2", MonthlyFeeCents: 4000, Credits: 5000, RateAmount: "0.0080", RatePerCreditMilliCents: 800},
	{Code: "tier-3", Name: "Tier 3", MonthlyFeeCents: 6000, Credits: 10000, RateAmount: "0.0060", RatePerCreditMilliCents: 600},
}

// TierByCode looks a tier up by its code.
func TierByCode(code string) (Tier, bool) {
	for _, t := range Tiers {
		if t.Code == code {
			return t, true
		}
	}
	return Tier{}, false
}

// WalletValueCents is what a tier's full credit allowance is worth in currency.
// By construction this equals the monthly fee.
func WalletValueCents(t Tier) int64 {
	return t.Credits * t.RatePerCreditMilliCents / 1000
}

// UsageChargeCents is what credits of usage cost at the tier's rate, before any
// prepaid credits are applied. This is the charge fee Lago puts on the invoice.
func UsageChargeCents(t Tier, credits int64) int64 {
	if credits <= 0 {
		return 0
	}
	return credits * t.RatePerCreditMilliCents / 1000
}

// PrepaidCreditCents is how much of the usage charge the granted credits absorb.
//
// The wallet cannot go negative, so the offset is capped at what the allowance
// was worth. Lago does this clamping itself in
// Credits::AllocatePrepaidCreditsByWalletsService; this mirrors it so a test can
// predict the invoice.
func PrepaidCreditCents(t Tier, credits int64) int64 {
	usage := UsageChargeCents(t, credits)
	if wallet := WalletValueCents(t); usage > wallet {
		return wallet
	}
	return usage
}

// OverageCents is the part of the usage charge the allowance did not cover.
func OverageCents(t Tier, credits int64) int64 {
	return UsageChargeCents(t, credits) - PrepaidCreditCents(t, credits)
}

// TotalBilledCents is everything the customer is invoiced for a period: the
// monthly fee plus any overage.
//
// This is the headline number the tests assert, because the fee and the usage
// land on separate invoices - the fee is charged in advance at subscribe time,
// the usage when the period closes - so the sum across a customer's invoices is
// the figure that is stable either way.
func TotalBilledCents(t Tier, credits int64) int64 {
	return t.MonthlyFeeCents + OverageCents(t, credits)
}

// CreditsRemaining is the allowance left after consuming used credits. It floors
// at zero: overage is billed in currency, not as a negative balance.
func CreditsRemaining(t Tier, used int64) int64 {
	if used >= t.Credits {
		return 0
	}
	return t.Credits - used
}

// CreditsConsumed is how much of the allowance usage actually drew down, which
// is what Lago's settled wallet balance moves by. Usage beyond the allowance is
// billed rather than consumed, so this caps at the allowance.
func CreditsConsumed(t Tier, used int64) int64 {
	if used >= t.Credits {
		return t.Credits
	}
	return used
}

// Ladder renders the catalog as a readable table, for logs and docs.
func Ladder() string {
	var b strings.Builder
	for _, t := range Tiers {
		fmt.Fprintf(&b, "  %-8s $%5.2f/month  %6d credits  $%s/credit\n",
			t.Code, float64(t.MonthlyFeeCents)/100, t.Credits, t.RateAmount)
	}
	return b.String()
}
