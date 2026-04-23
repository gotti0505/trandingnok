// Package sigmoid_dca implements the Sigmoid DCA strategy.
//
// The sole external entry point is (*SigmoidDCA).Step(), which satisfies the
// strategy.Strategy interface.  Step() is a pure function — identical inputs
// always produce identical outputs; no I/O of any kind is performed.
//
// Decision pipeline per tick:
//  1. Data adequacy check          — bail early when history is too short
//  2. Market state classification   — QUIET / BEAR / BULL via EMA200 + σ24h
//  3. Macro engine (BEAR only)      — price-drop DCA tiers → DEAD_STACK BUY
//  4. Micro engine (BULL only)      — Sigmoid balance → FLOATING SELL + stop-loss
//  5. DeadStack release check       — ROI ≥ trigger → RELEASE ledger intent
package sigmoid_dca

import (
	"quantsaas/internal/quant"
	"quantsaas/internal/strategy"
)

// minBarsRequired is the minimum candle history needed for σ24h (24 bars).
const minBarsRequired = 24

// SigmoidDCA is a stateless strategy executor.  All mutable state lives in
// StrategyInput.Runtime and is returned in StrategyOutput.Runtime.
type SigmoidDCA struct{}

// New returns a ready-to-use SigmoidDCA instance.
func New() *SigmoidDCA { return &SigmoidDCA{} }

// Step is the pure decision function.  It must never call http, sql, os, or
// time.Now().  No if-isBacktest branches allowed (docs §0 iron rule).
func (s *SigmoidDCA) Step(input strategy.StrategyInput) strategy.StrategyOutput {
	// 1. Data adequacy — return no-op when history is too short to be meaningful
	if len(input.Closes) < minBarsRequired {
		return strategy.StrategyOutput{
			Intents: []quant.TradeIntent{},
			Runtime: input.Runtime,
		}
	}

	price := input.Closes[len(input.Closes)-1]

	// MonthStr derived from the latest timestamp for the macro month-reset logic
	monthStr := ""
	if len(input.Timestamps) > 0 {
		monthStr = quant.MonthKey(input.Timestamps[len(input.Timestamps)-1])
	}

	// 2. Market state — also updates EMA200 inside RuntimeState
	ms, rt := quant.ComputeMarketState(quant.MarketStateInput{
		Closes: input.Closes,
		Highs:  input.Highs,
		Lows:   input.Lows,
		Price:  price,
		RT:     input.Runtime,
	})

	// QUIET → suspend all trading; return updated runtime (EMA200 still updated)
	if ms.IsQuiet {
		return strategy.StrategyOutput{Intents: []quant.TradeIntent{}, Runtime: rt}
	}

	var intents []quant.TradeIntent

	// 3. Macro engine: runs in BEAR market only
	if ms.State == "BEAR" {
		mo := runMacro(price, input.Portfolio, input.Params, rt, monthStr)
		rt = mo.RT
		if mo.Intent != nil {
			intents = append(intents, *mo.Intent)
		}
	}

	// 4. Micro engine: runs in BULL market only
	if ms.State == "BULL" {
		mo := runMicro(price, rt.EMA200, input.Portfolio, input.Params, rt)
		rt = mo.RT
		if mo.Intent != nil {
			intents = append(intents, *mo.Intent)
		}
	}

	// 5. DeadStack release: ROI-triggered ledger re-classification, every non-quiet tick
	if ri := runRelease(input.Portfolio, input.SpawnPoint, price); ri != nil {
		intents = append(intents, *ri)
	}

	if intents == nil {
		intents = []quant.TradeIntent{}
	}
	return strategy.StrategyOutput{Intents: intents, Runtime: rt}
}
