package strategy

import "quantsaas/internal/quant"

// StrategyInput is the complete immutable snapshot for one Step() call.
// Docs reference: 策略數學引擎.md §1.2
type StrategyInput struct {
	Closes     []float64 // close prices, oldest → newest
	Highs      []float64 // high prices, same length as Closes (needed for σ24h)
	Lows       []float64 // low prices, same length as Closes (needed for σ24h)
	Timestamps []int64   // millisecond timestamps matching Closes
	Portfolio  quant.PortfolioState
	Runtime    quant.RuntimeState
	Params     quant.Chromosome
	SpawnPoint quant.SpawnPoint // birth-time params — GA never evolves these
}

// StrategyOutput is the pure decision produced by Step().
// Docs reference: 策略數學引擎.md §1.3
type StrategyOutput struct {
	Intents []quant.TradeIntent // may be empty — never nil
	Runtime quant.RuntimeState  // updated state; SaaS must persist this after every tick
}

// Strategy is the interface every concrete strategy must implement.
// Step is a pure function — no side effects, no I/O, no goroutines.
type Strategy interface {
	Step(input StrategyInput) StrategyOutput
}
