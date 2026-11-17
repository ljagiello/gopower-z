package cli

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// This file turns the readings of a measure window into the summary that
// measure prints: per-field statistics, time bins and level shifts. It never
// talks to the meter, so it can be tested on synthetic series and reused for
// recorded logs.

// sample is one successful reading as the analysis sees it.
type sample struct {
	T      float64 // seconds since the start of the window
	V, I   float64 // VBUS volts, current amperes (signed like ibus_a)
	P      float64 // power watts (signed like power_w)
	DP, DM float64 // D+ and D- volts
	Temp   float64 // meter temperature °C, valid only if TempOK
	TempOK bool
}

type statJSON struct {
	Min   float64  `json:"min"`
	Mean  float64  `json:"mean"`
	Max   float64  `json:"max"`
	Last  float64  `json:"last"`
	Stdev float64  `json:"stdev"`
	P5    float64  `json:"p5"`
	P50   float64  `json:"p50"`
	P95   float64  `json:"p95"`
	CI95  *float64 `json:"ci95"`
}

type measureStats struct {
	VBusV  *statJSON `json:"vbus_v"`
	IBusA  *statJSON `json:"ibus_a"`
	PowerW *statJSON `json:"power_w"`
	TempC  *statJSON `json:"temp_c"`
	DPV    *statJSON `json:"dp_v"`
	DMV    *statJSON `json:"dm_v"`
}

// rangeJSON summarises one field inside one time bin.
type rangeJSON struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
}

type binJSON struct {
	TS      float64    `json:"t_s"`
	Samples int        `json:"samples"`
	VBusV   *rangeJSON `json:"vbus_v"`
	IBusA   *rangeJSON `json:"ibus_a"`
	PowerW  *rangeJSON `json:"power_w"`
}

type shiftJSON struct {
	TS        float64 `json:"t_s"`
	Field     string  `json:"field"`
	Kind      string  `json:"kind"`
	DurationS float64 `json:"duration_s"`
	From      float64 `json:"from"`
	To        float64 `json:"to"`
}

type analysis struct {
	Stats           measureStats
	BinS            float64
	Bins            []binJSON
	Shifts          []shiftJSON
	ShiftsTruncated bool
}

const (
	// ciBatches is how many equal-time batches the window is cut into for the
	// confidence interval of the mean; minCIBatches is how many must hold
	// samples, and minBatchS how long each must be, for an interval to be
	// reported. Batch means are close to independent only when a batch is
	// several times longer than the load's own fluctuations: in simulation,
	// 3 s batches keep coverage at 90-95% for loads correlated over up to 2 s.
	ciBatches    = 10
	minCIBatches = 8
	minBatchS    = 3.0
	// maxAutoBins bounds the automatic bin width; maxBins bounds --bin.
	maxAutoBins = 120
	maxBins     = 1000
	// maxShiftsPerField bounds each field's shift list (earliest kept).
	maxShiftsPerField = 32
	// shiftNoiseK scales the bin-to-bin noise into a shift threshold.
	shiftNoiseK = 6
	// minLevelBins is how long a level must last, in bins, to count.
	minLevelBins = 2
)

const (
	kindStep = "step"
	kindRamp = "ramp"
)

// shiftFields are the fields searched for level shifts, with the smallest
// change of the bin mean that counts as a shift and the decimals printed.
var shiftFields = []struct {
	name     string
	floor    float64
	decimals int
	get      func(sample) float64
}{
	{"vbus_v", 0.25, 6, func(s sample) float64 { return s.V }},
	{"ibus_a", 0.005, 6, func(s sample) float64 { return s.I }},
	{"dp_v", 0.2, 4, func(s sample) float64 { return s.DP }},
	{"dm_v", 0.2, 4, func(s sample) float64 { return s.DM }},
}

var shiftFieldNames = func() []string {
	n := make([]string, len(shiftFields))
	for i, f := range shiftFields {
		n[i] = f.name
	}
	return n
}()

// autoBin picks the bin width for a window: the smallest of a fixed ladder
// that spans at least five polling intervals and gives at most maxAutoBins
// bins.
func autoBin(window, interval time.Duration) time.Duration {
	ladder := []float64{0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 86400}
	for _, s := range ladder {
		bin := time.Duration(s * float64(time.Second))
		if bin >= 5*interval && math.Ceil(window.Seconds()/s) <= maxAutoBins {
			return bin
		}
	}
	return time.Duration(math.Ceil(window.Seconds()/maxAutoBins)) * time.Second
}

// analyze summarises the samples (in time order) of a window of the given
// length in seconds, with bins of binS seconds. Every slice in the result is
// non-nil.
func analyze(ss []sample, window, binS float64) analysis {
	ts := make([]float64, len(ss))
	for k, s := range ss {
		ts[k] = s.T
	}
	field := func(f func(sample) float64) []float64 {
		xs := make([]float64, len(ss))
		for k, s := range ss {
			xs[k] = f(s)
		}
		return xs
	}
	var temps, tempTs []float64
	for _, s := range ss {
		if s.TempOK {
			temps, tempTs = append(temps, s.Temp), append(tempTs, s.T)
		}
	}
	a := analysis{BinS: round(binS, 3), Bins: []binJSON{}, Shifts: []shiftJSON{}}
	a.Stats = measureStats{
		VBusV:  describe(field(func(s sample) float64 { return s.V }), ts, window, 6),
		IBusA:  describe(field(func(s sample) float64 { return s.I }), ts, window, 6),
		PowerW: describe(field(func(s sample) float64 { return s.P }), ts, window, 6),
		TempC:  describe(temps, tempTs, window, 3),
		DPV:    describe(field(func(s sample) float64 { return s.DP }), ts, window, 4),
		DMV:    describe(field(func(s sample) float64 { return s.DM }), ts, window, 4),
	}
	if len(ss) == 0 || binS <= 0 {
		return a
	}
	// Samples are in time order, so each bin is a contiguous run. The last
	// bin also takes samples read just after the end of the window.
	nb := max(1, int(math.Ceil(window/binS-1e-9)))
	bounds := make([]int, nb+1) // samples of bin k: ss[bounds[k]:bounds[k+1]]
	k := 0
	for b := 1; b <= nb; b++ {
		for k < len(ss) && (b == nb || ss[k].T < float64(b)*binS) {
			k++
		}
		bounds[b] = k
	}
	for b := range nb {
		run := ss[bounds[b]:bounds[b+1]]
		bin := binJSON{TS: round(float64(b)*binS, 3), Samples: len(run)}
		if len(run) > 0 {
			bin.VBusV = binRange(run, func(s sample) float64 { return s.V })
			bin.IBusA = binRange(run, func(s sample) float64 { return s.I })
			bin.PowerW = binRange(run, func(s sample) float64 { return s.P })
		}
		a.Bins = append(a.Bins, bin)
	}
	for _, f := range shiftFields {
		found := findShifts(ss, bounds, window, binS, f.name, f.floor, f.decimals, f.get)
		if len(found) > maxShiftsPerField {
			found, a.ShiftsTruncated = found[:maxShiftsPerField], true
		}
		a.Shifts = append(a.Shifts, found...)
	}
	slices.SortStableFunc(a.Shifts, func(x, y shiftJSON) int { return cmp.Compare(x.TS, y.TS) })
	return a
}

func binRange(run []sample, f func(sample) float64) *rangeJSON {
	lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
	for _, s := range run {
		v := f(s)
		lo, hi, sum = math.Min(lo, v), math.Max(hi, v), sum+v
	}
	return &rangeJSON{Min: round(lo, 6), Mean: round(sum/float64(len(run)), 6), Max: round(hi, 6)}
}

// describe computes the statistics of values xs taken at times ts (seconds
// into a window of the given length); nil when xs is empty.
func describe(xs, ts []float64, window float64, decimals int) *statJSON {
	n := len(xs)
	if n == 0 {
		return nil
	}
	mean := 0.0
	for _, x := range xs {
		mean += x
	}
	mean /= float64(n)
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	sd := 0.0
	if n > 1 {
		sd = math.Sqrt(ss / float64(n-1))
	}
	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	r := func(v float64) float64 { return round(v, decimals) }
	st := &statJSON{Min: r(sorted[0]), Mean: r(mean), Max: r(sorted[n-1]), Last: r(xs[n-1]), Stdev: r(sd),
		P5: r(quantile(sorted, 0.05)), P50: r(quantile(sorted, 0.5)), P95: r(quantile(sorted, 0.95))}
	if ci, ok := batchCI(xs, ts, window); ok {
		st.CI95 = new(r(ci))
	}
	return st
}

// quantile interpolates linearly between order statistics (sorted input).
func quantile(sorted []float64, q float64) float64 {
	h := q * float64(len(sorted)-1)
	lo := int(math.Floor(h))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// batchCI is the half-width of an approximate 95% confidence interval for
// the mean, from the means of ciBatches equal-time batches (the batch-means
// method). Readings close in time are correlated, so s/sqrt(n) over raw
// samples would shrink just by polling faster; batches of fixed length do
// not. Batch means are only close to independent when batches are long
// compared with how slowly the load itself wanders, so no interval is
// claimed for batches under minBatchS, or when consecutive batch means are
// correlated (lag-1 autocorrelation above 2/sqrt(m): a level that changed or
// drifted during the window fails it). It covers sampling variation over the
// window, not the meter's accuracy.
func batchCI(xs, ts []float64, window float64) (float64, bool) {
	if window/ciBatches < minBatchS {
		return 0, false
	}
	var sum [ciBatches]float64
	var cnt [ciBatches]int
	for k, x := range xs {
		b := min(ciBatches-1, max(0, int(ts[k]/window*ciBatches)))
		sum[b] += x
		cnt[b]++
	}
	var means []float64
	for b := range ciBatches {
		if cnt[b] > 0 {
			means = append(means, sum[b]/float64(cnt[b]))
		}
	}
	m := len(means)
	if m < minCIBatches {
		return 0, false
	}
	mu := 0.0
	for _, x := range means {
		mu += x
	}
	mu /= float64(m)
	v, lag := 0.0, 0.0
	for i, x := range means {
		v += (x - mu) * (x - mu)
		if i > 0 {
			lag += (x - mu) * (means[i-1] - mu)
		}
	}
	if v > 0 && lag/v > 2/math.Sqrt(float64(m)) {
		return 0, false
	}
	return tCrit975[m-1] * math.Sqrt(v/float64(m-1)/float64(m)), true
}

// tCrit975 holds the 97.5% quantile of Student's t for 0..ciBatches-1 degrees
// of freedom (index = df; only df >= minCIBatches-1 is used).
var tCrit975 = [ciBatches]float64{0, 12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262}

// level is a run of samples ss[lo:hi] at one level of a field.
type level struct{ lo, hi int }

// findShifts reports lasting changes of a field's level in four passes:
//
//  1. Candidates on bin means. A 3-bin running median removes one-bin
//     bursts; a bin starts a new level when it and the next bin both differ
//     from the current level by more than the threshold and agree with each
//     other. The threshold is the larger of the field's floor and
//     shiftNoiseK times the bin-to-bin noise (a robust estimate from the
//     median absolute bin-to-bin difference).
//  2. Each boundary moves to the sample where the change happened, searched
//     from the last non-empty bin before the new level.
//  3. Levels shorter than minLevelBins bins are merged away: a burst that
//     straddles a bin edge raises two bins and survives step 1, but it is
//     not a lasting level.
//  4. A run of levels that each drift in one direction, joined by shifts in
//     that direction, is one ramp, not a staircase of steps.
func findShifts(ss []sample, bounds []int, window, binS float64, field string, floor float64, decimals int, f func(sample) float64) []shiftJSON {
	type bin struct {
		idx  int
		mean float64
	}
	var bins []bin
	for b := 0; b+1 < len(bounds); b++ {
		if bounds[b+1] > bounds[b] {
			bins = append(bins, bin{b, meanOf(ss, bounds[b], bounds[b+1], f)})
		}
	}
	if len(bins) < 3 {
		return nil
	}
	diffs := make([]float64, len(bins)-1)
	for k := range diffs {
		diffs[k] = math.Abs(bins[k+1].mean - bins[k].mean)
	}
	slices.Sort(diffs)
	noise := 1.4826 * quantile(diffs, 0.5) / math.Sqrt2
	thr := math.Max(floor, shiftNoiseK*noise)

	// 1. Candidate levels on the smoothed bin means; starts holds positions
	// in bins where each level begins.
	smooth := make([]float64, len(bins))
	for k := range bins {
		c := min(max(k, 1), len(bins)-2)
		w := []float64{bins[c-1].mean, bins[c].mean, bins[c+1].mean}
		slices.Sort(w)
		smooth[k] = w[1]
	}
	starts := []int{0}
	levelSum, levelN := smooth[0], 1
	for k := 1; k < len(bins); k++ {
		level := levelSum / float64(levelN)
		cur := smooth[k]
		if k+1 < len(bins) && math.Abs(cur-level) > thr {
			next := smooth[k+1]
			if math.Abs(next-level) > thr && math.Abs(next-cur) <= thr && (next-level)*(cur-level) > 0 {
				starts = append(starts, k)
				levelSum, levelN = cur, 1
				continue
			}
		}
		levelSum += cur
		levelN++
	}
	if len(starts) == 1 {
		return nil
	}

	// 2. Locate each boundary on the samples. The provisional levels are
	// whole bins; the split minimises the squared error of "from before, to
	// after", and moving it one sample right changes the cost by one term.
	end := func(k int) int { // sample index where provisional level k ends
		if k+1 < len(starts) {
			return bounds[bins[starts[k+1]].idx]
		}
		return len(ss)
	}
	splits := make([]int, len(starts)+1)
	splits[0], splits[len(starts)] = bounds[bins[0].idx], len(ss)
	for k := 1; k < len(starts); k++ {
		from := meanOf(ss, bounds[bins[starts[k-1]].idx], end(k-1), f)
		to := meanOf(ss, bounds[bins[starts[k]].idx], end(k), f)
		first := bins[starts[k]].idx
		lo, hi := bounds[bins[starts[k]-1].idx], bounds[first+1]
		cost := 0.0
		for j := lo; j < hi; j++ {
			cost += (f(ss[j]) - to) * (f(ss[j]) - to)
		}
		split, best := lo, cost
		for s := lo; s < hi; s++ {
			x := f(ss[s])
			cost += (x-from)*(x-from) - (x-to)*(x-to)
			if cost < best {
				split, best = s+1, cost
			}
		}
		// Every level keeps at least one sample on each side of its split.
		splits[k] = min(max(split, splits[k-1]+1), hi-1)
	}
	levels := make([]level, len(starts))
	for k := range levels {
		levels[k] = level{splits[k], splits[k+1]}
	}

	// 3. Merge levels that do not last minLevelBins bins. A short level at
	// an edge joins its neighbour. In the middle, a short level that returns
	// to where it came from is a burst and all three merge; one between two
	// different levels is part of the transition and joins the next level.
	duration := func(l level) float64 {
		stop := window
		if l.hi < len(ss) {
			stop = ss[l.hi].T
		}
		return stop - ss[l.lo].T
	}
	for i := 0; i < len(levels) && len(levels) > 1; {
		if duration(levels[i]) >= minLevelBins*binS {
			i++
			continue
		}
		switch {
		case i == 0:
			levels[1].lo = levels[0].lo
			levels = slices.Delete(levels, 0, 1)
		case i == len(levels)-1:
			levels[i-1].hi = levels[i].hi
			levels = slices.Delete(levels, i, i+1)
			i--
		case math.Abs(meanOf(ss, levels[i-1].lo, levels[i-1].hi, f)-meanOf(ss, levels[i+1].lo, levels[i+1].hi, f)) > thr:
			levels[i+1].lo = levels[i].lo
			levels = slices.Delete(levels, i, i+1)
		default:
			levels[i-1].hi = levels[i+1].hi
			levels = slices.Delete(levels, i, i+2)
			i--
		}
		i = max(i, 0)
	}
	if len(levels) < 2 {
		return nil
	}

	// 4. Steps and ramps. drift is the sign of a level's change between its
	// first and second half (0 when it is flat). A ramp cut into a staircase
	// drifts within each level by about half the step between levels; a flat
	// level drifts by noise only. So a level drifts when the change is at
	// least 30% of its smaller neighbouring step and three standard errors
	// of the bin noise.
	means := make([]float64, len(levels))
	for k, l := range levels {
		means[k] = meanOf(ss, l.lo, l.hi, f)
	}
	drift := make([]float64, len(levels))
	for k, l := range levels {
		mid := (ss[l.lo].T + ss[l.hi-1].T) / 2
		h := l.lo
		for h < l.hi && ss[h].T < mid {
			h++
		}
		if h-l.lo < 2 || l.hi-h < 2 {
			continue
		}
		d := meanOf(ss, h, l.hi, f) - meanOf(ss, l.lo, h, f)
		step := math.Inf(1)
		if k > 0 {
			step = math.Abs(means[k] - means[k-1])
		}
		if k+1 < len(levels) {
			step = math.Min(step, math.Abs(means[k+1]-means[k]))
		}
		halfBins := math.Max(1, (ss[l.hi-1].T-ss[l.lo].T)/2/binS)
		if math.Abs(d) >= 0.3*step && math.Abs(d) > 3*noise*math.Sqrt(2/halfBins) {
			drift[k] = math.Copysign(1, d)
		}
	}
	at := func(i int) float64 { return ss[min(i, len(ss)-1)].T }
	// edgeMean is the mean of the first or last bin's worth of a level.
	edgeMean := func(l level, last bool) float64 {
		if last {
			lo := l.hi - 1
			for lo > l.lo && ss[l.hi-1].T-ss[lo-1].T < binS {
				lo--
			}
			return meanOf(ss, lo, l.hi, f)
		}
		hi := l.lo + 1
		for hi < l.hi && ss[hi].T-ss[l.lo].T < binS {
			hi++
		}
		return meanOf(ss, l.lo, hi, f)
	}
	r := func(v float64) float64 { return round(v, decimals) }
	var out []shiftJSON
	for k := 1; k < len(levels); {
		dir := math.Copysign(1, means[k]-means[k-1])
		// Extend over boundaries in the same direction while a level on
		// either side drifts that way.
		j := k
		for j < len(levels) && math.Copysign(1, means[j]-means[j-1]) == dir && (drift[j-1] == dir || drift[j] == dir) {
			j++
		}
		if j == k { // a step
			out = append(out, shiftJSON{TS: r3(at(levels[k].lo)), Field: field, Kind: kindStep, From: r(means[k-1]), To: r(means[k])})
			k++
			continue
		}
		// A ramp over boundaries k..j-1: it runs through the drifting levels
		// between them, starting at the first drifting level (or the first
		// boundary) and ending with the last.
		first, last := k, j-2
		if drift[k-1] == dir {
			first = k - 1
		}
		if drift[j-1] == dir {
			last = j - 1
		}
		start, stop := at(levels[first].lo), at(levels[last].hi)
		if levels[last].hi >= len(ss) {
			stop = window
		}
		from, to := means[k-1], means[j-1]
		if first == k-1 {
			from = edgeMean(levels[first], false)
		}
		if last == j-1 {
			to = edgeMean(levels[last], true)
		}
		out = append(out, shiftJSON{TS: r3(start), Field: field, Kind: kindRamp, DurationS: r3(stop - start), From: r(from), To: r(to)})
		k = j
	}
	return out
}

func meanOf(ss []sample, lo, hi int, f func(sample) float64) float64 {
	sum := 0.0
	for _, s := range ss[lo:hi] {
		sum += f(s)
	}
	return sum / float64(hi-lo)
}

func r3(v float64) float64 { return round(v, 3) }
