package lago

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTierRatesAgree proves the two representations of each tier's credit rate
// cannot drift: the decimal string sent to Lago and the integer milli-cents used
// to predict invoice amounts must describe the same number.
func TestTierRatesAgree(t *testing.T) {
	for _, tier := range Tiers {
		dollars, ok := new(big.Rat).SetString(tier.RateAmount)
		require.True(t, ok, "tier %q has an unparseable RateAmount %q", tier.Code, tier.RateAmount)

		// dollars -> milli-cents is a factor of 100 cents x 1000.
		want := new(big.Rat).Mul(dollars, big.NewRat(100_000, 1))
		got := new(big.Rat).SetInt64(tier.RatePerCreditMilliCents)
		require.Zero(t, want.Cmp(got),
			"tier %q: RateAmount %s is %s milli-cents but RatePerCreditMilliCents is %d",
			tier.Code, tier.RateAmount, want.RatString(), tier.RatePerCreditMilliCents)
	}
}

// TestAllowanceIsWorthTheFee asserts the invariant the whole model rests on: a
// tier's credit allowance is worth exactly what the tier costs per month. That
// equality is why usage inside the allowance nets to zero against the granted
// credits and the customer is billed just the fee.
func TestAllowanceIsWorthTheFee(t *testing.T) {
	for _, tier := range Tiers {
		require.Equal(t, tier.MonthlyFeeCents, WalletValueCents(tier),
			"tier %q: %d credits at %s are worth %d cents, but the tier costs %d",
			tier.Code, tier.Credits, tier.RateAmount, WalletValueCents(tier), tier.MonthlyFeeCents)
	}
}

// TestCatalogIsWellFormed checks the catalog itself: distinct codes, and a ladder
// where paying more per month buys both more credits and a better unit price.
// That ordering is the whole reason a customer would choose a larger tier.
func TestCatalogIsWellFormed(t *testing.T) {
	require.Len(t, Tiers, 3, "the catalog should offer three tiers")

	seen := make(map[string]bool, len(Tiers))
	for i, tier := range Tiers {
		require.NotEmpty(t, tier.Code, "tier %d needs a code", i)
		require.False(t, seen[tier.Code], "duplicate tier code %q", tier.Code)
		seen[tier.Code] = true

		require.Positive(t, tier.MonthlyFeeCents, "tier %q needs a fee", tier.Code)
		require.Positive(t, tier.Credits, "tier %q needs an allowance", tier.Code)

		if i == 0 {
			continue
		}
		prev := Tiers[i-1]
		require.Greater(t, tier.MonthlyFeeCents, prev.MonthlyFeeCents,
			"tier %q should cost more than %q", tier.Code, prev.Code)
		require.Greater(t, tier.Credits, prev.Credits,
			"tier %q should include more credits than %q", tier.Code, prev.Code)
		require.Less(t, tier.RatePerCreditMilliCents, prev.RatePerCreditMilliCents,
			"tier %q should price credits below %q", tier.Code, prev.Code)
	}
}

// TestTierByCode covers the lookup, including the miss - a bad tier code should
// be reported rather than silently yielding a zero-value tier that bills nothing.
func TestTierByCode(t *testing.T) {
	tier, ok := TierByCode("tier-2")
	require.True(t, ok, "tier-2 should be in the catalog")
	require.EqualValues(t, 5000, tier.Credits, "tier-2 allowance")

	_, ok = TierByCode("tier-99")
	require.False(t, ok, "an unknown tier code should not resolve")
}

// TestBilling walks the three scenarios the demo exercises, plus the boundaries
// either side of the allowance, where off-by-one errors in metered billing hide.
func TestBilling(t *testing.T) {
	tests := []struct {
		name      string
		tier      string
		used      int64
		usage     int64 // the charge fee, before credits
		prepaid   int64 // credits applied
		overage   int64
		total     int64 // fee + overage: what the customer is billed
		remaining int64
		consumed  int64
	}{
		{
			name: "no usage bills just the fee",
			tier: "tier-1", used: 0,
			usage: 0, prepaid: 0, overage: 0, total: 2000, remaining: 2000, consumed: 0,
		},
		{
			name: "usage inside the allowance is absorbed",
			tier: "tier-1", used: 1500,
			usage: 1500, prepaid: 1500, overage: 0, total: 2000, remaining: 500, consumed: 1500,
		},
		{
			name: "one credit short of the allowance",
			tier: "tier-1", used: 1999,
			usage: 1999, prepaid: 1999, overage: 0, total: 2000, remaining: 1, consumed: 1999,
		},
		{
			name: "exactly the allowance",
			tier: "tier-1", used: 2000,
			usage: 2000, prepaid: 2000, overage: 0, total: 2000, remaining: 0, consumed: 2000,
		},
		{
			name: "one credit over the allowance",
			tier: "tier-1", used: 2001,
			usage: 2001, prepaid: 2000, overage: 1, total: 2001, remaining: 0, consumed: 2000,
		},
		{
			name: "the over-spender in the demo",
			tier: "tier-1", used: 2400,
			usage: 2400, prepaid: 2000, overage: 400, total: 2400, remaining: 0, consumed: 2000,
		},
		{
			name: "the exact-spender in the demo",
			tier: "tier-2", used: 5000,
			usage: 4000, prepaid: 4000, overage: 0, total: 4000, remaining: 0, consumed: 5000,
		},
		{
			name: "the under-spender in the demo",
			tier: "tier-3", used: 3000,
			usage: 1800, prepaid: 1800, overage: 0, total: 6000, remaining: 7000, consumed: 3000,
		},
		{
			name: "heavy overage on the largest tier",
			tier: "tier-3", used: 20000,
			usage: 12000, prepaid: 6000, overage: 6000, total: 12000, remaining: 0, consumed: 10000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tier, ok := TierByCode(tc.tier)
			require.True(t, ok, "tier %q", tc.tier)

			require.Equal(t, tc.usage, UsageChargeCents(tier, tc.used), "usage charge cents")
			require.Equal(t, tc.prepaid, PrepaidCreditCents(tier, tc.used), "prepaid credit cents")
			require.Equal(t, tc.overage, OverageCents(tier, tc.used), "overage cents")
			require.Equal(t, tc.total, TotalBilledCents(tier, tc.used), "total billed cents")
			require.Equal(t, tc.remaining, CreditsRemaining(tier, tc.used), "credits remaining")
			require.Equal(t, tc.consumed, CreditsConsumed(tier, tc.used), "credits consumed")
		})
	}
}

// TestInAllowanceUsageIsFree states the property behind the invariant: anywhere
// at or below the allowance, the customer pays exactly the monthly fee.
func TestInAllowanceUsageIsFree(t *testing.T) {
	for _, tier := range Tiers {
		for _, used := range []int64{0, 1, tier.Credits / 2, tier.Credits - 1, tier.Credits} {
			require.Equal(t, tier.MonthlyFeeCents, TotalBilledCents(tier, used),
				"tier %q at %d of %d credits should bill just the fee", tier.Code, used, tier.Credits)
			require.Zero(t, OverageCents(tier, used),
				"tier %q at %d credits should have no overage", tier.Code, used)
		}
	}
}

// TestOverageGrowsAtTheTierRate checks the billing never falls as usage grows,
// and that past the allowance each extra credit costs exactly the tier's rate.
func TestOverageGrowsAtTheTierRate(t *testing.T) {
	for _, tier := range Tiers {
		var prev int64
		for used := int64(0); used <= tier.Credits+100; used++ {
			got := TotalBilledCents(tier, used)
			require.GreaterOrEqual(t, got, prev,
				"tier %q: bill fell between %d and %d credits", tier.Code, used-1, used)
			prev = got
		}

		// A thousand credits past the allowance costs a thousand credits' worth.
		over := TotalBilledCents(tier, tier.Credits+1000) - tier.MonthlyFeeCents
		require.Equal(t, 1000*tier.RatePerCreditMilliCents/1000, over,
			"tier %q: 1000 credits of overage", tier.Code)
	}
}
