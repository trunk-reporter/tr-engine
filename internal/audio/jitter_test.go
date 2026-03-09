package audio

import (
	"math"
	"testing"
)

func TestJitterStatsEmpty(t *testing.T) {
	var js JitterStats

	if js.Count != 0 {
		t.Errorf("Count = %d, want 0", js.Count)
	}
	if js.Min != 0 {
		t.Errorf("Min = %f, want 0", js.Min)
	}
	if js.Max != 0 {
		t.Errorf("Max = %f, want 0", js.Max)
	}
	if js.Last != 0 {
		t.Errorf("Last = %f, want 0", js.Last)
	}
	if js.Mean() != 0 {
		t.Errorf("Mean() = %f, want 0", js.Mean())
	}
	if js.Stddev() != 0 {
		t.Errorf("Stddev() = %f, want 0", js.Stddev())
	}
	if js.Variance() != 0 {
		t.Errorf("Variance() = %f, want 0", js.Variance())
	}
}

func TestJitterStatsAccumulate(t *testing.T) {
	var js JitterStats

	deltas := []float64{20, 20, 20, 100, 20}
	for _, d := range deltas {
		js.Add(d)
	}

	if js.Count != 5 {
		t.Errorf("Count = %d, want 5", js.Count)
	}
	if js.Min != 20 {
		t.Errorf("Min = %f, want 20", js.Min)
	}
	if js.Max != 100 {
		t.Errorf("Max = %f, want 100", js.Max)
	}
	if js.Last != 20 {
		t.Errorf("Last = %f, want 20", js.Last)
	}

	// Mean of [20, 20, 20, 100, 20] = 180/5 = 36.0
	if math.Abs(js.Mean()-36.0) > 0.001 {
		t.Errorf("Mean() = %f, want 36.0", js.Mean())
	}

	if js.Stddev() <= 0 {
		t.Errorf("Stddev() = %f, want > 0", js.Stddev())
	}

	// Population variance = ((20-36)^2 + (20-36)^2 + (20-36)^2 + (100-36)^2 + (20-36)^2) / 5
	// = (256 + 256 + 256 + 4096 + 256) / 5 = 5120 / 5 = 1024
	if math.Abs(js.Variance()-1024.0) > 0.001 {
		t.Errorf("Variance() = %f, want 1024.0", js.Variance())
	}

	// Stddev = sqrt(1024) = 32
	if math.Abs(js.Stddev()-32.0) > 0.001 {
		t.Errorf("Stddev() = %f, want 32.0", js.Stddev())
	}
}

func TestJitterStatsReset(t *testing.T) {
	var js JitterStats

	js.Add(10)
	js.Add(50)
	js.Add(30)
	js.Reset()

	if js.Count != 0 {
		t.Errorf("Count = %d, want 0 after reset", js.Count)
	}
	if js.Min != 0 {
		t.Errorf("Min = %f, want 0 after reset", js.Min)
	}
	if js.Max != 0 {
		t.Errorf("Max = %f, want 0 after reset", js.Max)
	}
	if js.Last != 0 {
		t.Errorf("Last = %f, want 0 after reset", js.Last)
	}
	if js.Mean() != 0 {
		t.Errorf("Mean() = %f, want 0 after reset", js.Mean())
	}
	if js.Stddev() != 0 {
		t.Errorf("Stddev() = %f, want 0 after reset", js.Stddev())
	}
}

func TestJitterStatsSnapshot(t *testing.T) {
	var js JitterStats

	js.Add(10)
	js.Add(20)
	js.Add(30)

	snap := js.Snapshot()

	// Verify snapshot matches original
	if snap.Count != js.Count {
		t.Errorf("snapshot Count = %d, want %d", snap.Count, js.Count)
	}
	if snap.Min != js.Min {
		t.Errorf("snapshot Min = %f, want %f", snap.Min, js.Min)
	}
	if snap.Max != js.Max {
		t.Errorf("snapshot Max = %f, want %f", snap.Max, js.Max)
	}
	if snap.Last != js.Last {
		t.Errorf("snapshot Last = %f, want %f", snap.Last, js.Last)
	}
	if snap.Mean() != js.Mean() {
		t.Errorf("snapshot Mean() = %f, want %f", snap.Mean(), js.Mean())
	}

	// Mutate original
	js.Add(1000)

	// Snapshot should be unchanged
	if snap.Count != 3 {
		t.Errorf("snapshot Count = %d after mutation, want 3", snap.Count)
	}
	if snap.Max != 30 {
		t.Errorf("snapshot Max = %f after mutation, want 30", snap.Max)
	}
	if snap.Last != 30 {
		t.Errorf("snapshot Last = %f after mutation, want 30", snap.Last)
	}
	if math.Abs(snap.Mean()-20.0) > 0.001 {
		t.Errorf("snapshot Mean() = %f after mutation, want 20.0", snap.Mean())
	}
}
