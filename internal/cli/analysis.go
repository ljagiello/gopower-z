package cli

import (
	"math"
	"slices"
	"time"

	"gonum.org/v1/gonum/floats"
	gstat "gonum.org/v1/gonum/stat"
)

// This file turns the readings of a measure window into the summary that
// measure prints: per-field statistics, time bins and levels (levels.go). It
// never talks to the meter, so it can be tested on synthetic series and on
// recorded captures.

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
	Min   float64 `json:"min"`
	Mean  float64 `json:"mean"`
	Max   float64 `json:"max"`
	Last  float64 `json:"last"`
	Stdev float64 `json:"stdev"`
	P5    float64 `json:"p5"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
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

// analysis is the part of measure's output computed from the samples; it is
// embedded in measureResult, so its fields appear in place in the JSON.
type analysis struct {
	Stats  measureStats `json:"stats"`
	BinS   float64      `json:"bin_s"`
	Bins   []binJSON    `json:"bins"`
	Levels []levelJSON  `json:"levels"`
}

const (
	// maxAutoBins bounds the automatic bin width; maxBins bounds --bin.
	maxAutoBins = 120
	maxBins     = 1000
	// maxSamples bounds one measure window: measure keeps every reading
	// until the end for the percentiles (about 64 bytes each).
	maxSamples = 1_000_000
)

// autoBin picks the bin width for a window: the smallest of a fixed ladder
// that spans at least five polling intervals and gives at most maxAutoBins
// bins. When no rung fits (a window over 120 days, or an interval over
// 4.8 h) it uses window/maxAutoBins, still at least five intervals.
func autoBin(window, interval time.Duration) time.Duration {
	ladder := []float64{0.5, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 86400}
	for _, s := range ladder {
		bin := time.Duration(s * float64(time.Second))
		if bin >= 5*interval && math.Ceil(window.Seconds()/s) <= maxAutoBins {
			return bin
		}
	}
	return max(time.Duration(math.Ceil(window.Seconds()/maxAutoBins))*time.Second, 5*interval)
}

// analyze summarises the samples (in time order) of a window of the given
// length in seconds, with bins of binS seconds. Bins and Levels are never nil.
func analyze(ss []sample, window, binS float64) analysis {
	var temps []float64
	for _, s := range ss {
		if s.TempOK {
			temps = append(temps, s.Temp)
		}
	}
	a := analysis{BinS: round(binS, 6), Bins: []binJSON{}, Levels: levels(ss, window)}
	a.Stats = measureStats{
		VBusV:  describe(values(ss, func(s sample) float64 { return s.V }), 6),
		IBusA:  describe(values(ss, func(s sample) float64 { return s.I }), 6),
		PowerW: describe(values(ss, func(s sample) float64 { return s.P }), 6),
		TempC:  describe(temps, 3),
		DPV:    describe(values(ss, func(s sample) float64 { return s.DP }), 4),
		DMV:    describe(values(ss, func(s sample) float64 { return s.DM }), 4),
	}
	if len(ss) == 0 || binS <= 0 {
		return a
	}
	// Samples are in time order, so each bin is a contiguous run. The last
	// bin also takes samples read just after the end of the window.
	nb := max(1, int(math.Ceil(window/binS-1e-9)))
	lo := 0
	for b := range nb {
		hi := lo
		for hi < len(ss) && (b == nb-1 || ss[hi].T < float64(b+1)*binS) {
			hi++
		}
		run := ss[lo:hi]
		bin := binJSON{TS: round(float64(b)*binS, 6), Samples: len(run)}
		if len(run) > 0 {
			bin.VBusV = binRange(run, func(s sample) float64 { return s.V })
			bin.IBusA = binRange(run, func(s sample) float64 { return s.I })
			bin.PowerW = binRange(run, func(s sample) float64 { return s.P })
		}
		a.Bins = append(a.Bins, bin)
		lo = hi
	}
	return a
}

// values extracts one field from a run of samples.
func values(run []sample, f func(sample) float64) []float64 {
	vs := make([]float64, len(run))
	for k, s := range run {
		vs[k] = f(s)
	}
	return vs
}

func binRange(run []sample, f func(sample) float64) *rangeJSON {
	vs := values(run, f)
	return &rangeJSON{Min: round(floats.Min(vs), 6), Mean: round(gstat.Mean(vs, nil), 6), Max: round(floats.Max(vs), 6)}
}

// describe computes the statistics of the values xs (in time order); nil
// when xs is empty. It sorts xs in place: callers pass a slice of their own
// (values() makes one), which saves a copy at a million readings.
func describe(xs []float64, decimals int) *statJSON {
	n := len(xs)
	if n == 0 {
		return nil
	}
	last := xs[n-1]
	mean, sd := gstat.MeanStdDev(xs, nil) // sample (n-1) standard deviation
	// gonum gives NaN for one value, and its compensated formula can round
	// the variance of a long constant run (D+ held at 0.6 V for hours) to a
	// tiny negative number, whose square root is NaN too. encoding/json
	// refuses NaN, which would lose the whole measure object.
	if !(sd > 0) {
		sd = 0
	}
	slices.Sort(xs)
	r := func(v float64) float64 { return round(v, decimals) }
	return &statJSON{Min: r(xs[0]), Mean: r(mean), Max: r(xs[n-1]), Last: r(last), Stdev: r(sd),
		P5: r(quantile(xs, 0.05)), P50: r(quantile(xs, 0.5)), P95: r(quantile(xs, 0.95))}
}

// quantile interpolates linearly between order statistics of sorted input:
// the usual definition (Hyndman & Fan type 7, the default of R and NumPy),
// so p50 is the ordinary median. gonum's stat.Quantile offers only other
// conventions (its LinInterp is type 4, which puts the median of 1..21 at
// 10.5), so this one is kept here.
func quantile(sorted []float64, q float64) float64 {
	h := q * float64(len(sorted)-1)
	lo := int(math.Floor(h))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}
