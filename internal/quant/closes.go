package quant

// ExtractCloses returns the Close field of each Bar in order.
// Spot-strategy kernels must use this helper instead of referencing Bar directly,
// preserving the invariant that strategy code is independent of the Bar struct.
func ExtractCloses(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// ExtractHighs returns the High field of each Bar in order.
func ExtractHighs(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.High
	}
	return out
}

// ExtractLows returns the Low field of each Bar in order.
func ExtractLows(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Low
	}
	return out
}

// ExtractTimestamps returns the OpenTime field (milliseconds) of each Bar in order.
func ExtractTimestamps(bars []Bar) []int64 {
	out := make([]int64, len(bars))
	for i, b := range bars {
		out[i] = b.OpenTime
	}
	return out
}
