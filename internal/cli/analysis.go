package cli

import (
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
	TS    float64 `json:"t_s"`
	Field string  `json:"field"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
}

type analysis struct {
	Stats  measureStats
	BinS   float64
	Bins   []binJSON
	Shifts []shiftJSON
}

const (
	// ciBatches is how many equal-time batches the window is cut into for the
	// confidence interval of the mean; minCIBatches is how many must hold
	// samples for an interval to be reported.
	ciBatches    = 20
	minCIBatches = 10
	// maxAutoBins bounds the automatic bin width; maxBins bounds --bin.
	maxAutoBins = 120
	maxBins     = 1000
	// maxShifts bounds the shift list (earliest first).
	maxShifts = 32
	// shiftNoiseK scales the bin-to-bin noise into a shift threshold.
	shiftNoiseK = 6
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
// that keeps at least five samples per bin and at most maxAutoBins bins.
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

// analyze summarises the samples of a window of the given length (seconds)
// with bins of binS seconds. meterPeriod is the time between the meter's own
// samples (0 if unknown); ci95 needs batches at least that long. Every slice
// in the result is non-nil.
func analyze(ss []sample, window, binS, meterPeriod float64) analysis {
	get := func(f func(sample) float64) ([]float64, []float64) {
		xs, ts := make([]float64, len(ss)), make([]float64, len(ss))
		for k, s := range ss {
			xs[k], ts[k] = f(s), s.T
		}
		return xs, ts
	}
	var temps, tempTs []float64
	for _, s := range ss {
		if s.TempOK {
			temps, tempTs = append(temps, s.Temp), append(tempTs, s.T)
		}
	}
	a := analysis{BinS: round(binS, 3), Bins: []binJSON{}, Shifts: []shiftJSON{}}
	// With batches shorter than the meter's sample period, neighbouring
	// batches share readings and the interval would come out too narrow.
	ciWindow := window
	if window/ciBatches < meterPeriod {
		ciWindow = 0
	}
	a.Stats = measureStats{
		VBusV:  describe(ciWindow, 6)(get(func(s sample) float64 { return s.V })),
		IBusA:  describe(ciWindow, 6)(get(func(s sample) float64 { return s.I })),
		PowerW: describe(ciWindow, 6)(get(func(s sample) float64 { return s.P })),
		TempC:  describe(ciWindow, 3)(temps, tempTs),
		DPV:    describe(ciWindow, 4)(get(func(s sample) float64 { return s.DP })),
		DMV:    describe(ciWindow, 4)(get(func(s sample) float64 { return s.DM })),
	}
	if len(ss) == 0 || binS <= 0 {
		return a
	}
	// Samples are in time order, so each bin is a contiguous run.
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
		a.Shifts = append(a.Shifts, findShifts(ss, bounds, f.name, f.floor, f.decimals, f.get)...)
	}
	slices.SortStableFunc(a.Shifts, func(x, y shiftJSON) int {
		switch {
		case x.TS < y.TS:
			return -1
		case x.TS > y.TS:
			return 1
		}
		return 0
	})
	if len(a.Shifts) > maxShifts {
		a.Shifts = a.Shifts[:maxShifts]
	}
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

// describe returns a function computing the statistics of values xs taken at
// times ts (seconds into a window of the given length; 0 = no ci95); nil
// when xs is empty.
func describe(window float64, decimals int) func(xs, ts []float64) *statJSON {
	return func(xs, ts []float64) *statJSON {
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

// batchCI is the half-width of a 95% confidence interval for the mean, from
// the means of ciBatches equal-time batches (the batch-means method). Readings
// close in time are correlated (the meter's own sample rate bounds how often
// a value changes), so s/sqrt(n) over raw samples would shrink just by polling
// faster; batches of fixed length do not. It covers sampling variation over
// the window, not the meter's accuracy.
func batchCI(xs, ts []float64, window float64) (float64, bool) {
	if window <= 0 {
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
	v := 0.0
	for _, x := range means {
		v += (x - mu) * (x - mu)
	}
	v /= float64(m - 1)
	return tCrit975[m-1] * math.Sqrt(v/float64(m)), true
}

// tCrit975 holds the 97.5% quantile of Student's t for 0..ciBatches-1 degrees
// of freedom (index = df; only df >= minCIBatches-1 is used).
var tCrit975 = [ciBatches]float64{0, 12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262,
	2.228, 2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093}

// findShifts reports lasting changes of a field's level. It works on bin
// means: a bin starts a new level when it and the following bin both differ
// from the current level by more than the threshold and agree with each
// other, so a burst shorter than two bins is ignored. The threshold is the
// larger of the field's floor and shiftNoiseK times the bin-to-bin noise
// (a robust estimate from the median absolute bin-to-bin difference). The
// moment of each shift is then located on the samples around it, and from/to
// are the means of the samples on either side up to the neighbouring shifts.
func findShifts(ss []sample, bounds []int, field string, floor float64, decimals int, f func(sample) float64) []shiftJSON {
	type bin struct {
		idx  int
		mean float64
	}
	var bins []bin
	for b := 0; b+1 < len(bounds); b++ {
		run := ss[bounds[b]:bounds[b+1]]
		if len(run) == 0 {
			continue
		}
		sum := 0.0
		for _, s := range run {
			sum += f(s)
		}
		bins = append(bins, bin{b, sum / float64(len(run))})
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

	// A 3-bin running median removes one-bin bursts (which would otherwise
	// pull the level, or start it wrong when they land in the first bin) and
	// keeps steps intact. The edges use the nearest full window.
	smooth := make([]float64, len(bins))
	for k := range bins {
		c := min(max(k, 1), len(bins)-2)
		w := []float64{bins[c-1].mean, bins[c].mean, bins[c+1].mean}
		slices.Sort(w)
		smooth[k] = w[1]
	}

	// starts holds the bin index where each level begins.
	starts := []int{bins[0].idx}
	levelSum, levelN := smooth[0], 1
	for k := 1; k < len(bins); k++ {
		level := levelSum / float64(levelN)
		cur := smooth[k]
		if k+1 < len(bins) && math.Abs(cur-level) > thr {
			next := smooth[k+1]
			if math.Abs(next-level) > thr && math.Abs(next-cur) <= thr && (next-level)*(cur-level) > 0 {
				starts = append(starts, bins[k].idx)
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
	// Each level as whole bins first; then each boundary moves to the sample
	// where the step happened (searched in the bin before and the first bin
	// of the new level), and from/to are the means between those samples.
	ends := append(slices.Clone(starts[1:]), len(bounds)-1)
	mean := func(lo, hi int) float64 {
		sum := 0.0
		for _, s := range ss[lo:hi] {
			sum += f(s)
		}
		return sum / float64(hi-lo)
	}
	splits := make([]int, len(starts)+1) // samples of level k: ss[splits[k]:splits[k+1]]
	splits[0], splits[len(starts)] = bounds[starts[0]], bounds[ends[len(starts)-1]]
	for k := 1; k < len(starts); k++ {
		from := mean(bounds[starts[k-1]], bounds[ends[k-1]])
		to := mean(bounds[starts[k]], bounds[ends[k]])
		// The split minimises the squared error of "from before, to after";
		// moving it one sample right changes the cost by one term.
		lo, hi := bounds[max(starts[k]-1, starts[k-1])], bounds[starts[k]+1]
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
		splits[k] = min(max(split, splits[k-1]+1), bounds[starts[k]+1]-1)
	}
	out := make([]shiftJSON, 0, len(starts)-1)
	for k := 1; k < len(starts); k++ {
		out = append(out, shiftJSON{TS: round(ss[splits[k]].T, 3), Field: field,
			From: round(mean(splits[k-1], splits[k]), decimals), To: round(mean(splits[k], splits[k+1]), decimals)})
	}
	return out
}
