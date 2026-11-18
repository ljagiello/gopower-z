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
// noise: the real pulsing-load capture (pulses of up to about a second, tiled,
// lifted 100 mA so its own slow wander stays under the 5% size rule) or
// Gaussian noise.
//
// Levels are documented as reliable at the default 100 ms polling on loads
// that vary by up to about 5% or pulse briefly (claimed cases), and as
// approximate elsewhere. Expectations follow the documented definition (its
// numbers written out here, so changing a constant in levels.go cannot move
// them), with margins:
//
//   - a phase is clear when it lasts 8 s and holds 12 readings (or 8
//     readings of a +300 mA step, which the rank test separates) and, with
//     Gaussian noise, its expected steady time clears 5 s by 4 standard
//     deviations (about 10 s at 5% noise); short when it lasts 4 s or less;
//     ambiguous otherwise;
//   - every switch next to a clear phase must be found, within 4 s; a switch
//     next to an ambiguous edge phase may be; one inside a short edge phase
//     must not; a case with an ambiguous interior phase asserts only the
//     structure;
//   - fast switching (every phase 4 s or less, away from 50% duty) is one
//     level, at the current it spends more time at;
//   - every level's median is within a third of the step of one of the two
//     currents;
//   - across claimed cases, change times are within 0.2 s at the median and
//     1.5 s at the 99th percentile (what the docs say);
//   - structure, on every case run: the levels tile the window, neighbouring
//     published medians pass the size rule, and with more than one level each
//     lasts 5 s and holds 5 readings.
//
// The regular run asserts a sample of claimed cases. CI runs
// POWERZ_LEVELS_SWEEP=2000 in a step without the race detector: all checks on
// claimed cases, the structure on approximate ones with windows up to an hour
// (longer ones take minutes). POWERZ_LEVELS_SWEEP_ALL=1 runs every case and
// reports (without failing on) approximate cases that miss expectations;
// POWERZ_LEVELS_SWEEP_SEED=a,b picks another seed.
const (
	sweepSample        = 30 // asserted claimed cases in the regular run
	sweepMinS          = 5.0
	sweepMinReadings   = 5
	sweepRel           = 0.05
	sweepAbsA          = 0.002
	sweepTol           = 4.0 // seconds a found change may be from the switch (more at slow polling)
	sweepP50, sweepP99 = 0.2, 1.5
)

// The default-seed full run: claimed cases that assert something, and how
// many of them require a change (pins the generator and expectations).
const sweepClaimedAsserted, sweepClaimedChanging = 714, 480

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
	// Mostly loads that change (on/off, steps); fast switching is always
	// read at 100 ms, so it gets fewer draws.
	switch k := rng.IntN(20); {
	case k < 7:
		c.kind = "onoff"
		if c.poll >= 3 {
			c.on, c.off = 36+rng.Float64()*54, 36+rng.Float64()*54
			c.window = pick(600, 3600, 20000)
		} else {
			c.on, c.off = 3+rng.Float64()*37, 3+rng.Float64()*37
		}
		c.amp = pick(0.015, 0.03, 0.06, 0.3)
		c.shift = rng.Float64() * (c.on + c.off)
	case k < 9:
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
	case k < 13:
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

// expect returns the changes the case must (or may) show, and ok=false when
// the definition leaves the case open.
func (c sweepCase) expect() (want []change, ok bool) {
	// clear reports whether a phase of dur seconds (on: with the added
	// current) is clearly a level. With Gaussian noise, a reading is steady
	// (within 5% of the median) with probability p, and the phase's expected
	// steady time must clear 5 s by 4 standard deviations: a window holds
	// hundreds of phases. (At 5% noise that is about 10 s.)
	clear := func(dur float64, on bool) bool {
		r := dur / c.poll
		if dur < 8 || r < 12 && (r < 8 || math.Abs(c.amp) < 0.3) {
			return false
		}
		if c.gauss == 0 {
			return true
		}
		cur, sd := 0.1655, 0.1655*c.gauss
		if on {
			cur, sd = 0.1655+c.amp, c.gauss*math.Hypot(0.1655, c.amp)
		}
		tol := math.Max(sweepAbsA, sweepRel*math.Abs(cur))
		pr := math.Erf(tol / (sd * math.Sqrt2))
		return c.poll*(r*pr-4*math.Sqrt(r*pr*(1-pr))) >= sweepMinS
	}
	on := c.added()
	seamy := func(t float64) bool { return c.gauss == 0 && !offSeam(t, t) }
	switch c.kind {
	case "fast":
		return nil, true
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
			return nil, false
		}
		for k := 1; k < len(ts); k++ { // interior phases: all must be clear
			if !clear(ts[k]-ts[k-1], on((ts[k]+ts[k-1])/2) != 0) {
				return nil, false
			}
		}
		for k, at := range ts {
			req := !seamy(at)
			if k == 0 { // the edge phase before it
				if at <= 4 {
					continue
				}
				if !clear(at, on(at/2) != 0) {
					req = false
				}
			}
			if k == len(ts)-1 {
				if c.window-at <= 4 {
					continue
				}
				if !clear(c.window-at, on((at+c.window)/2) != 0) {
					req = false
				}
			}
			want = append(want, change{at, req})
		}
		return want, true
	case "burst":
		switch {
		case c.dur <= sweepMinS-math.Max(0.5, 2*c.poll):
			return nil, true
		case clear(c.dur, true):
			return []change{{c.at, !seamy(c.at)}, {c.at + c.dur, !seamy(c.at + c.dur)}}, true
		}
		return nil, false
	}
	rel := math.Abs(c.amp) / math.Min(0.1655, 0.1655+c.amp)
	if rel < 0.065 || !clear(c.at, false) || !clear(c.window-c.at, true) {
		return nil, false
	}
	return []change{{c.at, !seamy(c.at)}}, true
}

// structure returns why the levels break the definition's structure ("" if
// they do not): they must tile the window, neighbouring published medians
// must pass the size rule, and with more than one level, each must last 5 s
// and hold 5 readings.
func structure(lv []levelJSON, n int, window float64) string {
	if n > 0 && len(lv) == 0 {
		return "no levels"
	}
	total := 0
	for k, l := range lv {
		total += l.Samples
		if k == 0 && l.StartS != 0 || k == len(lv)-1 && l.EndS != round(window, 3) || k > 0 && l.StartS != lv[k-1].EndS {
			return fmt.Sprintf("level %d (%v-%v s) does not tile the window", k, l.StartS, l.EndS)
		}
		if len(lv) > 1 && (l.EndS-l.StartS < sweepMinS-1e-6 || l.Samples < sweepMinReadings) {
			return fmt.Sprintf("level %d (%v-%v s, %d readings) is shorter than a level", k, l.StartS, l.EndS, l.Samples)
		}
		if k > 0 {
			a, b := lv[k-1].IBusA.P50, l.IBusA.P50
			if math.Abs(a-b) < math.Max(sweepAbsA, sweepRel*math.Min(math.Abs(a), math.Abs(b)))-1e-6 {
				return fmt.Sprintf("levels %d and %d have medians %.2f and %.2f mA, closer than the size rule", k-1, k, a*1000, b*1000)
			}
		}
	}
	if total != n {
		return fmt.Sprintf("levels hold %d readings of %d", total, n)
	}
	return ""
}

// sweepResult is what one case showed.
type sweepResult struct {
	structural string    // a structural failure, or ""
	expected   string    // a failed expectation, or ""
	asserted   bool      // expectations were checked
	changing   bool      // and required a change
	offsets    []float64 // seconds between each matched change and its switch
}

func check(t *testing.T, c sweepCase, cache map[[2]float64][]sample, expectations bool) sweepResult {
	ss := c.series(t, cache)
	lv := levels(ss, c.window)
	r := sweepResult{structural: structure(lv, len(ss), c.window)}
	want, ok := c.expect()
	if !expectations || !ok {
		return r
	}
	r.asserted = true
	for _, w := range want {
		r.changing = r.changing || w.required
	}
	tol := math.Max(sweepTol, 3*c.poll)
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
			r.expected = fmt.Sprintf("unexpected change at %.2f s (changes %v, want %v)", g, got[:min(6, len(got))], want[:min(6, len(want))])
			return r
		}
		used[best] = true
		r.offsets = append(r.offsets, math.Abs(g-want[best].t))
	}
	for k, w := range want {
		if w.required && !used[k] {
			r.expected = fmt.Sprintf("missing change at %.2f s (changes %v)", w.t, got[:min(6, len(got))])
			return r
		}
	}
	lo, hi := 0.1655, 0.1655+c.amp
	tolM := math.Abs(c.amp) / 3
	for k, l := range lv {
		if p := l.IBusA.P50; math.Abs(p-lo) > tolM && math.Abs(p-hi) > tolM {
			r.expected = fmt.Sprintf("level %d (%.1f-%.1f s) median %.1f mA is neither %.1f nor %.1f", k, l.StartS, l.EndS, p*1000, lo*1000, hi*1000)
			return r
		}
	}
	// Fast switching: one level at the current it spends more time at.
	if c.kind == "fast" {
		dom := lo
		if c.on > c.off {
			dom = hi
		}
		if p := lv[0].IBusA.P50; math.Abs(p-dom) > tolM {
			r.expected = fmt.Sprintf("fast switching: median %.1f mA, want %.1f (the current it spends more time at)", p*1000, dom*1000)
		}
	}
	return r
}

func TestLevelsSweep(t *testing.T) {
	full := 0
	if v := os.Getenv("POWERZ_LEVELS_SWEEP"); v != "" {
		full, _ = strconv.Atoi(v)
	}
	all := os.Getenv("POWERZ_LEVELS_SWEEP_ALL") != ""
	seed := [2]uint64{2026, 11}
	if v := os.Getenv("POWERZ_LEVELS_SWEEP_SEED"); v != "" {
		a, b, _ := strings.Cut(v, ",")
		seed[0], _ = strconv.ParseUint(a, 10, 64)
		seed[1], _ = strconv.ParseUint(b, 10, 64)
	}
	rng := rand.New(rand.NewPCG(seed[0], seed[1]))
	cache := map[[2]float64][]sample{}
	type tally struct{ asserted, changing, structureOnly, skipped, failed int }
	regimes := map[string]*tally{}
	var claimed tally
	var offsets []float64
	cases := 0
	for ; full > 0 && cases < full || full == 0 && claimed.asserted < sweepSample && cases < 20*sweepSample; cases++ {
		c := randomCase(rng)
		claim := "approximate"
		if c.claimed() {
			claim = "claimed"
		}
		src := "capture"
		if c.gauss > 0 {
			src = "gauss"
		}
		key := fmt.Sprintf("%-11s %-5s %-7s every %3.1f s", claim, c.kind, src, c.poll)
		if regimes[key] == nil {
			regimes[key] = &tally{}
		}
		reg := regimes[key]
		// The regular run: claimed cases up to an hour (time under -race).
		// The full run: approximate cases' structure up to an hour, unless all.
		run := c.claimed() && (full > 0 || c.window <= 3600) || full > 0 && (all || c.window <= 3600)
		if !run {
			reg.skipped++
			continue
		}
		r := check(t, c, cache, c.claimed() || all)
		switch {
		case r.asserted:
			reg.asserted++
		case c.claimed() || all:
			reg.skipped++ // the definition leaves it open; structure only
		default:
			reg.structureOnly++
		}
		if r.structural != "" {
			reg.failed++
			t.Errorf("%v: %s", c, r.structural)
			continue
		}
		if r.expected != "" {
			reg.failed++
			if c.claimed() {
				t.Errorf("%v: %s", c, r.expected)
			}
		}
		if c.claimed() && r.asserted {
			claimed.asserted++
			if r.changing {
				claimed.changing++
			}
			offsets = append(offsets, r.offsets...)
		}
	}
	slices.Sort(offsets)
	if len(offsets) > 0 {
		p50, p99 := quantile(offsets, 0.5), quantile(offsets, 0.99)
		t.Logf("claimed change times: %d changes, p50 %.2f s, p99 %.2f s, max %.2f s", len(offsets), p50, p99, offsets[len(offsets)-1])
		if len(offsets) >= 100 && (p50 > sweepP50 || p99 > sweepP99) {
			t.Errorf("change times p50 %.2f s, p99 %.2f s: the docs say within %v s and %v s", p50, p99, sweepP50, sweepP99)
		}
	}
	if full == 0 {
		if claimed.asserted < sweepSample || claimed.changing < sweepSample/3 {
			t.Errorf("sample: %d claimed cases asserted (%d requiring a change) in %d", claimed.asserted, claimed.changing, cases)
		}
		return
	}
	keys := make([]string, 0, len(regimes))
	for k := range regimes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r := regimes[k]
		t.Logf("%s  %4d asserted  %4d structure only  %4d skipped  %3d failed", k, r.asserted, r.structureOnly, r.skipped, r.failed)
	}
	t.Logf("%d cases: %d claimed cases asserted, %d of them requiring a change", cases, claimed.asserted, claimed.changing)
	if seed == [2]uint64{2026, 11} && full == 2000 {
		if claimed.asserted != sweepClaimedAsserted || claimed.changing != sweepClaimedChanging {
			t.Errorf("default sweep asserted %d claimed cases (%d changing), pinned at %d (%d): update the pins only for a deliberate change to the generator or expectations",
				claimed.asserted, claimed.changing, sweepClaimedAsserted, sweepClaimedChanging)
		}
	} else if claimed.asserted < full/4 {
		t.Errorf("only %d of %d cases were claimed cases that asserted anything", claimed.asserted, full)
	}
}
