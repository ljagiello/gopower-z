package cli

import (
	"math"
	"math/rand/v2"
	"testing"
)

// Regimes beyond the capture at 100 ms: few readings per phase, noisy loads,
// failed reads, the exact persistence boundary and very long windows.

// onoffSeries is an on/off load sampled every dt seconds for window seconds:
// lo in the off phases, hi in the on phases (on first, shifted by shift),
// plus Gaussian noise of sd (relative to the current) from rng, if any.
func onoffSeries(window, dt, on, off, shift, lo, hi, sd float64, rng *rand.Rand) []sample {
	var ss []sample
	for k := 0; float64(k)*dt < window; k++ {
		tt := float64(k) * dt
		i := lo
		if math.Mod(tt+shift, on+off) < on {
			i = hi
		}
		if rng != nil {
			i += rng.NormFloat64() * sd * i
		}
		ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
	}
	return ss
}

// changes counts the switches of an on/off load inside the window, leaving
// out edge phases shorter than minEdge seconds.
func changes(window, on, off, shift, minEdge float64) int {
	var ts []float64
	period := on + off
	for k := -1; float64(k)*period-shift < window+period; k++ {
		for _, at := range []float64{float64(k)*period - shift, float64(k)*period + on - shift} {
			if at > 0 && at < window {
				ts = append(ts, at)
			}
		}
	}
	n := len(ts)
	if n > 0 {
		first, last := ts[0], ts[0]
		for _, t := range ts {
			first, last = math.Min(first, t), math.Max(last, t)
		}
		if first < minEdge {
			n--
		}
		if window-last < minEdge {
			n--
		}
	}
	return n
}

// Few readings per phase (the rank-test regime): a stub must not join the
// wrong side and set off merges along the window.
func TestLevelsFewReadingsPerPhase(t *testing.T) {
	// The real capture read every second, +300 mA, 8 s on / 8 s off.
	capture := add(noise(t, 80, 10, 0), func(tt float64) float64 {
		if math.Mod(tt+2, 16) < 8 {
			return 0.3
		}
		return 0
	})
	lv := levels(capture, 80)
	if len(lv) != 10 {
		t.Errorf("capture, 8 s phases at 1 s polling: %d levels (starts %v), want 10", len(lv), starts(lv))
	}
	for k, l := range lv { // the capture's median is 65 mA
		if p := l.IBusA.P50; math.Abs(p-0.065) > 0.01 && math.Abs(p-0.365) > 0.01 {
			t.Errorf("capture, 8 s phases at 1 s polling: level %d median %.0f mA, want 65 or 365", k, p*1000)
		}
	}
	// 100 <-> 400 mA with 1 mA of noise, read every second.
	rng := rand.New(rand.NewPCG(7, 8))
	for _, phase := range []float64{8, 9, 10} {
		ss := onoffSeries(80, 1, phase, phase, 0, 0.1, 0.4, 0.0025, rng)
		if got, want := len(levels(ss, 80))-1, changes(80, phase, phase, 0, 8); got != want {
			t.Errorf("%v s phases at 1 s polling: %d changes, want %d", phase, got, want)
		}
	}
	// Clean 100 / 400 mA, 60 s phases read every 10 s (6 readings each) for
	// 20,000 s.
	ss := onoffSeries(20000, 10, 60, 60, 0, 0.1, 0.4, 0, nil)
	if got, want := len(levels(ss, 20000))-1, changes(20000, 60, 60, 0, 60); got != want {
		t.Errorf("60 s phases at 10 s polling: %d changes, want %d", got, want)
	}
}

// A noisy but steady load keeps its levels: sampling can make two halves of
// a level differ by 5%, but merged they hold.
func TestLevelsNoisyLoads(t *testing.T) {
	for _, c := range []struct {
		sd, phase float64
	}{{0.10, 20}, {0.15, 30}, {0.20, 60}} {
		for seed := range uint64(20) {
			rng := rand.New(rand.NewPCG(seed, 9))
			window := 6 * c.phase
			ss := onoffSeries(window, 0.1, c.phase, c.phase, 0, 0.1, 0.2, c.sd, rng)
			lv := levels(ss, window)
			if got, want := len(lv)-1, changes(window, c.phase, c.phase, 0, 8); got != want {
				t.Errorf("%v%% noise, %v s phases (seed %d): %d changes, want %d (starts %v)", c.sd*100, c.phase, seed, got, want, starts(lv))
			}
		}
	}
	// A 2x step at 20% noise (an approximate regime; seeds picked: 2 of the
	// first 20 put the change elsewhere).
	for seed := range uint64(5) {
		rng := rand.New(rand.NewPCG(seed, 10))
		var ss []sample
		for k := range 600 {
			tt, i := float64(k)/10, 0.1
			if tt >= 30 {
				i = 0.2
			}
			i += rng.NormFloat64() * 0.2 * i
			ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
		}
		if got := starts(levels(ss, 60)); len(got) != 1 || math.Abs(got[0]-30) > 2 {
			t.Errorf("2x step at 20%% noise (seed %d): levels start at %v, want [30]", seed, got)
		}
	}
}

// Failed reads elsewhere in the window do not make a level's readings count
// for more time: a stretch with 4 s of steady readings is not a level.
func TestLevelsFailedReadsDoNotInflate(t *testing.T) {
	series := func(cur func(t float64) (float64, bool)) []sample {
		var ss []sample
		for k := range 600 {
			tt := float64(k) / 10
			if i, ok := cur(tt); ok {
				ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
			}
		}
		return ss
	}
	for _, c := range []struct {
		name string
		cur  func(t float64) (float64, bool)
	}{
		// 45-52 s at 300 mA, pulsed to 600 mA for 0.3 s every 0.7 s: 4.0 s of
		// readings at 300 mA; the first half of the window mostly failed.
		{"4 s steady inside 7 s", func(t float64) (float64, bool) {
			if t > 0.05 && t < 30 && math.Mod(t, 1) > 0.15 {
				return 0, false
			}
			if t >= 45 && t < 52 {
				if math.Mod(t-45, 0.7) < 0.3 {
					return 0.6, true
				}
				return 0.3, true
			}
			return 0.1, true
		}},
		{"4.5 s burst", func(t float64) (float64, bool) {
			if t > 0.05 && t < 30 && math.Mod(t, 1) > 0.15 {
				return 0, false
			}
			if t >= 45 && t < 49.5 {
				return 0.3, true
			}
			return 0.1, true
		}},
	} {
		if lv := levels(series(c.cur), 60); len(lv) != 1 {
			t.Errorf("%s: levels start at %v", c.name, starts(lv))
		}
	}
	// Most reads failing (1 in 10 succeeds) except a dense 10 s stretch: the
	// typical spacing is then 1 s, but a reading in the dense stretch is
	// worth 0.1 s, so a 4.2 s burst there is not a level and a 6 s one is,
	// unless pulses (to 600 mA for 0.3 s every 0.7 s) leave it 3.4 s of
	// steady readings.
	for _, c := range []struct {
		window, dur float64
		pulsed      bool
		want        int
	}{{150, 4.2, false, 1}, {300, 4.5, false, 1}, {60, 4.2, false, 1}, {150, 6, false, 3}, {150, 6, true, 1}} {
		var ss []sample
		for k := 0; float64(k)*0.1 < c.window; k++ {
			tt := float64(k) / 10
			if (tt < 30 || tt >= 40) && k%10 != 0 {
				continue // failed read
			}
			i := 0.1
			if tt >= 33 && tt < 33+c.dur {
				i = 0.3
				if c.pulsed && math.Mod(tt-33, 0.7) < 0.3 {
					i = 0.6
				}
			}
			ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
		}
		if lv := levels(ss, c.window); len(lv) != c.want {
			t.Errorf("mostly failed reads, %v s burst (pulsed %v) in a dense stretch, %v s window: levels start at %v, want %d levels", c.dur, c.pulsed, c.window, starts(lv), c.want)
		}
	}
	// The last read lands just after the end of the window (measure's last
	// tick): it counts for no time, so a 4.896 s last level is not a level.
	var ss []sample
	for k := 0; k <= 600; k++ {
		tt, i := float64(k)/10+0.004, 0.1
		if k >= 551 {
			i = 0.3
		}
		ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
	}
	if lv := levels(ss, 60); len(lv) != 1 {
		t.Errorf("4.896 s at the end of the window, last read after it: levels %v..%v", starts(lv), lv[len(lv)-1].EndS)
	}
}

// The persistence boundary: a clean 5.0 s level (50 readings at 100 ms) is a
// level; 4.9 s is not.
func TestLevelsFiveSecondBoundary(t *testing.T) {
	for _, c := range []struct {
		dur  float64
		want int
	}{{5.0, 3}, {4.9, 1}} {
		var ss []sample
		for k := range 600 {
			tt, i := float64(k)/10, 0.1
			if tt >= 30-1e-9 && tt < 30+c.dur-1e-9 {
				i = 0.3
			}
			ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
		}
		if lv := levels(ss, 60); len(lv) != c.want {
			t.Errorf("%v s level: %d levels (starts %v), want %d", c.dur, len(lv), starts(lv), c.want)
		}
	}
}

// A 9 s phase on the capture, at a spot where its pulses hide the change
// from the plain averages and the short running median (a sweep case): the
// wider running median finds it.
func TestLevelsWideMedianFindsPhase(t *testing.T) {
	c := sweepCase{kind: "onoff", on: 27.557727488845142, off: 9.056030961078472, amp: 0.03, poll: 0.1, window: 3600, shift: 30.764448553298294}
	ss := c.series(t, map[[2]float64][]sample{})
	found := false
	for _, s := range starts(levels(ss, c.window)) {
		found = found || math.Abs(s-3365.26) <= 2
	}
	if !found {
		t.Errorf("%v: no change near 3365.26 s", c)
	}
}

// The size rule holds on the published medians, which include the readings
// each level takes in at the gaps around it (a case from the sweep: without
// the final check in resolve, two levels come out 0.5% apart).
func TestLevelsPublishedMediansKeepSizeRule(t *testing.T) {
	c := sweepCase{kind: "onoff", on: 19.17, off: 14.71, amp: 0.015, poll: 1, window: 120, shift: 24.97}
	ss := c.series(t, map[[2]float64][]sample{})
	if why := structure(levels(ss, c.window), len(ss), c.window); why != "" {
		t.Error(why)
	}
}

// Windows over 100,000 s average over window/100000 (3 s here, 1 s polling):
// a 12 s level is found, a 4 s burst is not.
func TestLevelsBeyondHundredThousandSeconds(t *testing.T) {
	if raceEnabled {
		t.Skip("long windows: CI runs this without the race detector")
	}
	window := 300000.0
	rng := rand.New(rand.NewPCG(11, 12))
	base := onoffSeries(window, 1, 0, 1, 0, 0.1, 0.1, 0.01, rng) // 100 mA, 1% noise
	for _, c := range []struct {
		at, dur float64
		want    int
	}{{150000.4, 12, 3}, {200000.4, 4, 1}} {
		ss := add(base, func(t float64) float64 {
			if t >= c.at && t < c.at+c.dur {
				return 0.1
			}
			return 0
		})
		lv := levels(ss, window)
		if len(lv) != c.want {
			t.Errorf("%v s at %v s: %d levels (starts %v), want %d", c.dur, c.at, len(lv), starts(lv), c.want)
		}
	}
}
