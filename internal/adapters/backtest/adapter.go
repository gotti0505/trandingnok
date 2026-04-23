// Package backtest provides the backtest adapter that drives Step() through a
// historical bar series.  It is the only "backtest" implementation in the system
// and intentionally calls the same Step() function used in live trading.
//
// Iron rule: this package must never be imported by internal/strategies/ or
// internal/quant/.  The dependency graph is one-way: backtest → strategy → quant.
package backtest

import (
	"quantsaas/internal/quant"
	"quantsaas/internal/strategy"
)

// Config configures one deterministic backtest run.
// Bars must be sorted time-ascending and may include warmup bars before EvalStartMs.
type Config struct {
	Bars        []quant.Bar
	EvalStartMs int64            // bars before this timestamp are warmup-only
	Chromosome  quant.Chromosome
	SpawnPoint  quant.SpawnPoint
	LotStep     float64 // min BTC quantity precision (e.g. 0.00001)
	LotMin      float64 // min BTC order qty; orders smaller than this are skipped
	TakerFee    float64 // e.g. 0.001 = 0.1% per side
}

// Metrics is the outcome of one backtest run.
type Metrics struct {
	FinalEquity float64
	MaxDrawdown float64 // peak-to-trough, 0.0 – 1.0
	ROI         float64 // Modified Dietz return
}

// RunBacktest drives strat.Step() through cfg.Bars and returns fitness metrics.
//
// Warmup phase (bars before EvalStartMs): Step() is called so indicators warm
// up to a realistic state; intents are discarded and the portfolio is untouched.
//
// Eval phase (EvalStartMs onwards): the portfolio is seeded with InitialCapital;
// all intents are applied at the bar's close price; NAV is recorded each bar for
// MaxDrawdown and Modified-Dietz ROI.
//
// Monthly injections (SpawnPoint.MonthlyInject) start at the first eval bar and
// occur at each natural-month boundary thereafter.
func RunBacktest(cfg Config, strat strategy.Strategy) Metrics {
	if len(cfg.Bars) == 0 || cfg.SpawnPoint.InitialCapital <= 0 {
		return Metrics{}
	}

	portfolio := quant.PortfolioState{
		ReserveFloor: cfg.SpawnPoint.ReserveFloor,
		NAVInitial:   cfg.SpawnPoint.InitialCapital,
	}
	runtime := quant.RuntimeState{}

	// Accumulate bar series as we iterate so Step() always sees the full history.
	closes := make([]float64, 0, len(cfg.Bars))
	highs := make([]float64, 0, len(cfg.Bars))
	lows := make([]float64, 0, len(cfg.Bars))
	timestamps := make([]int64, 0, len(cfg.Bars))

	inEval := false
	var evalFirstMs, evalLastMs int64
	var prevMonthKey string

	type cashFlow struct {
		dayFromStart int
		amount       float64
	}
	var flows []cashFlow
	var navCurve []float64

	for _, bar := range cfg.Bars {
		closes = append(closes, bar.Close)
		highs = append(highs, bar.High)
		lows = append(lows, bar.Low)
		timestamps = append(timestamps, bar.OpenTime)

		// Transition into eval period: seed the portfolio once.
		if !inEval && bar.OpenTime >= cfg.EvalStartMs {
			inEval = true
			evalFirstMs = bar.OpenTime
			portfolio.TotalUSDT = cfg.SpawnPoint.InitialCapital
			portfolio.SpendableUSDT = clamp0(portfolio.TotalUSDT - portfolio.ReserveFloor)
			prevMonthKey = quant.MonthKey(bar.OpenTime)
		}

		if inEval {
			evalLastMs = bar.OpenTime

			// Monthly injection at each natural-month boundary.
			mk := quant.MonthKey(bar.OpenTime)
			if mk != prevMonthKey && cfg.SpawnPoint.MonthlyInject > 0 {
				prevMonthKey = mk
				inj := cfg.SpawnPoint.MonthlyInject
				portfolio.TotalUSDT += inj
				portfolio.SpendableUSDT = clamp0(portfolio.TotalUSDT - portfolio.ReserveFloor)
				days := int((bar.OpenTime - evalFirstMs) / (24 * 60 * 60 * 1000))
				flows = append(flows, cashFlow{days, inj})
			}
		}

		// Step() runs for ALL bars (warmup + eval) so indicators warm up correctly.
		out := strat.Step(strategy.StrategyInput{
			Closes:     closes,
			Highs:      highs,
			Lows:       lows,
			Timestamps: timestamps,
			Portfolio:  portfolio,
			Runtime:    runtime,
			Params:     cfg.Chromosome,
			SpawnPoint: cfg.SpawnPoint,
		})
		runtime = out.Runtime

		// Apply intents and track NAV only during the eval period.
		if inEval && bar.Close > 0 {
			for _, intent := range out.Intents {
				portfolio = applyIntent(intent, portfolio, bar.Close, cfg.LotStep, cfg.LotMin, cfg.TakerFee)
			}
			btcTotal := portfolio.DeadStack + portfolio.FloatStack + portfolio.ColdSealedStack
			navCurve = append(navCurve, portfolio.TotalUSDT+btcTotal*bar.Close)
		}
	}

	if len(navCurve) == 0 {
		return Metrics{}
	}

	finalEquity := navCurve[len(navCurve)-1]
	maxDD := quant.MaxDrawdown(navCurve)

	// Modified Dietz ROI — strips out injection-driven NAV jumps.
	vStart := cfg.SpawnPoint.InitialCapital
	totalDays := int((evalLastMs - evalFirstMs) / (24 * 60 * 60 * 1000))
	sumF, weightedF := 0.0, 0.0
	for _, f := range flows {
		sumF += f.amount
		w := 1.0
		if totalDays > 0 {
			w = float64(totalDays-f.dayFromStart) / float64(totalDays)
		}
		weightedF += f.amount * w
	}
	denom := vStart + weightedF
	roi := 0.0
	if denom > 0 {
		roi = (finalEquity - vStart - sumF) / denom
	}

	return Metrics{FinalEquity: finalEquity, MaxDrawdown: maxDD, ROI: roi}
}

// applyIntent executes one trade intent against the in-memory portfolio.
// All fills are assumed to be market orders at the current close price.
func applyIntent(intent quant.TradeIntent, p quant.PortfolioState, price, lotStep, lotMin, fee float64) quant.PortfolioState {
	switch intent.Action {
	case "BUY":
		spend := intent.AmountUSDT
		if spend <= 0 || spend > p.SpendableUSDT {
			break
		}
		qty := snapToStep(spend/price*(1-fee), lotStep)
		if lotMin > 0 && qty < lotMin {
			break
		}
		p.TotalUSDT -= qty * price
		p.SpendableUSDT = clamp0(p.TotalUSDT - p.ReserveFloor)
		switch intent.LotType {
		case "DEAD_STACK":
			p.DeadStack += qty
		case "FLOATING":
			p.FloatStack += qty
		}

	case "SELL":
		qty := snapToStep(intent.QtyAsset, lotStep)
		if (lotMin > 0 && qty < lotMin) || qty > p.FloatStack {
			break
		}
		p.FloatStack -= qty
		p.TotalUSDT += qty * price * (1 - fee)
		p.SpendableUSDT = clamp0(p.TotalUSDT - p.ReserveFloor)

	case "RELEASE":
		// Ledger re-classification: DeadStack → FloatStack; no USDT change.
		qty := intent.QtyAsset
		if qty <= 0 || qty > p.DeadStack {
			break
		}
		p.DeadStack -= qty
		p.FloatStack += qty
	}
	return p
}

// snapToStep rounds qty down to the nearest multiple of step.
func snapToStep(qty, step float64) float64 {
	if step <= 0 {
		return qty
	}
	return float64(int64(qty/step)) * step
}

func clamp0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
