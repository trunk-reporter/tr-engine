package audio

import "math"

// JitterStats tracks inter-packet arrival jitter using Welford's online algorithm.
// Not safe for concurrent use — callers must synchronize externally.
type JitterStats struct {
	Count int     // number of deltas recorded
	Min   float64 // minimum delta (ms)
	Max   float64 // maximum delta (ms)
	Last  float64 // most recent delta (ms)
	mean  float64 // running mean (Welford's M)
	m2    float64 // running sum of squared deviations (Welford's S)
}

// Add records a new inter-packet delta in milliseconds.
func (js *JitterStats) Add(deltaMs float64) {
	js.Count++
	js.Last = deltaMs

	if js.Count == 1 {
		js.Min = deltaMs
		js.Max = deltaMs
		js.mean = deltaMs
		js.m2 = 0
		return
	}

	if deltaMs < js.Min {
		js.Min = deltaMs
	}
	if deltaMs > js.Max {
		js.Max = deltaMs
	}

	// Welford's online algorithm
	delta := deltaMs - js.mean
	js.mean += delta / float64(js.Count)
	delta2 := deltaMs - js.mean
	js.m2 += delta * delta2
}

// Mean returns the running mean.
func (js *JitterStats) Mean() float64 {
	return js.mean
}

// Variance returns the population variance (m2/count, 0 if count < 2).
func (js *JitterStats) Variance() float64 {
	if js.Count < 2 {
		return 0
	}
	return js.m2 / float64(js.Count)
}

// Stddev returns the population standard deviation (sqrt of variance).
func (js *JitterStats) Stddev() float64 {
	return math.Sqrt(js.Variance())
}

// Reset clears all accumulated stats.
func (js *JitterStats) Reset() {
	*js = JitterStats{}
}

// Snapshot returns a copy of current stats.
func (js *JitterStats) Snapshot() JitterStats {
	return *js
}
