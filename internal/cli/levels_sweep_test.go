package cli

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A seeded sweep of levels over loads, amplitudes, polling, windows and
// noise: the real pulsing-load capture (tiled, lifted 100 mA so its own slow
// wander stays under the 5% size rule) or Gaussian noise. Expectations follow
// the definition with margins:
//
//   - a phase is clear when it lasts 8 s and holds 12 readings (or 8
//     readings of a +300 mA step, which the rank test separates), short when
//     it lasts 4 s or less, ambiguous otherwise;
//   - every switch next to a clear phase must be found, within a reading or
//     two (or a pulse); a switch next to an ambiguous edge phase may be; one
//     inside a short edge phase must not; a case with an ambiguous interior
//     phase is skipped;
//   - fast switching (every phase 4 s or less, away from 50% duty) is one level;
//   - interior levels have a median within a third of the step of one of the
//     two currents;
//   - on every case, asserted or not, neighbouring published medians differ
//     by the size rule.
//
// Levels are documented as reliable at the default 100 ms polling for loads
// with up to about 5% noise or regular pulses (claimed); elsewhere (slower
// polling, noisier loads) as approximate: there the sweep counts failures but
// does not fail on them. CI runs the claimed cases of the full sweep
// (POWERZ_LEVELS_SWEEP=2000) in a step of its own without the race detector;
// POWERZ_LEVELS_SWEEP_ALL=1 adds the approximate ones; the regular test run
// asserts a sample of claimed cases.
const sweepSample = 30 // asserted cases in the regular run

type sweepCase struct {
	kind         string // onoff, fast, burst, step
	gauss        float64
	on, off, amp float64
	at, dur      float64
	poll, window float64
	shift        float64
	seed         uint64
}

// claimed reports whether the case is in the regime the docs call reliable.
func (c sweepCase) claimed() bool { return c.poll <= 0.1 && c.gauss <= 0.05 }

func (c sweepCase) String() string {
	src := "capture"
	if c.gauss > 0 {
		src = fmt.Sprintf("%.0f%% Gaussian", c.gauss*100)
	}
	switch c.kind {
	case "onoff", "fast":
		return fmt.Sprintf("%s %.2f/%.2f s %+.0f mA, every %v s, %v s window, shift %.2f, %s", c.kind, c.on, c.off, c.amp*1000, c.poll, c.window, c.shift, src)
	case "burst":
		return fmt.Sprintf("burst %.2f s %+.0f mA at %.2f s, every %v s, %v s window, %s", c.dur, c.amp*1000, c.at, c.poll, c.window, src)
	}
	return fmt.Sprintf("step %+.0f mA at %.2f s, every %v s, %v s window, %s", c.amp*1000, c.at, c.poll, c.window, src)
}

// offSeam reports whether [a, b] stays 3 s clear of the tiled capture's seams.
func offSeam(a, b float64) bool {
	const tile = 599.999
	for k := math.Floor(a/tile) * tile; k <= b+3; k += tile {
		if k > 0 && k >= a-3 && k <= b+3 {
			return false
		}
	}
	return true
}

func randomCase(rng *rand.Rand) sweepCase {
	pick := func(xs ...float64) float64 { return xs[rng.IntN(len(xs))] }
	c := sweepCase{poll: pick(0.1, 0.1, 0.5, 1, 3), window: pick(120, 600, 3600), seed: rng.Uint64()}
	if rng.IntN(3) == 0 {
		c.gauss = pick(0.01, 0.05, 0.1)
	}
	switch rng.IntN(4) {
	case 0:
		c.kind = "onoff"
		if c.poll >= 3 {
			c.on, c.off = 36+rng.Float64()*54, 36+rng.Float64()*54
			c.window = pick(600, 3600, 20000)
		} else {
			c.on, c.off = 3+rng.Float64()*37, 3+rng.Float64()*37
		}
		c.amp = pick(0.015, 0.03, 0.06, 0.3)
		c.shift = rng.Float64() * (c.on + c.off)
	case 1:
		c.kind = "fast"
		c.poll = 0.1
		for {
			c.on, c.off = 1+rng.Float64()*3, 1+rng.Float64()*3
			// away from 50% duty, counting the capture's own pulses (a quarter
			// of its time), which add to the off phases
			if math.Abs(c.on-c.off)/(c.on+c.off) >= 0.4 {
				break
			}
		}
		c.amp = pick(0.06, 0.3)
		c.window = pick(120, 600)
		c.shift = rng.Float64() * (c.on + c.off)
	case 2:
		c.kind = "burst"
		c.window = pick(600, 3600, 20000, 100000)
		if c.window == 100000 && c.poll < 1 {
			c.poll = 1
		}
		long := 6 + rng.Float64()*24
		if c.poll >= 3 {
			long = 36 + rng.Float64()*54
		}
		c.dur = pick(0.5+rng.Float64()*4, long)
		c.amp = pick(0.03, 0.1, 0.3)
		c.at = 10 + rng.Float64()*(c.window-20-c.dur)
	default:
		c.kind = "step"
		c.amp = pick(-0.03, -0.015, 0.015, 0.03, 0.3)
		c.at = 10 + rng.Float64()*(c.window-20)
	}
	return c
}

// series builds the case's readings.
func (c sweepCase) series(t *testing.T, cache map[[2]float64][]sample) []sample {
	var base []sample
	if c.gauss > 0 {
		rng := rand.New(rand.NewPCG(c.seed, 1))
		for k := 0; float64(k)*c.poll < c.window; k++ {
			i := 0.1655 * (1 + rng.NormFloat64()*c.gauss)
			base = append(base, sample{T: float64(k) * c.poll, V: 5, I: i, P: 5 * i})
		}
	} else {
		key := [2]float64{c.window, c.poll}
		if base = cache[key]; base == nil {
			base = add(noise(t, c.window, int(math.Round(c.poll*10)), 0), func(float64) float64 { return 0.1 })
			cache[key] = base
		}
	}
	on := c.added()
	if c.gauss == 0 {
		return add(base, on)
	}
	rng := rand.New(rand.NewPCG(c.seed, 2))
	out := make([]sample, len(base))
	for k, s := range base {
		d := on(s.T)
		s.I += d * (1 + rng.NormFloat64()*c.gauss)
		s.P = 5 * s.I
		out[k] = s
	}
	return out
}

// added is the current the case adds at time t.
func (c sweepCase) added() func(float64) float64 {
	switch c.kind {
	case "onoff", "fast":
		return func(t float64) float64 {
			if math.Mod(t+c.shift, c.on+c.off) < c.on {
				return c.amp
			}
			return 0
		}
	case "burst":
		return func(t float64) float64 {
			if t >= c.at && t < c.at+c.dur {
				return c.amp
			}
			return 0
		}
	}
	return func(t float64) float64 {
		if t >= c.at {
			return c.amp
		}
		return 0
	}
}

type change struct {
	t        float64
	required bool
}

// expect returns the changes the case must (or may) show, whether medians
// are checked, and ok=false when the definition leaves the case open.
func (c sweepCase) expect() (want []change, medians, ok bool) {
	clear := func(dur float64) bool {
		r := dur / c.poll
		return dur >= 8 && (r >= levelFewReadings || (r >= 8 && math.Abs(c.amp) >= 0.3))
	}
	seamy := func(t float64) bool { return c.gauss == 0 && !offSeam(t, t) }
	switch c.kind {
	case "fast":
		return nil, false, true
	case "onoff":
		period := c.on + c.off
		var ts []float64
		for k := -1; float64(k)*period-c.shift < c.window+period; k++ {
			for _, at := range []float64{float64(k)*period - c.shift, float64(k)*period + c.on - c.shift} {
				if at > 0 && at < c.window {
					ts = append(ts, at)
				}
			}
		}
		slices.Sort(ts)
		if len(ts) == 0 {
			return nil, false, false
		}
		for k := 1; k < len(ts); k++ { // interior phases: all must be clear
			if !clear(ts[k] - ts[k-1]) {
				return nil, false, false
			}
		}
		for k, at := range ts {
			req := !seamy(at)
			if k == 0 { // the edge phase before it
				if at <= 4 {
					continue
				}
				if !clear(at) {
					req = false
				}
			}
			if k == len(ts)-1 {
				if c.window-at <= 4 {
					continue
				}
				if !clear(c.window - at) {
					req = false
				}
			}
			want = append(want, change{at, req})
		}
		return want, true, true
	case "burst":
		switch {
		case c.dur <= levelMinS-math.Max(0.5, 2*c.poll):
			return nil, false, true
		case clear(c.dur):
			return []change{{c.at, !seamy(c.at)}, {c.at + c.dur, !seamy(c.at + c.dur)}}, true, true
		}
		return nil, false, false
	}
	rel := math.Abs(c.amp) / math.Min(0.1655, 0.1655+c.amp)
	if rel < 0.065 || !clear(math.Min(c.at, c.window-c.at)) {
		return nil, false, false
	}
	return []change{{c.at, !seamy(c.at)}}, true, true
}

// check returns why the case fails ("" when it passes), and whether it
// asserted the expected changes.
func check(t *testing.T, c sweepCase, cache map[[2]float64][]sample) (string, bool) {
	ss := c.series(t, cache)
	lv := levels(ss, c.window)
	// The size rule holds on the published medians, always.
	for k := 1; k < len(lv); k++ {
		a, b := lv[k-1].IBusA.P50, lv[k].IBusA.P50
		if math.Abs(a-b) < math.Max(levelAbsA, levelRel*math.Min(math.Abs(a), math.Abs(b)))-1e-6 {
			return fmt.Sprintf("levels %d and %d have medians %.2f and %.2f mA, closer than the size rule", k-1, k, a*1000, b*1000), true
		}
	}
	want, medians, ok := c.expect()
	if !ok {
		return "", false
	}
	tol := math.Max(2, 3*c.poll)
	got := starts(lv)
	used := make([]bool, len(want))
	for _, g := range got {
		best := -1
		for k, w := range want {
			if !used[k] && math.Abs(g-w.t) <= tol && (best < 0 || math.Abs(g-w.t) < math.Abs(g-want[best].t)) {
				best = k
			}
		}
		if best < 0 {
			return fmt.Sprintf("unexpected change at %.2f s (changes %v, want %v)", g, got[:min(6, len(got))], want[:min(6, len(want))]), true
		}
		used[best] = true
	}
	for k, w := range want {
		if w.required && !used[k] {
			return fmt.Sprintf("missing change at %.2f s (changes %v)", w.t, got[:min(6, len(got))]), true
		}
	}
	if medians {
		lo, hi := 0.1655, 0.1655+c.amp
		tolM := math.Abs(c.amp) / 3
		for k := 1; k+1 < len(lv); k++ { // interior levels
			if p := lv[k].IBusA.P50; math.Abs(p-lo) > tolM && math.Abs(p-hi) > tolM {
				return fmt.Sprintf("level %d (%.1f-%.1f s) median %.1f mA is neither %.1f nor %.1f", k, lv[k].StartS, lv[k].EndS, p*1000, lo*1000, hi*1000), true
			}
		}
	}
	return "", true
}

func TestLevelsSweep(t *testing.T) {
	full := 0
	if v := os.Getenv("POWERZ_LEVELS_SWEEP"); v != "" {
		full, _ = strconv.Atoi(v)
	}
	rng := rand.New(rand.NewPCG(2026, 11))
	cache := map[[2]float64][]sample{}
	type tally struct{ asserted, skipped, failed int }
	regimes := map[string]*tally{}
	asserted, cases := 0, 0
	for ; full > 0 && cases < full || full == 0 && asserted < sweepSample && cases < 20*sweepSample; cases++ {
		c := randomCase(rng)
		if full == 0 && c.window > 3600 {
			continue // the regular run keeps to short windows (time under -race)
		}
		src := "capture"
		if c.gauss > 0 {
			src = "gauss"
		}
		claim := "approximate"
		if c.claimed() {
			claim = "claimed"
		}
		key := fmt.Sprintf("%-11s %-5s %-7s every %3.1f s", claim, c.kind, src, c.poll)
		if regimes[key] == nil {
			regimes[key] = &tally{}
		}
		if !c.claimed() && (full == 0 || os.Getenv("POWERZ_LEVELS_SWEEP_ALL") == "") {
			continue // approximate regimes: POWERZ_LEVELS_SWEEP_ALL=1 (slow: long windows)
		}
		why, did := check(t, c, cache)
		if did {
			regimes[key].asserted++
			if c.claimed() {
				asserted++
			}
		} else {
			regimes[key].skipped++
		}
		if why != "" {
			regimes[key].failed++
			// Outside the claimed regime only the structural size rule fails.
			if c.claimed() || strings.Contains(why, "closer than the size rule") {
				t.Errorf("%v: %s", c, why)
			}
		}
	}
	if full > 0 {
		keys := make([]string, 0, len(regimes))
		for k := range regimes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			r := regimes[k]
			t.Logf("%s  %4d asserted  %4d skipped  %3d failed", k, r.asserted, r.skipped, r.failed)
		}
		t.Logf("%d cases: %d claimed cases asserted", cases, asserted)
		if asserted < full/5 {
			t.Errorf("only %d of %d cases were claimed cases that asserted anything", asserted, full)
		}
	} else if asserted < sweepSample {
		t.Errorf("only %d cases asserted in %d", asserted, cases)
	}
}
