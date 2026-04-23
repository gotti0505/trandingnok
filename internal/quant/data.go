package quant

// Bar represents a single OHLCV candlestick.
type Bar struct {
	OpenTime int64
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// PortfolioState is the account snapshot fed into Step().
// All fields are read-only within a single Step() call.
type PortfolioState struct {
	TotalUSDT       float64 // gross USDT balance
	ReserveFloor    float64 // safety baseline — never spent (SpawnPoint-fixed)
	SpendableUSDT   float64 // TotalUSDT - ReserveFloor
	DeadStack       float64 // macro floor BTC — only grows via DCA
	FloatStack      float64 // micro floating BTC — eligible for Sigmoid sell
	ColdSealedStack float64 // permanently sealed BTC — Step() must not touch
	NAVInitial      float64 // initial NAV used for ROI calculation
}

// RuntimeState is the incremental read-write state that Step() consumes and
// emits each tick. It is persisted as JSON after every successful tick.
type RuntimeState struct {
	EMA200         float64   `json:"ema200"`          // current EMA-200 value (0 = uninitialised)
	EMA200History  []float64 `json:"ema200_history"`  // rolling last ≤11 EMA values, newest last
	MonthHigh      float64   `json:"month_high"`      // highest close seen in current natural month
	MonthStr       string    `json:"month_str"`       // "2006-01" — used for new-month detection
	TriggeredTiers []int     `json:"triggered_tiers"` // DCA tier indices (1/2/3) fired this month
	StopLossHit    bool      `json:"stop_loss_hit"`   // once set, micro engine stays silent
}

// TradeIntent is a single buy/sell decision produced by Step().
type TradeIntent struct {
	Action     string  // "BUY" | "SELL"
	Engine     string  // "MACRO" | "MICRO"
	LotType    string  // "DEAD_STACK" | "FLOATING"
	AmountUSDT float64 // BUY only — USDT to spend
	QtyAsset   float64 // SELL only — BTC quantity
}
