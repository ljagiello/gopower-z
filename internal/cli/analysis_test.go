package cli

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestDescribe(t *testing.T) {
	// 1..21 shuffled: percentiles must come from sorted values, with
	// interpolation, and stdev is the sample (n-1) standard deviation.
	xs := []float64{11, 3, 19, 7, 1, 15, 21, 5, 13, 9, 17, 2, 20, 4, 12, 6, 18, 8, 14, 10, 16}
	s := describe(xs, 6)
	if s.Min != 1 || s.Max != 21 || s.Mean != 11 || s.Last != 16 || s.P5 != 2 || s.P50 != 11 || s.P95 != 20 ||
		s.Stdev != round(math.Sqrt(38.5), 6) {
		t.Errorf("1..21: %+v", s)
	}
	s = describe([]float64{4, 1, 3, 2}, 6)
	if s.Min != 1 || s.Max != 4 || s.Mean != 2.5 || s.Last != 2 || s.P5 != 1.15 || s.P50 != 2.5 || s.P95 != 3.85 ||
		s.Stdev != round(math.Sqrt(5.0/3), 6) {
		t.Errorf("[4 1 3 2]: %+v", s)
	}
	// Skewed, so mean (4), median (2.5) and midrange (5.5) all differ.
	s = describe([]float64{1, 2, 3, 10}, 6)
	if s.Mean != 4 || s.P50 != 2.5 || s.Min != 1 || s.Max != 10 || s.Last != 10 || s.P5 != 1.15 || s.P95 != 8.95 ||
		s.Stdev != round(math.Sqrt(50.0/3), 6) {
		t.Errorf("[1 2 3 10]: %+v", s)
	}
	if s := describe([]float64{0.25}, 6); s.Stdev != 0 || s.P5 != 0.25 || s.P95 != 0.25 {
		t.Errorf("one value: %+v", s)
	}
	if describe(nil, 6) != nil {
		t.Error("no values must give nil")
	}
	// A long constant run (D+ held at 0.6 V for a full window): the
	// variance must not round to a negative number and come out as NaN,
	// which encoding/json would refuse, losing the whole measure object.
	long := make([]float64, maxSamples)
	for k := range long {
		long[k] = 0.6
	}
	s = describe(long, 4)
	if _, err := json.Marshal(s); err != nil || s.Stdev != 0 || s.Mean != 0.6 {
		t.Errorf("1 000 000 x 0.6: %+v, %v", s, err)
	}
}

// The JSON keys are the contract: each must carry its own value. Readings
// 10, 1, 3, 2 make every statistic, and the bin's min, mean and max, differ.
func TestStatAndBinKeys(t *testing.T) {
	var ss []sample
	for k, v := range []float64{10, 1, 3, 2} {
		ss = append(ss, sample{T: float64(k) / 10, V: v, I: v, P: v, DP: v, DM: v, Temp: v, TempOK: true})
	}
	b, err := json.Marshal(analyze(ss, 0.4, 1))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Stats map[string]map[string]float64 `json:"stats"`
		Bins  []map[string]any              `json:"bins"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"min": 1, "mean": 4, "max": 10, "last": 2, "stdev": round(math.Sqrt(50.0/3), 3),
		"p5": 1.15, "p50": 2.5, "p95": 8.95}
	for field, st := range got.Stats {
		for key, v := range want {
			if round(st[key], 3) != v {
				t.Errorf("stats.%s.%s = %v, want %v", field, key, st[key], v)
			}
		}
		if len(st) != len(want) {
			t.Errorf("stats.%s has keys %v", field, st)
		}
	}
	if len(got.Stats) != 6 || len(got.Bins) != 1 {
		t.Fatalf("stats %v, bins %v", got.Stats, got.Bins)
	}
	for _, field := range []string{"vbus_v", "ibus_a", "power_w"} {
		r := got.Bins[0][field].(map[string]any)
		if r["min"] != 1.0 || r["mean"] != 4.0 || r["max"] != 10.0 {
			t.Errorf("bins[0].%s = %v", field, r)
		}
	}
}

// Each bin summarises its own samples, field by field: values that differ
// per field and are not linear in time, so a field read from the wrong
// sample value or a mean replaced by the midrange shows up.
func TestBinContents(t *testing.T) {
	var ss []sample
	for k := range 30 { // 3 s at 100 ms
		x := float64(k)
		ss = append(ss, sample{T: x / 10, V: 5 + x*x/1000, I: 0.1 + math.Sqrt(x)/100, P: 1 + math.Mod(x, 7)/10, DP: 3, DM: 0})
	}
	a := analyze(ss, 3, 1)
	if len(a.Bins) != 3 {
		t.Fatalf("bins %+v", a.Bins)
	}
	for b, bin := range a.Bins {
		run := ss[b*10 : b*10+10]
		check := func(name string, got *rangeJSON, f func(sample) float64) {
			lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
			for _, s := range run {
				lo, hi, sum = math.Min(lo, f(s)), math.Max(hi, f(s)), sum+f(s)
			}
			want := rangeJSON{Min: round(lo, 6), Mean: round(sum/10, 6), Max: round(hi, 6)}
			if got == nil || *got != want {
				t.Errorf("bin %d %s = %+v, want %+v", b, name, got, want)
			}
		}
		check("vbus_v", bin.VBusV, func(s sample) float64 { return s.V })
		check("ibus_a", bin.IBusA, func(s sample) float64 { return s.I })
		check("power_w", bin.PowerW, func(s sample) float64 { return s.P })
		if bin.Samples != 10 || bin.TS != float64(b) {
			t.Errorf("bin %d: %d samples from %v s", b, bin.Samples, bin.TS)
		}
	}
}

func TestBins(t *testing.T) {
	var ss []sample
	for k := 0; float64(k)*0.1 < 10.25; k++ {
		ss = append(ss, sample{T: float64(k) * 0.1, I: float64(k) / 1000})
	}
	a := analyze(ss, 10.25, 1)
	if len(a.Bins) != 11 || a.BinS != 1 {
		t.Fatalf("want 11 bins of 1 s (the last one short), got %d of %v", len(a.Bins), a.BinS)
	}
	n := 0
	for k, b := range a.Bins {
		n += b.Samples
		if b.TS != float64(k) {
			t.Errorf("bin %d starts at %v", k, b.TS)
		}
	}
	if n != len(ss) || a.Bins[10].Samples != 3 {
		t.Errorf("bins hold %d samples (last bin %d), want %d", n, a.Bins[10].Samples, len(ss))
	}
	// Bins that are not 1 s wide start at k * bin_s, without float noise.
	tenths := analyze(ss[:10], 1, 0.1)
	for k, b := range tenths.Bins {
		if want := []float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}[k]; b.TS != want || b.Samples != 1 {
			t.Errorf("0.1 s bin %d: t_s %v with %d samples, want %v with 1", k, b.TS, b.Samples, want)
		}
	}
	// 2.1 / 0.3 is 7.000000000000001 in floating point; it is still 7 bins.
	if n := len(analyze(ss[:21], 2.1, 0.3).Bins); n != 7 {
		t.Errorf("2.1 s in 0.3 s bins: %d bins, want 7", n)
	}
	// A run of failed readings leaves an empty bin with null values.
	var gap []sample
	for _, s := range ss {
		if s.T < 3 || s.T >= 4 {
			gap = append(gap, s)
		}
	}
	if b := analyze(gap, 10.25, 1).Bins[3]; b.Samples != 0 || b.VBusV != nil || b.IBusA != nil || b.PowerW != nil {
		t.Errorf("empty bin %+v", b)
	}
}

// measure caps the window at --duration, so the last read can land at or
// after the end of the window: it belongs to the last bin.
func TestSamplesAtWindowEnd(t *testing.T) {
	var ss []sample
	for k := range 300 {
		ss = append(ss, sample{T: float64(k) * 0.1, I: 0.1})
	}
	ss = append(ss, sample{T: 30, I: 0.5}, sample{T: 30.003, I: 0.5})
	a := analyze(ss, 30, 1)
	n := 0
	for _, b := range a.Bins {
		n += b.Samples
	}
	last := a.Bins[len(a.Bins)-1]
	if len(a.Bins) != 30 || n != len(ss) || last.Samples != 12 || last.IBusA.Max != 0.5 || a.Stats.IBusA.Max != 0.5 {
		t.Errorf("%d bins holding %d of %d samples; last bin %+v", len(a.Bins), n, len(ss), last)
	}
}

// Windows cut short (an agent harness sends SIGTERM) must still marshal:
// encoding/json refuses NaN and Inf, which would lose the whole result.
func TestAnalyzeDegenerateWindows(t *testing.T) {
	one := []sample{{T: 0, V: 5, I: 0.1, P: 0.5, DP: 3}}
	two := []sample{one[0], {T: 0.1, V: 5, I: 0.1, P: 0.5, DP: 3}}
	for name, c := range map[string]struct {
		ss           []sample
		window, binS float64
	}{
		"none": {nil, 0, 0.5}, "one": {one, 0.05, 0.5}, "one at zero length": {one, 0, 0.5}, "two": {two, 0.2, 0.5},
		"single bin": {two, 0.2, 60},
	} {
		a := analyze(c.ss, c.window, c.binS)
		if _, err := json.Marshal(measureResult{analysis: a}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.Bins == nil {
			t.Errorf("%s: bins must be [] not null", name)
		}
		if len(c.ss) > 0 && (a.Stats.IBusA == nil || a.Stats.IBusA.Stdev != 0 || len(a.Bins) != 1) {
			t.Errorf("%s: stats %+v, %d bins", name, a.Stats.IBusA, len(a.Bins))
		}
	}
}

func TestAutoBin(t *testing.T) {
	for _, c := range []struct {
		window, interval, want time.Duration
	}{
		{5 * time.Second, 100 * time.Millisecond, 500 * time.Millisecond},
		{time.Minute, 100 * time.Millisecond, 500 * time.Millisecond},
		{10 * time.Minute, 100 * time.Millisecond, 5 * time.Second},
		{time.Hour, 100 * time.Millisecond, 30 * time.Second},
		{time.Minute, time.Second, 5 * time.Second},
		{time.Minute, 120 * time.Millisecond, time.Second}, // 0.5 s would be under 5 intervals
		{48 * time.Hour, 100 * time.Millisecond, 1800 * time.Second},
		{200 * 24 * time.Hour, time.Second, 144000 * time.Second}, // past the ladder: window / 120
		{100 * time.Hour, 6 * time.Hour, 30 * time.Hour},          // past the ladder, still 5 intervals
	} {
		if got := autoBin(c.window, c.interval); got != c.want {
			t.Errorf("autoBin(%v, %v) = %v, want %v", c.window, c.interval, got, c.want)
		}
	}
}
