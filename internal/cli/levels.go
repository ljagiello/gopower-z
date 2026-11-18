package cli

import (
	"math"
	"slices"

	"pgregory.net/changepoint"
)

// Levels: the stretches of a measure window in which the current holds a
// steady typical value. A charger that switches on partway shows as two
// levels; a device that pulses every few seconds stays one level; a load
// that switches on and off for long enough shows every phase, and one that
// switches faster is one level.
//
// A level:
//
//   - lasts at least levelMinS seconds and holds levelMinReadings readings,
//     and is steady: at least levelMinS of its readings are within the size
//     tolerance of its median (pulses inside it do not count, so on a load
//     that pulses a third of the time a phase needs about 8 s);
//   - has a median that differs from its neighbours' by at least levelRel of
//     the smaller and at least levelAbsA;
//   - with fewer than levelFewReadings readings (slow polling), also differs
//     from its neighbour by a rank test (Mann-Whitney, |z| >= levelMinZ), so
//     a few readings that happen to catch a pulsing load's pulses do not
//     make one.
//
// Shorter stretches (bursts, boot spikes) stay inside the levels around
// them; a run of them lasting levelMinS is a level of its own (a level the
// pulses broke up, or fast switching, whose median is whichever current it
// spends more time at: near half the time, either).
//
// Candidate changes come from ED-PELT (Haynes, Fearnhead & Eckley, "A
// computationally efficient nonparametric approach for changepoint
// detection", 2017), a published method from pgregory.net/changepoint, run
// on 1 s averages of the current and of a short running median of it
// (window/levelPoints on very long windows), in chunks of levelChunk points
// over two sets of chunks half a chunk apart, then again inside each level
// found. ED-PELT treats readings as independent, so on a pulsing load it
// proposes many tiny changes, and in a run with many real changes it can
// miss some; resolve decides, and gives the same levels whatever extra
// candidates it is offered (TestResolveIgnoresExtraCandidates).
const (
	levelMinS        = 5.0     // seconds a level must last
	levelMinReadings = 5       // readings a level must hold
	levelRel         = 0.05    // neighbouring medians differ by at least 5% ...
	levelAbsA        = 0.002   // ... and at least 2 mA
	levelPoints      = 100_000 // averaging points at most (1 s up to 100,000 s of window)
	levelChunk       = 30      // ED-PELT input points per run
	levelSearches    = 3       // candidate searches (the later ones inside the levels found)
	levelSmoothS     = 1.5     // running median for candidates: seconds ...
	levelSmoothN     = 15      // ... and readings at most
	levelFewReadings = 12      // a level with fewer readings must also pass the rank test ...
	levelMinZ        = 3.0     // ... at |z| >= levelMinZ against its neighbour
)

type levelJSON struct {
	StartS  float64    `json:"start_s"`
	EndS    float64    `json:"end_s"`
	Samples int        `json:"samples"`
	VBusV   *rangeJSON `json:"vbus_v"`
	IBusA   *statJSON  `json:"ibus_a"`
	PowerW  *rangeJSON `json:"power_w"`
}

// levels splits the samples (in time order) of a window of the given length
// in seconds into levels of steady current. It returns an empty slice when
// there are no samples, one level when nothing changed. Readings taken just
// after the end of the window (measure's last read) belong to the last level.
func levels(ss []sample, window float64) []levelJSON {
	out := []levelJSON{}
	if len(ss) == 0 {
		return out
	}
	group := math.Max(1, window/levelPoints) // seconds per ED-PELT point
	avg, starts := averages(ss, group)
	// The same averages of a short running median of the readings, which
	// strips pulses (and pulses the polling aliases into every few readings)
	// that hide changes from ED-PELT on the plain averages.
	smooth, _ := averages(runningMedian(ss), group)

	cands := map[int]bool{}
	search(avg, starts, 0, len(avg), cands)
	search(smooth, starts, 0, len(smooth), cands)
	bounds := resolve(ss, candidateBounds(cands, len(ss)), group)
	for range levelSearches - 1 {
		n := len(cands)
		for k := 0; k+1 < len(bounds); k++ {
			g0, _ := slices.BinarySearch(starts, bounds[k])
			g1, _ := slices.BinarySearch(starts, bounds[k+1])
			search(avg, starts, g0, g1, cands)
			search(smooth, starts, g0, g1, cands)
		}
		if len(cands) == n {
			break
		}
		next := resolve(ss, candidateBounds(cands, len(ss)), group)
		if slices.Equal(next, bounds) {
			break
		}
		bounds = next
	}

	for k := 0; k+1 < len(bounds); k++ {
		run := ss[bounds[k]:bounds[k+1]]
		start, end := 0.0, window
		if k > 0 {
			start = run[0].T
		}
		if k+2 < len(bounds) {
			end = ss[bounds[k+1]].T
		}
		out = append(out, levelJSON{
			StartS: round(start, 3), EndS: round(end, 3), Samples: len(run),
			VBusV:  binRange(run, func(s sample) float64 { return s.V }),
			IBusA:  describe(values(run, func(s sample) float64 { return s.I }), 6),
			PowerW: binRange(run, func(s sample) float64 { return s.P }),
		})
	}
	return out
}

// averages returns the mean current over groups of `group` seconds and the
// first sample of each; groups without a reading (failed reads) are skipped.
func averages(ss []sample, group float64) (avg []float64, starts []int) {
	for i := 0; i < len(ss); {
		k := math.Floor(ss[i].T / group)
		j, sum := i, 0.0
		for j < len(ss) && math.Floor(ss[j].T/group) == k {
			sum += ss[j].I
			j++
		}
		avg = append(avg, sum/float64(j-i))
		starts = append(starts, i)
		i = j
	}
	return avg, starts
}

// runningMedian returns ss with each current replaced by the median of the
// readings around it: up to levelSmoothS seconds and levelSmoothN readings
// (an odd number, at least 3).
func runningMedian(ss []sample) []sample {
	out := slices.Clone(ss)
	if len(ss) < 3 {
		return out
	}
	step := (ss[len(ss)-1].T - ss[0].T) / float64(len(ss)-1)
	h := 1 // readings either side
	if step > 0 {
		h = max(1, min(levelSmoothN/2, int(levelSmoothS/step)/2))
	}
	win := make([]float64, 0, 2*h+1)
	for i := range ss {
		win = win[:0]
		for j := max(0, i-h); j <= min(len(ss)-1, i+h); j++ {
			win = append(win, ss[j].I)
		}
		slices.Sort(win)
		out[i].I = quantile(win, 0.5)
	}
	return out
}

// search adds ED-PELT's changes in avg[g0:g1] to cands (as sample indexes),
// over two sets of chunks half a chunk apart, so a change near the edge of
// one chunk (where ED-PELT sees little on one side) is in the middle of
// another.
func search(avg []float64, starts []int, g0, g1 int, cands map[int]bool) {
	for _, first := range []int{levelChunk, levelChunk / 2} { // length of the first chunk
		for c0 := g0; c0 < g1; {
			c1 := min(g1, c0+levelChunk)
			if c0 == g0 {
				c1 = min(g1, g0+first)
			}
			for _, c := range changepoint.NonParametric(avg[c0:c1], 1) {
				cands[starts[c0+c]] = true
			}
			c0 = c1
		}
	}
}

func candidateBounds(cands map[int]bool, n int) []int {
	bounds := []int{0}
	for b := range cands {
		if b > 0 && b < n {
			bounds = append(bounds, b)
		}
	}
	slices.Sort(bounds[1:])
	return append(bounds, n)
}

// level is a stretch of samples [a, b) and the sorted currents it is judged
// by: its own readings, or for a level grown from several persistent
// stretches, theirs only (never the short stretches absorbed between them).
type level struct {
	a, b  int
	judge []float64
}

func (l level) med() float64 { return quantile(l.judge, 0.5) }

// resolve turns candidate boundaries (sample indexes, from 0 to len(ss))
// into level boundaries:
//
//  1. (candidates sit on the averaging grid, so until step 5 a stretch
//     counts as lasting levelMinS with one averaging point of slack)
//  2. neighbouring candidate stretches that fail the size rule merge (the
//     pieces of a level the candidates cut too finely join up again);
//     stretches that then last levelMinS and hold levelMinReadings are
//     persistent; a run of shorter ones that lasts as long is a level of its
//     own (fast switching, or a level broken up by pulses); the shorter
//     runs are gaps;
//  3. neighbouring levels that fail the size rule merge, across any gap
//     between them, each judged by its own readings (never a gap's);
//  4. a gap left between two levels is split at the reading that best
//     separates their medians; at the window edges it joins its one
//     neighbour; with no level at all, the window is one level.
func resolve(ss []sample, bounds []int, group float64) []int {
	step := 0.0 // mean spacing of the readings
	if len(ss) > 1 {
		step = (ss[len(ss)-1].T - ss[0].T) / float64(len(ss)-1)
	}
	lasts := func(a, b int, slack float64) bool {
		return b-a >= levelMinReadings && ss[b-1].T-ss[a].T+step >= levelMinS-slack-1e-9
	}
	persistent := func(a, b int) bool { return lasts(a, b, group) }
	// holds reports whether a level holds its value: at least levelMinS of
	// readings (and levelMinReadings) within the size tolerance of its own
	// median. Pulses inside a level do not count toward it; a stretch around
	// a few pulses and a burst has a raised median but is not steady at it.
	holds := func(l level) bool {
		m := l.med()
		tol := math.Max(levelAbsA, levelRel*math.Abs(m))
		n := 0
		for i := l.a; i < l.b; i++ {
			if math.Abs(ss[i].I-m) <= tol {
				n++
			}
		}
		return n >= levelMinReadings && float64(n)*step >= levelMinS-1e-9
	}
	reach := 2 * group // how far step 5 moves a change
	own := func(a, b int) level {
		xs := values(ss[a:b], func(s sample) float64 { return s.I })
		slices.Sort(xs)
		return level{a, b, xs}
	}

	// 2. Like stretches join; then levels are the persistent stretches and
	// the runs of short ones that last.
	var pieces []level
	for k := 0; k+1 < len(bounds); k++ {
		pieces = append(pieces, own(bounds[k], bounds[k+1]))
	}
	pieces = mergeClose(pieces)
	var ls []level
	run := -1 // start of the current run of short stretches
	for _, p := range pieces {
		a, b := p.a, p.b
		if !persistent(a, b) {
			if run < 0 {
				run = a
			}
			continue
		}
		if run >= 0 && persistent(run, a) {
			ls = append(ls, own(run, a))
		}
		run = -1
		ls = append(ls, p)
	}
	if run >= 0 && persistent(run, len(ss)) {
		ls = append(ls, own(run, len(ss)))
	}
	for {
		if len(ls) == 0 {
			return []int{0, len(ss)}
		}
		// 3. The size rule.
		ls = mergeClose(ls)

		// 4. Gaps.
		ls[0].a = 0
		for k := 1; k < len(ls); k++ {
			if l, r := &ls[k-1], &ls[k]; r.a > l.b {
				s := split(ss, l.b, r.a, l.med(), r.med())
				l.b, r.a = s, s
			}
		}
		ls[len(ls)-1].b = len(ss)

		// 5. Each change to the reading where the current crosses between the
		// two levels' medians, as far as levelMinS either side (a run of short
		// stretches can carry fragments of its neighbours, and a change the
		// candidates missed can sit a few seconds from the nearest one); a
		// level that then no longer lasts becomes a gap, and 3-5 run again.
		for k := 1; k < len(ls); k++ {
			l, r := &ls[k-1], &ls[k]
			lo, hi := r.a, r.a
			for lo > l.a+1 && ss[r.a].T-ss[lo-1].T <= reach {
				lo--
			}
			for hi < r.b-1 && ss[hi].T-ss[r.a].T <= reach {
				hi++
			}
			s := split(ss, lo, hi, l.med(), r.med())
			l.b, r.a = s, s
		}
		var keep []level
		for _, l := range ls {
			if lasts(l.a, l.b, 0) && (len(ls) == 1 || holds(l)) {
				keep = append(keep, l)
			}
		}
		if len(keep) == len(ls) {
			break
		}
		ls = keep
	}

	out := []int{0}
	for _, l := range ls[1:] {
		out = append(out, l.a)
	}
	return append(out, len(ss))
}

// split returns the index s in [a, b] that leaves the fewest readings on the
// wrong side: in [a, s) closer to mr, in [s, b) closer to ml; among equal
// splits, the one nearest the middle.
func split(ss []sample, a, b int, ml, mr float64) int {
	cost := 0
	for i := a; i < b; i++ {
		if math.Abs(ss[i].I-ml) < math.Abs(ss[i].I-mr) {
			cost++
		}
	}
	best, at, mid := cost, a, (a+b)/2
	for s := a + 1; s <= b; s++ {
		x := ss[s-1].I
		if math.Abs(x-ml) < math.Abs(x-mr) {
			cost--
		} else if math.Abs(x-mr) < math.Abs(x-ml) {
			cost++
		}
		if cost < best || (cost == best && abs(s-mid) < abs(at-mid)) {
			best, at = cost, s
		}
	}
	return at
}

// mergeClose merges neighbouring levels whose medians differ by less than
// the size rule, the closest pair (relative to its threshold) first; a
// merged level spans both and is judged by both sets of readings.
func mergeClose(ls []level) []level {
	if len(ls) < 2 {
		return ls
	}
	str := make([]float64, len(ls)-1)
	for j := range str {
		str[j] = strength(ls[j], ls[j+1])
	}
	for {
		k, best := -1, 1.0
		for j, v := range str {
			if v < best {
				k, best = j, v
			}
		}
		if k < 0 {
			return ls
		}
		ls[k] = level{ls[k].a, ls[k+1].b, mergeSorted(ls[k].judge, ls[k+1].judge)}
		ls = slices.Delete(ls, k+1, k+2)
		str = slices.Delete(str, k, k+1)
		if k > 0 {
			str[k-1] = strength(ls[k-1], ls[k])
		}
		if k < len(str) {
			str[k] = strength(ls[k], ls[k+1])
		}
	}
}

// strength of the change between two neighbouring levels: below 1 it fails
// the size rule, or (for a level of few readings) the rank test.
func strength(l, r level) float64 {
	a, b := l.med(), r.med()
	st := math.Abs(a-b) / math.Max(levelAbsA, levelRel*math.Min(math.Abs(a), math.Abs(b)))
	if min(len(l.judge), len(r.judge)) < levelFewReadings {
		st = math.Min(st, math.Abs(rankZ(l.judge, r.judge))/levelMinZ)
	}
	return st
}

// rankZ is the Mann-Whitney U statistic of two sorted samples as a z score
// (normal approximation with the tie correction).
func rankZ(a, b []float64) float64 {
	na, nb := float64(len(a)), float64(len(b))
	n := na + nb
	if na == 0 || nb == 0 {
		return 0
	}
	u, ties := 0.0, 0.0 // U: pairs with a's value below b's, ties count half
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		// the next value and how many of each sample hold it
		v := math.Inf(1)
		if i < len(a) {
			v = a[i]
		}
		if j < len(b) && b[j] < v {
			v = b[j]
		}
		ta, tb := 0, 0
		for i < len(a) && a[i] == v {
			i++
			ta++
		}
		for j < len(b) && b[j] == v {
			j++
			tb++
		}
		// each of the tb values from b is above the i-ta values of a seen
		// before, and level with the ta equal ones
		u += float64(tb) * (float64(i-ta) + 0.5*float64(ta))
		t := float64(ta + tb)
		ties += t*t*t - t
	}
	v := na * nb / 12 * ((n + 1) - ties/(n*(n-1)))
	if v <= 0 {
		return 0
	}
	return (u - na*nb/2) / math.Sqrt(v)
}

func mergeSorted(a, b []float64) []float64 {
	m := make([]float64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			m = append(m, a[i])
			i++
		} else {
			m = append(m, b[j])
			j++
		}
	}
	return append(append(m, a[i:]...), b[j:]...)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
