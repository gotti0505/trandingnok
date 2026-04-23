package quant

import (
	"fmt"
	"time"
)

const (
	macroMinOrderUSDT = 10.1 // minimum DCA order size — docs §3E / plan §3E

	// tier multipliers and spend ratios — docs §4.1
	tier1DropMul  = 1.0
	tier2DropMul  = 1.5
	tier3DropMul  = 2.0
	tier1SpendPct = 0.05 // 5% of SpendableUSDT
	tier2SpendPct = 0.10 // 10%
	tier3SpendPct = 0.15 // 15%
)

// MacroInput bundles everything ComputeMacroDecision needs.
type MacroInput struct {
	Price     float64      // current close
	Portfolio PortfolioState
	Params    Chromosome
	RT        RuntimeState // mutable — caller owns the update via the returned RuntimeState
	// MonthStr is derived from the timestamp by the caller ("2006-01")
	MonthStr string
}

// MacroOutput carries the buy intent (if any) and the updated runtime state.
type MacroOutput struct {
	Intent *TradeIntent // nil when no order is generated this tick
	RT     RuntimeState
}

// ComputeMacroDecision evaluates the three DCA price-drop tiers and returns at
// most one BUY intent per tick.  It also handles the monthly state reset.
//
// Macro engine iron rule: only BUY intents, never SELL.  All buys go to DEAD_STACK.
// Docs reference: 策略數學引擎.md §4
func ComputeMacroDecision(in MacroInput) MacroOutput {
	rt := in.RT

	// --- Monthly reset (docs §4.2) ---
	if in.MonthStr != rt.MonthStr {
		rt.MonthStr = in.MonthStr
		rt.MonthHigh = in.Price // first close of the new month becomes the baseline
		rt.TriggeredTiers = nil
	} else {
		// Update running month-high
		if in.Price > rt.MonthHigh {
			rt.MonthHigh = in.Price
		}
	}

	// Nothing to buy without spendable capital
	if in.Portfolio.SpendableUSDT <= 0 || rt.MonthHigh <= 0 {
		return MacroOutput{RT: rt}
	}

	drop := in.Params.DCADropPct

	type tier struct {
		id       int
		trigger  float64 // price threshold below which the tier fires
		spendPct float64
	}
	tiers := []tier{
		{1, rt.MonthHigh * (1 - drop*tier1DropMul), tier1SpendPct},
		{2, rt.MonthHigh * (1 - drop*tier2DropMul), tier2SpendPct},
		{3, rt.MonthHigh * (1 - drop*tier3DropMul), tier3SpendPct},
	}

	for _, t := range tiers {
		if in.Price >= t.trigger {
			continue // price hasn't dropped enough
		}
		if containsInt(rt.TriggeredTiers, t.id) {
			continue // already fired this month
		}
		amount := RoundToUSDT(in.Portfolio.SpendableUSDT * t.spendPct)
		if amount < macroMinOrderUSDT {
			continue // below minimum — skip without marking triggered
		}
		// Clamp to available capital
		if amount > in.Portfolio.SpendableUSDT {
			amount = RoundToUSDT(in.Portfolio.SpendableUSDT)
		}
		rt.TriggeredTiers = append(rt.TriggeredTiers, t.id)
		intent := &TradeIntent{
			Action:     "BUY",
			Engine:     "MACRO",
			LotType:    "DEAD_STACK",
			AmountUSDT: amount,
		}
		return MacroOutput{Intent: intent, RT: rt}
	}
	return MacroOutput{RT: rt}
}

// MonthKey formats an int64 millisecond timestamp as "2006-01" for monthly bucketing.
func MonthKey(tsMillis int64) string {
	if tsMillis <= 0 {
		return ""
	}
	t := time.Unix(tsMillis/1000, 0).UTC()
	return fmt.Sprintf("%d-%02d", t.Year(), t.Month())
}

// containsInt reports whether slice contains v.
func containsInt(slice []int, v int) bool {
	for _, x := range slice {
		if x == v {
			return true
		}
	}
	return false
}
