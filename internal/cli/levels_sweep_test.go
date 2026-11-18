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

// A seeded sweep of levels over loads, change sizes, polling, windows and
// noise. Loads: the real pulsing-load capture (tiled, lifted 100 mA so its
// own slow wander stays under the 5% size rule; at 5% of its median, 18% of
// its readings are off it, in pulses of up to 0.8 s), Gaussian noise, or
// regular +55 mA pulses (0.2-1 s long, 5-30% of the time) on 1% noise.
//
// The docs call levels reliable for the cases claimed() selects, a table of
// parameters: 100 ms polling; Gaussian noise up to 5%, the capture, or pulses
// up to 0.8 s and 25% of the time. There every change of 15% or more is
// asserted case by case; smaller ones (down to the 5% size rule) can be
// merged near the window edges, and the sweep asserts their documented miss
// rate instead. Elsewhere levels are approximate. Expectations follow the
// documented definition (its numbers written out here, so changing a
// constant in levels.go cannot move them), with margins:
//
//   - a phase is clear when it lasts 8 s and holds 12 readings (or 8
//     readings of a +300 mA step, which the rank test separates) and its
//     expected steady time clears 5 s: by 4 standard deviations with
//     Gaussian noise (about 10 s at 5% noise), by a pulse and 0.5 s with
//     regular pulses, and on the capture (whose pulses cluster in places) when
//     it lasts 9.5 s (at 9 s, ED-PELT misses about 1 change in 600 next to
//     such a phase, at the capture's hardest spots); short when it lasts 4 s
//     or less; ambiguous otherwise;
//   - every switch next to a clear phase must be found, within 4 s; the two
//     switches nearest an edge phase that is not clearly a level may be (it
//     joins its neighbour, and the mixed median can merge that neighbour with
//     the next phase); one inside a short edge phase must not; a case with an
//     ambiguous interior phase asserts only the structure;
//   - fast switching (every phase 4 s or less, away from 50% duty) is one
//     level, at the current it spends more time at;
//   - every level's median is within a third of the step of one of the two
//     currents, except an edge level that took in an edge phase too short to
//     be a level (its median mixes the two);
//   - change times, across claimed changes of 15% or more: without synthetic
//     pulses, within 0.2 s at the median, 0.6 s at the 90th percentile and
//     1.5 s at the 99th; with pulses, 0.2, 1.2 and 2 s; unbiased (mean within
//     0.15 s);
//   - structure, on every case run: the levels tile the window, neighbouring
//     published medians pass the size rule, and with more than one level each
//     lasts 5 s and holds 5 readings.
//
// The regular run asserts a sample of claimed cases. CI runs
// POWERZ_LEVELS_SWEEP=3000 in a step without the race detector: all checks on
// claimed cases, the structure on approximate ones with windows up to an hour
// (longer ones take minutes). POWERZ_LEVELS_SWEEP_ALL=1 runs every case and
// reports (without failing on) approximate cases that miss expectations;
// POWERZ_LEVELS_SWEEP_SEED=a,b picks another seed. The full run also checks
// the small-change miss rate on its worst case (smallChangesAtEdges).
const (
	sweepSample      = 30 // asserted claimed cases in the regular run
	sweepMinS        = 5.0
	sweepMinReadings = 5
	sweepRel         = 0.05
	sweepAbsA        = 0.002
	sweepFloor       = 0.15 // smaller changes: a miss rate, not case by case
	sweepSmallRate   = 0.03 // share of worst-case small-change windows that may miss one (docs: about 1%)
	sweepTol         = 4.0  // seconds a found change may be from the switch (more at slow polling)
	sweepBias        = 0.15
	sweepPulseA      = 0.055
)

// Documented change-time quantiles (p50, p90, p99), without and with pulses.
var sweepTimes, sweepPulsedTimes = [3]float64{0.2, 0.6, 1.5}, [3]float64{0.2, 1.2, 2}

// The default-seed full run, pinned: claimed cases asserted case by case,
// how many of them require a change, and small-change cases.
const sweepPinAsserted, sweepPinChanging, sweepPinSmall = 764, 460, 208

type sweepCase struct {
	kind         string  // onoff, fast, burst, step
	gauss        float64 // Gaussian noise, relative (0: the capture)
	pulseW, duty float64 // regular pulses: seconds long, share of the time (on 1% noise)
	pulsePhase   float64
	on, off, amp float64
	at, dur      float64
	poll, window float64
	shift        float64
	seed         uint64
}

func (c sweepCase) pulsed() bool { return c.pulseW > 0 }

// claimed reports whether the case is in the table the docs call reliable.
func (c sweepCase) claimed() bool {
	return c.poll <= 0.1 && c.gauss <= 0.05 && (!c.pulsed() || c.pulseW <= 0.8 && c.duty <= 0.25)
}

// small reports whether the case's change is under the floor (relative to
// the smaller current).
func (c sweepCase) small() bool {
	return c.kind != "fast" && math.Abs(c.amp)/math.Min(0.1655, 0.1655+c.amp) < sweepFloor
}

func (c sweepCase) source() string {
	switch {
	case c.pulsed():
		return fmt.Sprintf("%.2f s pulses %.0f%% of the time", c.pulseW, c.duty*100)
	case c.gauss > 0:
		return fmt.Sprintf("%.0f%% Gaussian", c.gauss*100)
	}
	return "capture"
}

func (c sweepCase) String() string {
	switch c.kind {
	case "onoff", "fast":
		return fmt.Sprintf("%s %.2f/%.2f s %+.0f mA, every %v s, %v s window, shift %.2f, %s", c.kind, c.on, c.off, c.amp*1000, c.poll, c.window, c.shift, c.source())
	case "burst":
		return fmt.Sprintf("burst %.2f s %+.0f mA at %.2f s, every %v s, %v s window, %s", c.dur, c.amp*1000, c.at, c.poll, c.window, c.source())
	}
	return fmt.Sprintf("step %+.0f mA at %.2f s, every %v s, %v s window, %s", c.amp*1000, c.at, c.poll, c.window, c.source())
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
	switch rng.IntN(6) {
	case 0, 1:
		c.gauss = pick(0.01, 0.05, 0.1)
	case 2: // regular pulses, across the claimed bounds (0.8 s, 25%)
		c.gauss = 0.01
		c.pulseW = 0.2 + 0.8*rng.Float64()
		c.duty = 0.05 + 0.25*rng.Float64()
		c.pulsePhase = rng.Float64() * c.pulseW / c.duty
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
			// away from 50% duty, counting the load's own pulses (a quarter
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
		period := c.pulseW / c.duty
		for k := 0; float64(k)*c.poll < c.window; k++ {
			tt := float64(k) * c.poll
			i := 0.1655
			if c.pulsed() && math.Mod(tt+c.pulsePhase, period) < c.pulseW {
				i += sweepPulseA
			}
			i *= 1 + rng.NormFloat64()*c.gauss
			base = append(base, sample{T: tt, V: 5, I: i, P: 5 * i})
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

// expect returns the changes the case must (or may) show; whether the first
// and the last level's medians are checked (not when the level took in an
// edge phase too short to be a level); and ok=false when the definition
// leaves the case open.
func (c sweepCase) expect() (want []change, edges [2]bool, ok bool) {
	on := c.added()
	// clear reports whether a phase of dur seconds (on: with the added
	// current) is clearly a level. With Gaussian noise, a reading is steady
	// (within 5% of the median) with probability p, and the phase's expected
	// steady time must clear 5 s by 4 standard deviations: a window holds
	// hundreds of phases. (At 5% noise that is about 10 s.) Regular pulses
	// take their share of the time, and one pulse more.
	clear := func(dur float64, isOn bool) bool {
		r := dur / c.poll
		if dur < 8 || r < 12 && (r < 8 || math.Abs(c.amp) < 0.3) {
			return false
		}
		if c.gauss == 0 {
			return dur >= 9.5 // the capture: pulses cluster in places
		}
		if c.pulsed() && dur*(1-c.duty)-c.pulseW < sweepMinS+0.5 {
			return false
		}
		cur, sd := 0.1655, 0.1655*c.gauss
		if isOn {
			cur, sd = 0.1655+c.amp, c.gauss*math.Hypot(0.1655, c.amp)
		}
		tol := math.Max(sweepAbsA, sweepRel*math.Abs(cur))
		pr := math.Erf(tol / (sd * math.Sqrt2))
		return c.poll*(r*pr-4*math.Sqrt(r*pr*(1-pr))) >= sweepMinS
	}
	seamy := func(t float64) bool { return c.gauss == 0 && !offSeam(t, t) }
	both := [2]bool{true, true}
	switch c.kind {
	case "fast":
		// Regular pulses on the off phases count as time near the on
		// current; near half the time, either current can be the median.
		if high := c.on / (c.on + c.off); c.pulsed() && math.Abs(high+(1-high)*c.duty-0.5) < 0.15 {
			return nil, edges, false
		}
		return nil, both, true
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
			return nil, edges, false
		}
		for k := 1; k < len(ts); k++ { // interior phases: all must be clear
			if !clear(ts[k]-ts[k-1], on((ts[k]+ts[k-1])/2) != 0) {
				return nil, edges, false
			}
		}
		last := len(ts) - 1
		edges = [2]bool{clear(ts[0], on(ts[0]/2) != 0), clear(c.window-ts[last], on((ts[last]+c.window)/2) != 0)}
		for k, at := range ts {
			req := !seamy(at)
			// An edge phase that is not clearly a level joins its neighbour,
			// and the mixed median can merge that neighbour with the next
			// phase too: the changes on both sides of the neighbour may go.
			if k == 0 && at <= 4 || k == last && c.window-at <= 4 {
				continue // inside a short edge phase
			}
			if k <= 1 && !edges[0] || k >= last-1 && !edges[1] {
				req = false
			}
			want = append(want, change{at, req})
		}
		return want, edges, true
	case "burst":
		switch {
		case c.dur <= sweepMinS-math.Max(0.5, 2*c.poll):
			return nil, both, true
		case clear(c.dur, true):
			return []change{{c.at, !seamy(c.at)}, {c.at + c.dur, !seamy(c.at + c.dur)}}, both, true
		}
		return nil, edges, false
	}
	if !clear(c.at, false) || !clear(c.window-c.at, true) {
		return nil, edges, false
	}
	return []change{{c.at, !seamy(c.at)}}, both, true
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
	offsets    []float64 // seconds from each matched switch to its change (signed)
}

func check(t *testing.T, c sweepCase, cache map[[2]float64][]sample, expectations bool) sweepResult {
	ss := c.series(t, cache)
	return judge(c, levels(ss, c.window), len(ss), expectations)
}

// judge checks levels lv (of n readings) against the case's expectations.
func judge(c sweepCase, lv []levelJSON, n int, expectations bool) sweepResult {
	r := sweepResult{structural: structure(lv, n, c.window)}
	want, edges, ok := c.expect()
	if !expectations || !ok || r.structural != "" {
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
		r.offsets = append(r.offsets, g-want[best].t)
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
		if k == 0 && !edges[0] || k == len(lv)-1 && !edges[1] {
			continue // took in an edge phase too short to be a level
		}
		if p := l.IBusA.P50; math.Abs(p-lo) > tolM && math.Abs(p-hi) > tolM {
			r.expected = fmt.Sprintf("level %d (%.1f-%.1f s) median %.1f mA is neither %.1f nor %.1f", k, l.StartS, l.EndS, p*1000, lo*1000, hi*1000)
			return r
		}
	}
	if c.kind == "fast" { // one level, at the current it spends more time at
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

// timing returns why signed change-time offsets miss the documented
// distribution ("" if they do not, or there are too few to tell).
func timing(offsets []float64, bound [3]float64) (string, string) {
	if len(offsets) < 100 {
		return "", ""
	}
	abs := make([]float64, len(offsets))
	mean := 0.0
	for k, o := range offsets {
		abs[k] = math.Abs(o)
		mean += o / float64(len(offsets))
	}
	slices.Sort(abs)
	p50, p90, p99 := quantile(abs, 0.5), quantile(abs, 0.9), quantile(abs, 0.99)
	stats := fmt.Sprintf("%d changes, p50 %.2f s, p90 %.2f s, p99 %.2f s, max %.2f s, mean %+.3f s", len(abs), p50, p90, p99, abs[len(abs)-1], mean)
	if p50 > bound[0] || p90 > bound[1] || p99 > bound[2] || math.Abs(mean) > sweepBias {
		return stats, fmt.Sprintf("change times %s: the docs say p50 %v s, p90 %v s, p99 %v s, mean within %v s", stats, bound[0], bound[1], bound[2], sweepBias)
	}
	return stats, ""
}

func TestLevelsSweep(t *testing.T) {
	full := 0
	if v := os.Getenv("POWERZ_LEVELS_SWEEP"); v != "" {
		var err error
		if full, err = strconv.Atoi(v); err != nil || full <= 0 {
			t.Fatalf("POWERZ_LEVELS_SWEEP=%q: want a number of cases", v)
		}
	}
	all := os.Getenv("POWERZ_LEVELS_SWEEP_ALL") != ""
	seed := [2]uint64{2026, 11}
	if v := os.Getenv("POWERZ_LEVELS_SWEEP_SEED"); v != "" {
		a, b, _ := strings.Cut(v, ",")
		var errA, errB error
		seed[0], errA = strconv.ParseUint(a, 10, 64)
		seed[1], errB = strconv.ParseUint(b, 10, 64)
		if errA != nil || errB != nil {
			t.Fatalf("POWERZ_LEVELS_SWEEP_SEED=%q: want two numbers, a,b", v)
		}
	}
	rng := rand.New(rand.NewPCG(seed[0], seed[1]))
	cache := map[[2]float64][]sample{}
	type tally struct{ asserted, changing, structureOnly, skipped, failed int }
	regimes := map[string]*tally{}
	var claimed, small tally
	var offsets, pulsedOffsets []float64
	cases := 0
	for ; full > 0 && cases < full || full == 0 && claimed.asserted < sweepSample && cases < 20*sweepSample; cases++ {
		c := randomCase(rng)
		claim := "approximate"
		switch {
		case c.claimed() && c.small():
			claim = "small"
		case c.claimed():
			claim = "claimed"
		}
		src := "capture"
		switch {
		case c.pulsed():
			src = "pulses"
		case c.gauss > 0:
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
			if claim == "claimed" {
				t.Errorf("%v: %s", c, r.expected)
			}
		}
		if !r.asserted {
			continue
		}
		switch claim {
		case "small":
			small.asserted++
			if r.expected != "" {
				small.failed++
			}
		case "claimed":
			claimed.asserted++
			if r.changing {
				claimed.changing++
			}
			if c.pulsed() {
				pulsedOffsets = append(pulsedOffsets, r.offsets...)
			} else {
				offsets = append(offsets, r.offsets...)
			}
		}
	}
	for _, tm := range []struct {
		name    string
		offsets []float64
		bound   [3]float64
	}{{"claimed change times", offsets, sweepTimes}, {"with pulses", pulsedOffsets, sweepPulsedTimes}} {
		stats, bad := timing(tm.offsets, tm.bound)
		if stats != "" {
			t.Logf("%s: %s", tm.name, stats)
		}
		if bad != "" {
			t.Errorf("%s: %s", tm.name, bad)
		}
	}
	if small.asserted > 0 {
		t.Logf("changes under %.0f%%: %d of %d cases miss one", sweepFloor*100, small.failed, small.asserted)
		if small.asserted >= 50 && float64(small.failed) > sweepSmallRate*float64(small.asserted) {
			t.Errorf("changes under %.0f%%: %d of %d cases miss one; the docs say about %.0f%%", sweepFloor*100, small.failed, small.asserted, sweepSmallRate*100)
		}
	}
	if full > 0 {
		smallChangesAtEdges(t, cache)
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
	t.Logf("%d cases: %d claimed cases asserted, %d of them requiring a change; %d small-change cases", cases, claimed.asserted, claimed.changing, small.asserted)
	if seed == [2]uint64{2026, 11} && full == 3000 {
		if claimed.asserted != sweepPinAsserted || claimed.changing != sweepPinChanging || small.asserted != sweepPinSmall {
			t.Errorf("default sweep asserted %d claimed cases (%d changing) and %d small-change cases, pinned at %d (%d) and %d: update the pins only for a deliberate change to the generator or expectations",
				claimed.asserted, claimed.changing, small.asserted, sweepPinAsserted, sweepPinChanging, sweepPinSmall)
		}
	} else if claimed.asserted < full/5 {
		t.Errorf("only %d of %d cases were claimed cases that asserted anything", claimed.asserted, full)
	}
}

// smallChangesAtEdges checks the documented miss rate for changes under the
// floor on their worst case: +15 mA (9%) on/off in 10-13 s phases, in 2 min
// windows (every one with edge phases), on each claimed load.
func smallChangesAtEdges(t *testing.T, cache map[[2]float64][]sample) {
	for _, src := range []string{"capture", "5% Gaussian", "pulses"} {
		rng := rand.New(rand.NewPCG(9, uint64(len(src))))
		failed, asserted := 0, 0
		for range 300 {
			c := sweepCase{kind: "onoff", on: 10 + 3*rng.Float64(), off: 10 + 3*rng.Float64(), amp: 0.015, poll: 0.1, window: 120, seed: rng.Uint64()}
			c.shift = rng.Float64() * (c.on + c.off)
			switch src {
			case "5% Gaussian":
				c.gauss = 0.05
			case "pulses":
				c.gauss, c.pulseW, c.duty = 0.01, 0.2+0.6*rng.Float64(), 0.05+0.2*rng.Float64()
				c.pulsePhase = rng.Float64() * c.pulseW / c.duty
			}
			r := check(t, c, cache, true)
			if r.structural != "" {
				t.Errorf("%v: %s", c, r.structural)
			}
			if r.asserted {
				asserted++
				if r.expected != "" {
					failed++
				}
			}
		}
		t.Logf("+15 mA in 10-13 s phases, 2 min windows, %s: %d of %d windows miss a change", src, failed, asserted)
		if float64(failed) > sweepSmallRate*float64(asserted) {
			t.Errorf("+15 mA in 10-13 s phases, %s: %d of %d windows miss a change; the docs say about 1%%", src, failed, asserted)
		}
	}
}

// The sweep's checks catch wrong levels: one level everywhere, a change
// dropped, every change 1 s late.
func TestLevelsSweepCatchesWrongLevels(t *testing.T) {
	rng := rand.New(rand.NewPCG(2026, 11))
	cache := map[[2]float64][]sample{}
	var late []float64
	checked := 0
	for checked < 20 {
		c := randomCase(rng)
		if !c.claimed() || c.small() || c.window > 600 || c.pulsed() {
			continue
		}
		ss := c.series(t, cache)
		lv := levels(ss, c.window)
		r := judge(c, lv, len(ss), true)
		if !r.changing || r.expected != "" || r.structural != "" || len(lv) < 3 {
			continue
		}
		checked++
		one := []levelJSON{{StartS: 0, EndS: round(c.window, 3), Samples: len(ss),
			IBusA: describe(values(ss, func(s sample) float64 { return s.I }), 6)}}
		if judge(c, one, len(ss), true).expected == "" {
			t.Errorf("%v: one level everywhere passes", c)
		}
		dropped := slices.Clone(lv)
		dropped[0].EndS, dropped[0].Samples = dropped[1].EndS, dropped[0].Samples+dropped[1].Samples
		dropped = slices.Delete(dropped, 1, 2)
		if r := judge(c, dropped, len(ss), true); r.expected == "" && r.structural == "" {
			t.Errorf("%v: a dropped change passes", c)
		}
		shifted := slices.Clone(lv)
		for k := range shifted {
			if k > 0 {
				shifted[k].StartS += 1
			}
			if k+1 < len(shifted) {
				shifted[k].EndS += 1
			}
		}
		r = judge(c, shifted, len(ss), true)
		late = append(late, r.offsets...)
	}
	if _, bad := timing(append(late, make([]float64, max(0, 100-len(late)))...), sweepTimes); bad == "" {
		t.Errorf("every change 1 s late passes the timing check (%d changes)", len(late))
	}
}
