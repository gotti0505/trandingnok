package quant

import "math"

// rTickMaxSell caps how much FloatStack may be sold in a single tick (5%).
// Hard-coded constant — not subject to GA evolution (docs §3.2).
const rTickMaxSell = 0.05

// MicroInput bundles everything ComputeMicroDecision needs.
type MicroInput struct {
	Price     float64
	EMA200    float64 // from RuntimeState, already computed this tick
	Portfolio PortfolioState
	Params    Chromosome
	RT        RuntimeState
}

// MicroOutput carries at most one SELL intent and the (possibly updated) RuntimeState.
type MicroOutput struct {
	Intent *TradeIntent // nil when no order this tick
	RT     RuntimeState
}

// ComputeMicroDecision runs the Sigmoid micro engine.
//
// Execution order (docs §3):
//  1. Stop-loss check — if ROI < stop_loss_threshold, liquidate all FloatStack
//  2. Sigmoid signal — sell a fraction of FloatStack proportional to price premium
//
// The micro engine only fires in BULL market state; the caller must gate on that.
// Docs reference: 策略數學引擎.md §3
func ComputeMicroDecision(in MicroInput) MicroOutput {
	rt := in.RT

	// --- 1. Stop-loss (docs §3.3, priority over Sigmoid) ---
	if !rt.StopLossHit {
		nav := in.Portfolio.TotalUSDT +
			(in.Portfolio.DeadStack+in.Portfolio.FloatStack+in.Portfolio.ColdSealedStack)*in.Price
		roi := 0.0
		if in.Portfolio.NAVInitial > 0 {
			roi = (nav - in.Portfolio.NAVInitial) / in.Portfolio.NAVInitial
		}
		if roi < in.Params.StopLossThreshold {
			rt.StopLossHit = true
			if in.Portfolio.FloatStack > 0 {
				return MicroOutput{
					Intent: &TradeIntent{
						Action:   "SELL",
						Engine:   "MICRO",
						LotType:  "FLOATING",
						QtyAsset: in.Portfolio.FloatStack,
					},
					RT: rt,
				}
			}
			return MicroOutput{RT: rt}
		}
	}

	// Once stop-loss is hit, micro engine stays silent until manually reset
	if rt.StopLossHit {
		return MicroOutput{RT: rt}
	}

	// --- 2. Sigmoid sell signal (docs §3.1 – §3.2) ---
	if in.Portfolio.FloatStack <= 0 || in.EMA200 <= 0 {
		return MicroOutput{RT: rt}
	}

	// x = k · ln(P / EMA200)
	x := in.Params.SigmoidSensitivity * math.Log(in.Price/in.EMA200)
	// s = 1 / (1 + e^{-x})
	s := 1.0 / (1.0 + math.Exp(-x))

	if s <= 0.5 {
		return MicroOutput{RT: rt} // price not above EMA200 enough
	}

	sellRatio := (s - 0.5) * 2.0                         // ∈ (0, 1]
	sellQty := in.Portfolio.FloatStack * sellRatio * rTickMaxSell

	if sellQty <= 0 {
		return MicroOutput{RT: rt}
	}

	return MicroOutput{
		Intent: &TradeIntent{
			Action:   "SELL",
			Engine:   "MICRO",
			LotType:  "FLOATING",
			QtyAsset: sellQty,
		},
		RT: rt,
	}
}
