package quant

import (
	"math"
)

// EMA computes the exponential moving average of the full series and returns
// the final value.  Seeds with the first element, then iterates forward.
// Returns 0 if the series is empty.
func EMA(series []float64, period int) float64 {
	n := len(series)
	if n == 0 || period <= 0 {
		return 0
	}
	alpha := 2.0 / float64(period+1)
	ema := series[0]
	for i := 1; i < n; i++ {
		ema = alpha*series[i] + (1-alpha)*ema
	}
	return ema
}

// UpdateEMA applies one incremental EMA step given the previous EMA value.
func UpdateEMA(prev, price float64, period int) float64 {
	if period <= 0 {
		return price
	}
	alpha := 2.0 / float64(period+1)
	return alpha*price + (1-alpha)*prev
}

// StdDev returns the sample standard deviation of the last `period` elements
// of series.  Returns 0 when the effective window has fewer than 2 points.
func StdDev(series []float64, period int) float64 {
	n := len(series)
	if n < 2 || period < 2 {
		return 0
	}
	start := n - period
	if start < 0 {
		start = 0
	}
	window := series[start:]
	m := len(window)
	if m < 2 {
		return 0
	}
	var sum float64
	for _, v := range window {
		sum += v
	}
	mean := sum / float64(m)
	var vsum float64
	for _, v := range window {
		d := v - mean
		vsum += d * d
	}
	return math.Sqrt(vsum / float64(m-1))
}

// MAVAbsChange returns the mean absolute change of the last L closes.
// Formula: Σ|close[i] − close[i−1]| for the L-element window, divided by L−1.
// Returns 0 when fewer than 2 points are available.
func MAVAbsChange(closes []float64, L int) float64 {
	n := len(closes)
	if n < 2 || L < 2 {
		return 0
	}
	start := n - L
	if start < 0 {
		start = 0
	}
	window := closes[start:]
	m := len(window)
	if m < 2 {
		return 0
	}
	var sum float64
	for i := 1; i < m; i++ {
		d := window[i] - window[i-1]
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return sum / float64(m-1)
}

// ClipFloat64 clamps v to the closed interval [lo, hi].
func ClipFloat64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// RoundToUSDT rounds v to two decimal places (cent precision).
func RoundToUSDT(v float64) float64 {
	return math.Round(v*100) / 100
}
