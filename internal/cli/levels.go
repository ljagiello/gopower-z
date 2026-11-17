package cli

import (
	"math"
	"slices"

	"pgregory.net/changepoint"
)

// Levels: the stretches of a measure window in which the current holds a
// steady typical value. A charger that switches on partway shows as two
// levels; a device that pulses every few seconds stays one level; a load
// that switches on and off for tens of seconds at a time shows every phase.
//
// Candidate changes come from ED-PELT (Haynes, Fearnhead & Eckley, "A
// computationally efficient nonparametric approach for changepoint
// detection", 2017), a published method from pgregory.net/changepoint, run
// on 1 s averages of the current (window/levelPoints on very long windows)
// in chunks of levelChunk points: its cost is quadratic when nothing
// changes, and its penalty per change is paid more easily with few changes
// per run, so a long on/off pattern is not seen as one mixed level. It
// treats readings as independent, so on a pulsing load it proposes many
// tiny changes; rules turn its candidates into levels a person would name:
//
//   - persistence: a level lasts at least levelMinS seconds (window /
//     levelMaxCount on windows over 5000 s, which bounds the output) and
//     holds at least levelMinReadings readings, so bursts, boot spikes and
//     single slow reads stay inside the level around them, at the window
//     edges too;
//   - size: neighbouring levels differ in median current by at least
//     levelRel of the smaller one, and at least levelAbsA;
//   - evidence: their readings differ clearly (Mann-Whitney rank test,
//     |z| >= levelMinZ), so at slow polling a few readings that happen to
//     catch a pulsing load's pulses do not make a level.
//
// Each change is then moved to the reading where the current crosses from
// one level to the other, and the rules are applied once more.
//
// Tuned on real captures in testdata/: a 4 s pulse (no change in any
// window, at any polling up to 10 s), an ESP32 parked in its bootloader
// (65 -> 61 mA for 7 s, found both times) and a board reboot (48 -> 65 mA).
const (
	levelMinS        = 5.0    // seconds a level must last
	levelMinReadings = 5      // readings a level must hold
	levelRel         = 0.05   // neighbouring medians differ by at least 5% ...
	levelAbsA        = 0.002  // ... and at least 2 mA
	levelPoints      = 20_000 // averaging points at most (1 s up to 20,000 s of window)
	levelChunk       = 30     // ED-PELT input points per run
	levelMinZ        = 3.0    // rank test between neighbouring levels
	levelMaxCount    = 1000   // levels at most: persistence is at least window/levelMaxCount
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

	// Average the current over groups of `group` seconds; groups without a
	// reading (failed reads) are skipped. starts[g] is the first sample of
	// point g.
	var avg []float64
	var starts []int
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
	// Candidates: ED-PELT's changes in each chunk, over two sets of chunks
	// half a chunk apart, so a change near the edge of one chunk (where
	// ED-PELT sees little on one side) is in the middle of another.
	found := map[int]bool{}
	for _, first := range []int{levelChunk, levelChunk / 2} { // length of the first chunk
		for c0 := 0; c0 < len(avg); {
			c1 := min(len(avg), c0+levelChunk)
			if c0 == 0 {
				c1 = min(len(avg), first)
			}
			for _, c := range changepoint.NonParametric(avg[c0:c1], 1) {
				found[starts[c0+c]] = true
			}
			c0 = c1
		}
	}
	bounds := []int{0}
	for b := range found {
		bounds = append(bounds, b)
	}
	slices.Sort(bounds[1:])
	bounds = append(bounds, len(ss))

	// ED-PELT's changes sit on the averaging grid, so the first pass allows
	// a point of slack either side before the persistence rule; after the
	// changes move to the reading, the rule applies exactly.
	lv := newLevelSet(ss, bounds, window)
	lv.settle(2 * group)
	lv.refine()
	lv.settle(0)

	for k := 0; k+1 < len(lv.bounds); k++ {
		run := ss[lv.bounds[k]:lv.bounds[k+1]]
		out = append(out, levelJSON{
			StartS: round(lv.start(k), 3), EndS: round(lv.end(k), 3), Samples: len(run),
			VBusV:  binRange(run, func(s sample) float64 { return s.V }),
			IBusA:  describe(values(run, func(s sample) float64 { return s.I }), 6),
			PowerW: binRange(run, func(s sample) float64 { return s.P }),
		})
	}
	return out
}

// levelSet holds candidate levels as sample boundaries (bounds[k] is the
// first sample of level k, the last entry is len(ss)), with each level's
// currents kept sorted, so a merge is a linear merge of two sorted runs and
// a median is a lookup.
type levelSet struct {
	ss     []sample
	window float64
	minS   float64 // seconds a level must last in this window
	step   float64 // mean spacing of the readings, seconds
	bounds []int
	sorted [][]float64
}

func newLevelSet(ss []sample, bounds []int, window float64) *levelSet {
	l := &levelSet{ss: ss, window: window, bounds: bounds, minS: math.Max(levelMinS, window/levelMaxCount)}
	if len(ss) > 1 {
		l.step = (ss[len(ss)-1].T - ss[0].T) / float64(len(ss)-1)
	}
	for k := 0; k+1 < len(bounds); k++ {
		l.sorted = append(l.sorted, l.currents(bounds[k], bounds[k+1]))
	}
	return l
}

func (l *levelSet) currents(a, b int) []float64 {
	xs := values(l.ss[a:b], func(s sample) float64 { return s.I })
	slices.Sort(xs)
	return xs
}

func (l *levelSet) med(k int) float64 { return quantile(l.sorted[k], 0.5) }

// start and end are level k's times as reported: the first level starts
// with the window, the last ends with it, and the others meet at a level's
// first reading.
func (l *levelSet) start(k int) float64 {
	if k == 0 {
		return 0
	}
	return l.ss[l.bounds[k]].T
}

func (l *levelSet) end(k int) float64 {
	if k+2 >= len(l.bounds) {
		return l.window
	}
	return l.ss[l.bounds[k+1]].T
}

// span is how long level k's own readings cover: from its first reading to
// one reading interval past its last, so a run of failed reads after a
// level does not count toward it.
func (l *levelSet) span(k int) float64 {
	return l.ss[l.bounds[k+1]-1].T - l.ss[l.bounds[k]].T + l.step
}

// short reports whether level k fails the persistence rule, with slack
// seconds of tolerance.
func (l *levelSet) short(k int, slack float64) bool {
	return l.span(k) < l.minS-slack-1e-9 || l.bounds[k+1]-l.bounds[k] < levelMinReadings
}

// join merges level k with level k+1.
func (l *levelSet) join(k int) {
	a, b := l.sorted[k], l.sorted[k+1]
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
	m = append(append(m, a[i:]...), b[j:]...)
	l.sorted[k] = m
	l.sorted = slices.Delete(l.sorted, k+1, k+2)
	l.bounds = slices.Delete(l.bounds, k+1, k+2)
}

func (l *levelSet) settle(slack float64) {
	l.persist(slack)
	l.mergeClose()
}

// persist merges levels that fail the persistence rule, shortest first,
// into the neighbour whose median is closer.
func (l *levelSet) persist(slack float64) {
	for len(l.sorted) > 1 {
		k, least := -1, math.Inf(1)
		for j := range l.sorted {
			if d := l.span(j); l.short(j, slack) && d < least {
				k, least = j, d
			}
		}
		if k < 0 {
			return
		}
		switch {
		case k == 0:
			l.join(0)
		case k == len(l.sorted)-1:
			l.join(k - 1)
		case math.Abs(l.med(k)-l.med(k-1)) <= math.Abs(l.med(k)-l.med(k+1)):
			l.join(k - 1)
		default:
			l.join(k)
		}
	}
}

// mergeClose merges neighbouring levels that fail the size or the evidence
// rule, the weakest pair first.
func (l *levelSet) mergeClose() {
	if len(l.sorted) < 2 {
		return
	}
	str := make([]float64, len(l.sorted)-1) // strength of each neighbouring pair
	for j := range str {
		str[j] = l.strength(j)
	}
	for {
		k, best := -1, 1.0
		for j, v := range str {
			if v < best {
				k, best = j, v
			}
		}
		if k < 0 {
			return
		}
		l.join(k)
		str = slices.Delete(str, k, k+1)
		if k > 0 {
			str[k-1] = l.strength(k - 1)
		}
		if k < len(str) {
			str[k] = l.strength(k)
		}
	}
}

// strength of the change between levels j and j+1: below 1 it fails the
// size rule or the evidence rule.
func (l *levelSet) strength(j int) float64 {
	a, b := l.med(j), l.med(j+1)
	thr := math.Max(levelAbsA, levelRel*math.Min(math.Abs(a), math.Abs(b)))
	return math.Min(math.Abs(a-b)/thr, math.Abs(rankZ(l.sorted[j], l.sorted[j+1]))/levelMinZ)
}

// rankZ is the Mann-Whitney U statistic of two sorted samples as a z score
// (normal approximation, no tie correction, which errs toward merging).
func rankZ(a, b []float64) float64 {
	na, nb := float64(len(a)), float64(len(b))
	u := 0.0 // pairs with the value from a below the value from b, ties count half
	i, j := 0, 0
	for _, y := range b {
		for i < len(a) && a[i] < y {
			i++
		}
		j = max(j, i)
		for j < len(a) && a[j] <= y {
			j++
		}
		u += float64(i) + 0.5*float64(j-i)
	}
	sd := math.Sqrt(na * nb * (na + nb + 1) / 12)
	if sd == 0 {
		return 0
	}
	return (u - na*nb/2) / sd
}

// refine moves each change to the reading where the current crosses from
// one level to the next: within two averaging points of where ED-PELT put
// it, the split that leaves the fewest readings closer to the other
// level's median.
func (l *levelSet) refine() {
	if len(l.bounds) <= 2 {
		return
	}
	reach := 2 * math.Max(1, l.window/levelPoints) // seconds either side
	for k := 1; k+1 < len(l.bounds); k++ {
		ma, mb := l.med(k-1), l.med(k)
		b := l.bounds[k]
		lo, hi := b, b
		for lo > l.bounds[k-1]+1 && l.ss[b].T-l.ss[lo-1].T <= reach {
			lo--
		}
		for hi < l.bounds[k+1]-1 && l.ss[hi].T-l.ss[b].T <= reach {
			hi++
		}
		// cost(s): readings in [lo,s) closer to mb plus readings in [s,hi)
		// closer to ma.
		cost := 0
		for i := lo; i < hi; i++ {
			if math.Abs(l.ss[i].I-ma) < math.Abs(l.ss[i].I-mb) {
				cost++
			}
		}
		best, at := cost, lo
		for s := lo + 1; s <= hi; s++ {
			x := l.ss[s-1].I
			if math.Abs(x-ma) < math.Abs(x-mb) {
				cost--
			} else if math.Abs(x-mb) < math.Abs(x-ma) {
				cost++
			}
			if cost < best || (cost == best && abs(s-b) < abs(at-b)) {
				best, at = cost, s
			}
		}
		l.bounds[k] = at
	}
	// Medians follow the moved boundaries.
	for k := 0; k+1 < len(l.bounds); k++ {
		l.sorted[k] = l.currents(l.bounds[k], l.bounds[k+1])
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
