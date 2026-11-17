package cli

import (
	"math"
	"slices"

	"pgregory.net/changepoint"
)

// Levels: the stretches of a measure window in which the current holds a
// steady typical value. A charger that switches on partway shows as two
// levels; a device that pulses every few seconds stays one level.
//
// Changes are found with ED-PELT (Haynes, Fearnhead & Eckley, "A
// computationally efficient nonparametric approach for changepoint
// detection", 2017), a published method from pgregory.net/changepoint, run
// on short averages of the current. On its own it treats readings as
// independent, so on a pulsing load it reports many tiny changes; two rules
// turn its output into levels a person would name:
//
//   - persistence: a level lasts at least levelMinS seconds, so bursts and
//     boot spikes stay inside the level around them, at the edges of the
//     window too;
//   - size: neighbouring levels differ in median current by at least
//     levelRel of the smaller one, and at least levelAbsA.
//
// Tuned on real captures in testdata/: a 4 s pulse (no change in 1320
// windows), an ESP32 parked in its bootloader (65 -> 61 mA for 7 s, found
// both times) and a board reboot (48 -> 65 mA).
const (
	levelMinS   = 5.0   // seconds a level must last
	levelRel    = 0.05  // neighbouring medians differ by at least 5% ...
	levelAbsA   = 0.002 // ... and at least 2 mA
	levelPoints = 1000  // ED-PELT input points at most (its cost is quadratic when nothing changes)
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
// there are no samples, one level when nothing changed.
func levels(ss []sample, window float64) []levelJSON {
	out := []levelJSON{}
	if len(ss) == 0 {
		return out
	}
	// Readings taken just after the end of the window (measure's last read)
	// belong to the last level, as they do to the last bin.
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
	bounds := []int{0} // sample index where each level starts, then len(ss)
	if len(avg) >= 3 {
		for _, c := range changepoint.NonParametric(avg, 1) {
			bounds = append(bounds, starts[c])
		}
	}
	bounds = append(bounds, len(ss))

	cur := values(ss, func(s sample) float64 { return s.I })
	lv := newLevelSet(ss, cur, bounds, window)
	lv.persist(levelMinS)
	lv.mergeClose()

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

// levelSet holds candidate levels as sample boundaries, with each level's
// median current cached (recomputed only when two levels merge).
type levelSet struct {
	ss     []sample
	cur    []float64
	window float64
	bounds []int
	med    []float64
}

func newLevelSet(ss []sample, cur []float64, bounds []int, window float64) *levelSet {
	l := &levelSet{ss: ss, cur: cur, window: window, bounds: bounds}
	for k := 0; k+1 < len(bounds); k++ {
		l.med = append(l.med, l.median(k))
	}
	return l
}

func (l *levelSet) median(k int) float64 {
	xs := slices.Clone(l.cur[l.bounds[k]:l.bounds[k+1]])
	slices.Sort(xs)
	return quantile(xs, 0.5)
}

// start and end are level k's times: the first level starts with the window,
// the last ends with it, and the others meet at a level's first reading.
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

// join merges level k with level k+1.
func (l *levelSet) join(k int) {
	l.bounds = slices.Delete(l.bounds, k+1, k+2)
	l.med = slices.Delete(l.med, k+1, k+2)
	l.med[k] = l.median(k)
}

// persist merges levels shorter than minLevel seconds, shortest first, into
// the neighbour whose median is closer.
func (l *levelSet) persist(minLevel float64) {
	for len(l.med) > 1 {
		k, short := -1, minLevel
		for j := range l.med {
			if d := l.end(j) - l.start(j); d < short {
				k, short = j, d
			}
		}
		if k < 0 {
			return
		}
		switch {
		case k == 0:
			l.join(0)
		case k == len(l.med)-1:
			l.join(k - 1)
		case math.Abs(l.med[k]-l.med[k-1]) <= math.Abs(l.med[k]-l.med[k+1]):
			l.join(k - 1)
		default:
			l.join(k)
		}
	}
}

// mergeClose merges neighbouring levels whose medians differ by less than
// the size rule, the closest pair (relative to its threshold) first.
func (l *levelSet) mergeClose() {
	for {
		k, best := -1, 1.0
		for j := 0; j+1 < len(l.med); j++ {
			a, b := l.med[j], l.med[j+1]
			thr := math.Max(levelAbsA, levelRel*math.Min(math.Abs(a), math.Abs(b)))
			if r := math.Abs(a-b) / thr; r < best {
				k, best = j, r
			}
		}
		if k < 0 {
			return
		}
		l.join(k)
	}
}
