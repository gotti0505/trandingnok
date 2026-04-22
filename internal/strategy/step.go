package strategy

// StrategyInput is the complete snapshot fed into Step().
// All fields are immutable within a single Step() call.
type StrategyInput struct {
	// Portfolio state
	DeadBTC    float64 // macro floor position — never sold
	FloatBTC   float64 // micro floating position — may be bought/sold
	SpendableUSDT float64 // available USDT for new purchases
	TotalEquity   float64 // DeadBTC*Price + FloatBTC*Price + SpendableUSDT

	// Market data
	Price   float64   // current mark price
	Candles []Candle  // recent OHLCV candles (oldest → newest)

	// Strategy parameters (chromosome)
	Params map[string]float64
}

// StrategyOutput is the pure decision produced by Step().
type StrategyOutput struct {
	// Signed USD delta: positive = buy, negative = sell, 0 = no-op
	TheoreticalUSD float64

	// Human-readable reason (for logging only, not used by execution layer)
	Reason string
}

// Candle represents a single OHLCV bar.
type Candle struct {
	OpenTime int64
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// Strategy is the interface every concrete strategy must implement.
type Strategy interface {
	// Step is a pure function — no side effects, no I/O, no goroutines.
	Step(input StrategyInput) StrategyOutput
}
