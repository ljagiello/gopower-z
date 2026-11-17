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

func step(at, from, to float64) func(float64) float64 {
	return func(t float64) float64 {
		if t >= at {
			return to
		}
		return from
	}
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// ------------------------------------------------------------- statistics

func TestDescribe(t *testing.T) {
	// 1..21 shuffled: percentiles must come from sorted values, with
	// interpolation, and stdev is the sample (n-1) standard deviation.
	xs := []float64{11, 3, 19, 7, 1, 15, 21, 5, 13, 9, 17, 2, 20, 4, 12, 6, 18, 8, 14, 10, 16}
	s := describe(xs, make([]float64, len(xs)), 0, 6)
	if s.Min != 1 || s.Max != 21 || s.Mean != 11 || s.Last != 16 || s.P5 != 2 || s.P50 != 11 || s.P95 != 20 ||
		s.Stdev != round(math.Sqrt(38.5), 6) || s.CI95 != nil {
		t.Errorf("1..21: %+v", s)
	}
	s = describe([]float64{4, 1, 3, 2}, make([]float64, 4), 0, 6)
	if s.Min != 1 || s.Max != 4 || s.Mean != 2.5 || s.Last != 2 || s.P5 != 1.15 || s.P50 != 2.5 || s.P95 != 3.85 ||
		s.Stdev != round(math.Sqrt(5.0/3), 6) {
		t.Errorf("[4 1 3 2]: %+v", s)
	}
}

// ------------------------------------------------------------- shifts: steps

// The case that motivated bins and shifts: a charger switching on through a
// window turns 134 mA into 441 mA. min/mean/max alone read as a noisy mean;
// the shift list must name the step, once, at the right time, at any bin
// width, with from/to measured from the step rather than from bin edges.
func TestShiftChargeStep(t *testing.T) {
	ss := series(60, 0.1, 0.003, 1, step(42.3, 0.134, 0.441))
	for _, binS := range []float64{0.5, 1, 2, 5} {
		a := analyze(ss, 60, binS)
		if len(a.Shifts) != 1 {
			t.Fatalf("bin %v: want one shift, got %+v", binS, a.Shifts)
		}
		s := a.Shifts[0]
		if s.Field != "ibus_a" || s.Kind != kindStep || s.DurationS != 0 || !near(s.TS, 42.3, 0.11) ||
			!near(s.From, 0.134, 0.001) || !near(s.To, 0.441, 0.001) {
			t.Errorf("bin %v: shift %+v, want an ibus_a step 0.134 -> 0.441 at 42.3 s", binS, s)
		}
	}
}

// The threshold must follow the noise: at 20 mA and 50 mA of noise a floor
// of 5 mA alone would report shifts everywhere.
func TestShiftNoneOnNoise(t *testing.T) {
	for _, noise := range []float64{0.004, 0.02, 0.05} {
		for seed := range uint64(20) {
			ss := series(60, 0.1, noise, seed, func(float64) float64 { return 0.3 })
			if a := analyze(ss, 60, 0.5); len(a.Shifts) != 0 {
				t.Fatalf("noise %v seed %d: shifts %+v", noise, seed, a.Shifts)
			}
		}
	}
}

func TestShiftThreeLevels(t *testing.T) {
	ss := series(60, 0.1, 0.002, 2, func(t float64) float64 {
		switch {
		case t < 20:
			return 0.1
		case t < 40:
			return 0.3
		}
		return 0.2
	})
	a := analyze(ss, 60, 0.5)
	if len(a.Shifts) != 2 {
		t.Fatalf("shifts %+v", a.Shifts)
	}
	s1, s2 := a.Shifts[0], a.Shifts[1]
	if !near(s1.TS, 20, 0.11) || !near(s1.From, 0.1, 0.001) || !near(s1.To, 0.3, 0.001) ||
		!near(s2.TS, 40, 0.11) || !near(s2.From, 0.3, 0.001) || !near(s2.To, 0.2, 0.001) {
		t.Errorf("shifts %+v, want 0.1 -> 0.3 at 20 s and 0.3 -> 0.2 at 40 s", a.Shifts)
	}
}

// A step just before a run of failed reads is located from the last
// non-empty bin, not placed after the gap.
func TestShiftStepBeforeGap(t *testing.T) {
	for _, gapEnd := range []float64{13.0, 14.0} {
		var ss []sample
		for _, s := range series(30, 0.1, 0.002, 2, step(12.2, 0.134, 0.441)) {
			if s.T < 12.5 || s.T >= gapEnd {
				ss = append(ss, s)
			}
		}
		a := analyze(ss, 30, 0.5)
		if len(a.Shifts) != 1 || !near(a.Shifts[0].TS, 12.2, 0.11) || !near(a.Shifts[0].From, 0.134, 0.001) {
			t.Errorf("gap to %v s: shifts %+v, want one step at 12.2 s from 0.134", gapEnd, a.Shifts)
		}
	}
}

// A USB device dropping off the bus shows as D+ falling, a PD renegotiation
// as VBUS rising; the list is in time order across fields.
func TestShiftOrderAcrossFields(t *testing.T) {
	ss := series(30, 0.1, 0.002, 5, step(15, 0.09, 0.2))
	for k := range ss {
		if ss[k].T >= 20 {
			ss[k].V = 9.02
		}
		if ss[k].T >= 10 {
			ss[k].DP = 0.01
		}
	}
	a := analyze(ss, 30, 0.5)
	want := []struct {
		field string
		at    float64
	}{{"dp_v", 10}, {"ibus_a", 15}, {"vbus_v", 20}}
	if len(a.Shifts) != len(want) {
		t.Fatalf("shifts %+v", a.Shifts)
	}
	for k, w := range want {
		if a.Shifts[k].Field != w.field || !near(a.Shifts[k].TS, w.at, 0.11) {
			t.Errorf("shift %d = %+v, want %s at %v s", k, a.Shifts[k], w.field, w.at)
		}
	}
}

// ------------------------------------------------------------- shifts: bursts

// A burst is not a lasting level however it falls against the bin edges:
// one that straddles an edge raises two bins, and must still be dropped.
func TestShiftIgnoresShortBursts(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for k := range 200 {
		length := 0.05 + 0.9*r.Float64() // under two 0.5 s bins
		at := 5 + 20*r.Float64()
		ss := series(30, 0.05, 0.002, uint64(k), func(t float64) float64 {
			if t >= at && t < at+length {
				return 0.35
			}
			return 0.09
		})
		if a := analyze(ss, 30, 0.5); len(a.Shifts) != 0 {
			t.Fatalf("burst of %.2f s at %.2f s reported as %+v", length, at, a.Shifts)
		}
	}
	// Several bursts in one window: once a level has absorbed one burst, the
	// next must still be recognised as a burst (levels are compared by
	// median, which a burst does not move).
	for name, c := range map[string]struct {
		dt, binS float64
		on       func(t float64) bool
	}{
		"radio every 2.3 s":                  {0.05, 0.5, func(t float64) bool { return math.Mod(t, 2.3) < 0.15 }},
		"0.3 s every 5 s across 1 s edges":   {0.05, 1, func(t float64) bool { return math.Mod(t+0.15, 5) < 0.3 }},
		"two 0.9 s bursts":                   {0.05, 0.5, func(t float64) bool { return (t >= 10 && t < 10.9) || (t >= 20 && t < 20.9) }},
		"0.3-0.9 s bursts of varying length": {0.05, 0.5, func(t float64) bool { return math.Mod(t, 3.1) < 0.3+0.6*math.Abs(math.Sin(math.Floor(t/3.1))) }},
	} {
		ss := series(60, c.dt, 0.002, 9, func(t float64) float64 {
			if c.on(t) {
				return 0.35
			}
			return 0.09
		})
		if a := analyze(ss, 60, c.binS); len(a.Shifts) != 0 {
			t.Errorf("%s reported as %+v", name, a.Shifts)
		}
	}
}

// A burst does not hide or distort a real step that follows it.
func TestShiftBurstThenStep(t *testing.T) {
	ss := series(60, 0.05, 0.002, 4, func(t float64) float64 {
		switch {
		case t >= 25 && t < 25.9:
			return 0.35
		case t >= 30:
			return 0.105
		}
		return 0.09
	})
	a := analyze(ss, 60, 0.5)
	if len(a.Shifts) != 1 || a.Shifts[0].Kind != kindStep || !near(a.Shifts[0].TS, 30, 0.11) ||
		!near(a.Shifts[0].From, 0.09, 0.001) || !near(a.Shifts[0].To, 0.105, 0.001) {
		t.Errorf("shifts %+v, want one step 0.090 -> 0.105 at 30 s", a.Shifts)
	}
}

// Random mixtures of steps, bursts, ramps and gaps: the analysis never
// panics, always marshals, keeps shifts in order and inside the window, and
// never lists a step smaller than the floor (a level change that is not
// one).
func TestShiftRandomMixtures(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 43))
	for k := range 500 {
		window := 10 + 110*r.Float64()
		binS := []float64{0.5, 1, 2}[r.IntN(3)]
		type feature struct{ at, length, height float64 }
		var feats []feature
		for range r.IntN(6) {
			feats = append(feats, feature{window * r.Float64(), 3 * r.Float64(), 0.3*r.Float64() - 0.1})
		}
		level := 0.05 + 0.5*r.Float64()
		slope := 0.0
		if r.IntN(3) == 0 {
			slope = (r.Float64() - 0.5) * 0.02
		}
		gapAt, gapLen := window*r.Float64(), 2*r.Float64()
		var ss []sample
		for _, s := range series(window, 0.05+0.1*r.Float64(), 0.001+0.02*r.Float64(), uint64(k), func(t float64) float64 {
			v := level + slope*t
			for _, f := range feats {
				if t >= f.at && (f.length > 2 || t < f.at+f.length) {
					v += f.height
				}
			}
			return v
		}) {
			if s.T < gapAt || s.T >= gapAt+gapLen {
				ss = append(ss, s)
			}
		}
		a := analyze(ss, window, binS)
		if _, err := json.Marshal(measureResult{Stats: a.Stats, BinS: a.BinS, Bins: a.Bins, Shifts: a.Shifts}); err != nil {
			t.Fatalf("case %d: %v", k, err)
		}
		for i, s := range a.Shifts {
			// t_s and duration_s are rounded to milliseconds.
			if s.TS < 0 || s.TS > window+5e-4 || s.DurationS < 0 || s.TS+s.DurationS > window+1e-3 ||
				(i > 0 && s.TS < a.Shifts[i-1].TS) {
				t.Fatalf("case %d: shift %d out of place: %+v", k, i, a.Shifts)
			}
			if s.Field == "ibus_a" && s.Kind == kindStep && math.Abs(s.To-s.From) <= 0.005 {
				t.Fatalf("case %d: a step smaller than the 5 mA floor: %+v", k, s)
			}
		}
	}
}

// Long periodic bursts are real level changes; each field keeps its first 32
// and says the list was cut, without hiding other fields' shifts.
func TestShiftCapPerField(t *testing.T) {
	ss := series(600, 0.1, 0.001, 4, func(t float64) float64 {
		if math.Mod(t, 6) >= 3 {
			return 0.4
		}
		return 0.1
	})
	for k := range ss {
		if ss[k].T >= 500 {
			ss[k].V = 9.02
		}
	}
	a := analyze(ss, 600, 1)
	n := map[string]int{}
	for _, s := range a.Shifts {
		n[s.Field]++
	}
	if n["ibus_a"] != 32 || n["vbus_v"] != 1 || !a.ShiftsTruncated {
		t.Fatalf("per-field counts %v, truncated %v", n, a.ShiftsTruncated)
	}
	if first := a.Shifts[0]; first.Field != "ibus_a" || !near(first.TS, 3, 0.11) {
		t.Errorf("the earliest shifts must be kept, first is %+v", first)
	}
	for k := 1; k < len(a.Shifts); k++ {
		if a.Shifts[k].TS < a.Shifts[k-1].TS {
			t.Fatalf("shifts not in time order at %d", k)
		}
	}
	if b := analyze(series(60, 0.1, 0.002, 1, step(30, 0.1, 0.4)), 60, 0.5); b.ShiftsTruncated {
		t.Error("truncated set without truncation")
	}
}

// ------------------------------------------------------------- shifts: ramps

// A drift is one ramp, not a staircase of steps; a staircase of flat levels
// stays a list of steps.
func TestShiftRamps(t *testing.T) {
	cases := []struct {
		name           string
		window         float64
		binS           float64
		cur            func(float64) float64
		at             float64
		minLen, maxLen float64
		from, to       float64
	}{
		{"linear ramp", 60, 0.5, func(t float64) float64 { return 0.1 + 0.25*t/60 }, 0, 54, 60, 0.1, 0.35},
		// After about 30 s the decay is slower than the bin noise, so the
		// ramp may end there and the tail count as the level it settles at.
		{"exponential decay", 60, 0.5, func(t float64) float64 { return 0.1 + 0.9*math.Exp(-t/10) }, 0, 25, 60, 0.98, 0.1},
		{"CV taper", 600, 5, func(t float64) float64 { return 0.5 + 1.5*math.Exp(-t/200) }, 0, 450, 600, 1.98, 0.59},
		{"flat, ramp, flat", 60, 0.5, func(t float64) float64 { return 0.1 + 0.3*math.Min(1, math.Max(0, (t-20)/20)) }, 21, 14, 22, 0.1, 0.4},
	}
	for _, c := range cases {
		a := analyze(series(c.window, 0.1, 0.003, 3, c.cur), c.window, c.binS)
		if len(a.Shifts) != 1 {
			t.Errorf("%s: want one ramp, got %+v", c.name, a.Shifts)
			continue
		}
		s := a.Shifts[0]
		if s.Kind != kindRamp || !near(s.TS, c.at, 0.1*c.window) || s.DurationS < c.minLen || s.DurationS > c.maxLen ||
			!near(s.From, c.from, 0.03) || !near(s.To, c.to, 0.03) {
			t.Errorf("%s: %+v, want a ramp from %.2f to %.2f lasting %v-%v s from %v s", c.name, s, c.from, c.to, c.minLen, c.maxLen, c.at)
		}
	}
	stairs := analyze(series(60, 0.1, 0.003, 7, func(t float64) float64 { return 0.1 + 0.1*math.Floor(t/15) }), 60, 0.5)
	if len(stairs.Shifts) != 3 {
		t.Fatalf("staircase: %+v", stairs.Shifts)
	}
	for _, s := range stairs.Shifts {
		if s.Kind != kindStep {
			t.Errorf("staircase reported a %s: %+v", s.Kind, s)
		}
	}
}

// Steps are not mistaken for ramps, and no shift appears from noise alone,
// across many seeds and step positions.
func TestShiftSweep(t *testing.T) {
	for seed := range uint64(60) {
		at := 30 + float64(seed%60) + float64(seed%10)/10
		a := analyze(series(120, 0.1, 0.005, seed, step(at, 0.09, 0.105)), 120, float64(1+seed%3))
		if len(a.Shifts) != 1 || a.Shifts[0].Kind != kindStep || !near(a.Shifts[0].TS, at, 1) ||
			!near(a.Shifts[0].From, 0.09, 0.0015) || !near(a.Shifts[0].To, 0.105, 0.0015) {
			t.Fatalf("seed %d: 15 mA step at %v s reported as %+v", seed, at, a.Shifts)
		}
	}
}

// ------------------------------------------------------------- ci95

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
		a := analyze(series(60, dt, 0, 0, at), 60, 1)
		if a.Stats.IBusA.CI95 == nil {
			t.Fatalf("no ci95 at dt %v", dt)
		}
		return *a.Stats.IBusA.CI95
	}
	slow, fast := ci(0.05), ci(0.025)
	if fast < 0.9*slow {
		t.Errorf("ci95 shrank from %v to %v just by polling twice as fast", slow, fast)
	}
	// The mean of 600 independent values with sd 3 mA has a 95% interval of
	// about 0.24 mA.
	if slow < 0.00012 || slow > 0.0005 {
		t.Errorf("ci95 %v A, want about 0.24 mA", slow)
	}
}

// When the load wanders (an AR(1) process correlated over 0.5 s, sampled by
// a 10 SPS meter), the interval must still cover the true mean about 95% of
// the time over a 60 s window.
func TestCI95Coverage(t *testing.T) {
	covered, reported := 0, 0
	for seed := range uint64(300) {
		r := rand.New(rand.NewPCG(seed, 99))
		phi, x := math.Exp(-0.1/0.5), 0.0
		var ss []sample
		for k := range 600 {
			x = phi*x + math.Sqrt(1-phi*phi)*r.NormFloat64()*0.01
			ss = append(ss, sample{T: float64(k) * 0.1, I: 0.1 + x, V: 5, P: 0.5, DP: 3})
		}
		if st := analyze(ss, 60, 1).Stats.IBusA; st.CI95 != nil {
			reported++
			if math.Abs(st.Mean-0.1) <= *st.CI95 {
				covered++
			}
		}
	}
	if reported < 270 || float64(covered)/float64(reported) < 0.88 {
		t.Errorf("reported %d/300, coverage %.1f%%", reported, 100*float64(covered)/float64(reported))
	}
}

func TestCI95Gates(t *testing.T) {
	white := func(window float64, keep func(t float64) bool) *float64 {
		var ss []sample
		for _, s := range series(window, 0.1, 0.01, 11, func(float64) float64 { return 0.1 }) {
			if keep(s.T) {
				ss = append(ss, s)
			}
		}
		return analyze(ss, window, 1).Stats.IBusA.CI95
	}
	all := func(float64) bool { return true }
	if white(29.9, all) != nil || white(30, all) == nil {
		t.Error("ci95 needs batches of at least 3 s (a 30 s window)")
	}
	// 60 s in 6 s batches: samples in 7 batches are too few, 8 are enough.
	batches := func(n int) func(float64) bool { return func(t float64) bool { return int(t/6) < n } }
	if white(60, batches(7)) != nil || white(60, batches(8)) == nil {
		t.Error("ci95 needs samples in at least 8 of 10 batches")
	}
	// A level that changed during the window has no single mean to bound.
	if a := analyze(series(60, 0.1, 0.002, 1, step(30, 0.1, 0.2)), 60, 1); a.Stats.IBusA.CI95 != nil {
		t.Errorf("ci95 %v across a step", *a.Stats.IBusA.CI95)
	}
}

// Batches are equal stretches of time, not equal counts of samples: with
// dense polling early and sparse polling late, equal-count batches would put
// nearly every batch in the steady first half and report a tiny interval.
func TestCI95EqualTimeBatches(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	var ss []sample
	for k := range 3000 { // 0-30 s every 10 ms, steady
		ss = append(ss, sample{T: float64(k) * 0.01, I: 0.1, V: 5, DP: 3})
	}
	for k := range 60 { // 30-60 s every 0.5 s, noisy around the same mean
		ss = append(ss, sample{T: 30 + float64(k)*0.5, I: 0.1 + r.NormFloat64()*0.02, V: 5, DP: 3})
	}
	st := analyze(ss, 60, 1).Stats.IBusA
	if st.CI95 == nil || *st.CI95 < 0.002 {
		t.Errorf("ci95 %v, want the noisy half to count as half the window (about 3 mA)", st.CI95)
	}
}

// ------------------------------------------------------------- bins and edges

func TestBins(t *testing.T) {
	ss := series(10.25, 0.1, 0, 0, func(t float64) float64 { return t / 100 })
	a := analyze(ss, 10.25, 1)
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

// measure caps the window at --duration, so the last read can land at or
// after the end of the window: it belongs to the last bin and batch.
func TestSamplesAtWindowEnd(t *testing.T) {
	ss := series(30, 0.1, 0.002, 1, func(float64) float64 { return 0.1 })
	ss = append(ss, sample{T: 30, I: 0.1, V: 5.076, DP: 3}, sample{T: 30.003, I: 0.1, V: 5.076, DP: 3})
	a := analyze(ss, 30, 1)
	n := 0
	for _, b := range a.Bins {
		n += b.Samples
	}
	if len(a.Bins) != 30 || n != len(ss) || a.Bins[29].Samples != 12 || a.Stats.IBusA.CI95 == nil {
		t.Errorf("%d bins holding %d of %d samples (last bin %d), ci95 %v", len(a.Bins), n, len(ss), a.Bins[29].Samples, a.Stats.IBusA.CI95)
	}
}

// Windows cut short (an agent harness sends SIGTERM) must still marshal:
// encoding/json refuses NaN and Inf, which would lose the whole result.
func TestAnalyzeDegenerateWindows(t *testing.T) {
	one := []sample{{T: 0, V: 5, I: 0.1, P: 0.5, DP: 3, DM: 0}}
	two := []sample{one[0], {T: 0.1, V: 5, I: 0.1, P: 0.5, DP: 3, DM: 0}}
	constant := series(30, 0.1, 0, 0, func(float64) float64 { return 0.25 })
	for name, c := range map[string]struct {
		ss     []sample
		window float64
		binS   float64
	}{
		"none": {nil, 0, 0.5}, "one": {one, 0.05, 0.5}, "one at zero length": {one, 0, 0.5}, "two": {two, 0.2, 0.5},
		"constant": {constant, 30, 0.5}, "single bin": {constant, 30, 60},
	} {
		a := analyze(c.ss, c.window, c.binS)
		if _, err := json.Marshal(measureResult{Stats: a.Stats, BinS: a.BinS, Bins: a.Bins, Shifts: a.Shifts}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.Bins == nil || a.Shifts == nil {
			t.Errorf("%s: bins/shifts must be [] not null", name)
		}
		if len(c.ss) > 0 && a.Stats.IBusA == nil {
			t.Errorf("%s: no current stats", name)
		}
		if c.window < 30 && a.Stats.IBusA != nil && a.Stats.IBusA.CI95 != nil {
			t.Errorf("%s: ci95 from a window under 30 s", name)
		}
	}
	a := analyze(constant, 30, 0.5)
	if s := a.Stats.IBusA; s.Stdev != 0 || s.CI95 == nil || *s.CI95 != 0 || s.P5 != 0.25 || s.P95 != 0.25 || len(a.Shifts) != 0 {
		t.Errorf("constant series: stats %+v, shifts %+v", s, a.Shifts)
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
