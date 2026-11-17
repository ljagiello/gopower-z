package cli

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// series samples a signal every dt seconds over a window, with Gaussian noise
// of the given standard deviation (amperes) on the current.
func series(window, dt, noise float64, seed uint64, cur func(t float64) float64) []sample {
	r := rand.New(rand.NewPCG(seed, seed+1))
	var ss []sample
	for k := 0; float64(k)*dt < window; k++ {
		t := float64(k) * dt
		i := cur(t) + r.NormFloat64()*noise
		ss = append(ss, sample{T: t, V: 5.076, I: i, P: 5.076 * i, DP: 3.0, DM: 0.002, Temp: 28, TempOK: true})
	}
	return ss
}

// The case that motivated bins and shifts: a charger switching on halfway
// through a window turns 134 mA into 441 mA. min/mean/max alone read as a
// noisy 290 mA; the shift list must name the step, once, at the right time.
func TestShiftChargeStep(t *testing.T) {
	ss := series(60, 0.1, 0.003, 1, func(t float64) float64 {
		if t >= 42.3 {
			return 0.441
		}
		return 0.134
	})
	// from/to are measured from the located step, not from bin edges, so
	// they do not depend on where the step falls inside a bin.
	for _, binS := range []float64{0.5, 1, 2, 5} {
		a := analyze(ss, 60, binS, 0)
		if len(a.Shifts) != 1 {
			t.Fatalf("bin %v: want one shift, got %+v", binS, a.Shifts)
		}
		s := a.Shifts[0]
		if s.Field != "ibus_a" || math.Abs(s.TS-42.3) > 0.11 || math.Abs(s.From-0.134) > 0.001 || math.Abs(s.To-0.441) > 0.001 {
			t.Errorf("bin %v: shift %+v, want ibus_a 0.134 -> 0.441 at 42.3 s", binS, s)
		}
	}
}

func TestShiftNoneOnNoise(t *testing.T) {
	for seed := range uint64(20) {
		ss := series(60, 0.1, 0.004, seed, func(float64) float64 { return 0.09 })
		if a := analyze(ss, 60, 0.5, 0); len(a.Shifts) != 0 {
			t.Fatalf("seed %d: flat noise reported shifts %+v", seed, a.Shifts)
		}
	}
}

// Short bursts (a radio waking for a fraction of a bin) are not level shifts.
func TestShiftIgnoresShortBursts(t *testing.T) {
	ss := series(60, 0.05, 0.002, 3, func(t float64) float64 {
		if math.Mod(t, 5) < 0.3 {
			return 0.35
		}
		return 0.09
	})
	if a := analyze(ss, 60, 1, 0); len(a.Shifts) != 0 {
		t.Errorf("bursts reported as shifts: %+v", a.Shifts)
	}
}

// Long periodic bursts are real level changes, but the list stays bounded.
func TestShiftListBounded(t *testing.T) {
	ss := series(600, 0.1, 0.001, 4, func(t float64) float64 {
		if math.Mod(t, 6) < 3 {
			return 0.4
		}
		return 0.1
	})
	a := analyze(ss, 600, 1, 0)
	if len(a.Shifts) != maxShifts {
		t.Fatalf("want the list capped at %d, got %d", maxShifts, len(a.Shifts))
	}
	for k := 1; k < len(a.Shifts); k++ {
		if a.Shifts[k].TS < a.Shifts[k-1].TS {
			t.Fatalf("shifts not in time order: %+v", a.Shifts)
		}
	}
}

// A USB device dropping off the bus shows as D+ falling; a PD renegotiation
// as VBUS rising. Both are reported with their field.
func TestShiftOtherFields(t *testing.T) {
	ss := series(30, 0.1, 0.002, 5, func(float64) float64 { return 0.09 })
	for k := range ss {
		if ss[k].T >= 10 {
			ss[k].DP = 0.01
		}
		if ss[k].T >= 20 {
			ss[k].V = 9.02
		}
	}
	a := analyze(ss, 30, 0.5, 0)
	got := map[string]float64{}
	for _, s := range a.Shifts {
		got[s.Field] = s.TS
	}
	if len(a.Shifts) != 2 || math.Abs(got["dp_v"]-10) > 0.11 || math.Abs(got["vbus_v"]-20) > 0.11 {
		t.Errorf("shifts %+v, want dp_v at 10 s and vbus_v at 20 s", a.Shifts)
	}
}

// The interval must describe the signal, not the polling rate: the meter's
// value only changes at its own sample rate, so polling twice as fast reads
// each value twice. s/sqrt(n) would shrink by sqrt(2); batch means must not.
func TestCI95IndependentOfPollingRate(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	levels := make([]float64, 600) // a new value every 0.1 s (a 10 SPS meter)
	for k := range levels {
		levels[k] = 0.09 + r.NormFloat64()*0.003
	}
	at := func(t float64) float64 { return levels[min(len(levels)-1, int(t*10))] }
	ci := func(dt float64) float64 {
		a := analyze(series(60, dt, 0, 0, at), 60, 1, 0)
		if a.Stats.IBusA.CI95 == nil {
			t.Fatalf("no ci95 at dt %v", dt)
		}
		return *a.Stats.IBusA.CI95
	}
	slow, fast := ci(0.05), ci(0.025)
	if fast < 0.9*slow {
		t.Errorf("ci95 shrank from %v to %v just by polling twice as fast", slow, fast)
	}
	// And it is in the right range for this signal: the mean of 600
	// independent values with sd 3 mA has a 95% interval of about 0.24 mA.
	if slow < 0.00012 || slow > 0.0005 {
		t.Errorf("ci95 %v A, want about 0.24 mA", slow)
	}
}

// A meter at 2 SPS changes its value every 0.5 s: over a 5 s window the
// 0.25 s batches would share readings, so no interval is claimed. Over 20 s
// the batches are long enough again.
func TestCI95NeedsBatchesLongerThanMeterPeriod(t *testing.T) {
	ss := series(20, 0.05, 0.003, 11, func(float64) float64 { return 0.09 })
	short := ss[:100] // the first 5 s
	if a := analyze(short, 5, 0.5, 0.5); a.Stats.IBusA.CI95 != nil {
		t.Errorf("ci95 %v from 0.25 s batches of a 2 SPS meter", *a.Stats.IBusA.CI95)
	}
	if a := analyze(ss, 20, 0.5, 0.5); a.Stats.IBusA.CI95 == nil {
		t.Error("no ci95 from 1 s batches of a 2 SPS meter")
	}
	if a := analyze(short, 5, 0.5, 0); a.Stats.IBusA.CI95 == nil {
		t.Error("unknown meter period must not suppress ci95")
	}
}

// Windows cut short (an agent harness sends SIGTERM) must still marshal:
// encoding/json refuses NaN and Inf, which would lose the whole result.
func TestAnalyzeDegenerateWindows(t *testing.T) {
	one := []sample{{T: 0, V: 5, I: 0.1, P: 0.5, DP: 3, DM: 0}}
	two := []sample{one[0], {T: 0.1, V: 5, I: 0.1, P: 0.5, DP: 3, DM: 0}}
	constant := series(10, 0.1, 0, 0, func(float64) float64 { return 0.25 })
	for name, c := range map[string]struct {
		ss     []sample
		window float64
	}{
		"none": {nil, 0}, "one": {one, 0.05}, "one at zero length": {one, 0}, "two": {two, 0.2},
		"constant": {constant, 10}, "single bin": {constant, 10},
	} {
		binS := 0.5
		if name == "single bin" {
			binS = 60
		}
		a := analyze(c.ss, c.window, binS, 0)
		if _, err := json.Marshal(measureResult{Stats: a.Stats, BinS: a.BinS, Bins: a.Bins, Shifts: a.Shifts}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.Bins == nil || a.Shifts == nil {
			t.Errorf("%s: bins/shifts must be [] not null", name)
		}
		if len(c.ss) > 0 && a.Stats.IBusA == nil {
			t.Errorf("%s: no current stats", name)
		}
		if len(c.ss) < minCIBatches && a.Stats.IBusA != nil && a.Stats.IBusA.CI95 != nil {
			t.Errorf("%s: ci95 from too few batches", name)
		}
	}
	a := analyze(constant, 10, 0.5, 0)
	if s := a.Stats.IBusA; s.Stdev != 0 || s.CI95 == nil || *s.CI95 != 0 || s.P5 != 0.25 || s.P95 != 0.25 {
		t.Errorf("constant series stats %+v", s)
	}
}

func TestBins(t *testing.T) {
	ss := series(10.25, 0.1, 0, 0, func(t float64) float64 { return t / 100 })
	a := analyze(ss, 10.25, 1, 0)
	if len(a.Bins) != 11 || a.BinS != 1 {
		t.Fatalf("want 11 bins of 1 s (the last one short), got %d of %v", len(a.Bins), a.BinS)
	}
	n := 0
	for k, b := range a.Bins {
		n += b.Samples
		if b.TS != float64(k) {
			t.Errorf("bin %d starts at %v", k, b.TS)
		}
	}
	if n != len(ss) {
		t.Errorf("bins hold %d samples, want %d", n, len(ss))
	}
	if b := a.Bins[3]; b.IBusA.Min != 0.03 || b.IBusA.Max != 0.039 || math.Abs(b.IBusA.Mean-0.0345) > 1e-9 {
		t.Errorf("bin 3 current %+v", b.IBusA)
	}
}

func TestAutoBin(t *testing.T) {
	for _, c := range []struct {
		window, interval, want time.Duration
	}{
		{5 * time.Second, 100 * time.Millisecond, 500 * time.Millisecond},
		{time.Minute, 100 * time.Millisecond, 500 * time.Millisecond},
		{10 * time.Minute, 100 * time.Millisecond, 5 * time.Second},
		{time.Hour, 100 * time.Millisecond, 30 * time.Second},
		{time.Minute, time.Second, 5 * time.Second},
		{48 * time.Hour, 100 * time.Millisecond, 1800 * time.Second},
	} {
		if got := autoBin(c.window, c.interval); got != c.want {
			t.Errorf("autoBin(%v, %v) = %v, want %v", c.window, c.interval, got, c.want)
		}
	}
}
