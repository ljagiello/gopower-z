package cli

import (
	"compress/gzip"
	"encoding/csv"
	"math"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// capture reads a `powerz log --format csv` recording from testdata.
func capture(t *testing.T, name string) []sample {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(z).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var ss []sample
	var t0 time.Time
	for _, row := range rows[1:] { // time,vbus_v,ibus_a,power_w,...
		tm, err := time.Parse(time.RFC3339Nano, row[0])
		if err != nil {
			t.Fatal(err)
		}
		if t0.IsZero() {
			t0 = tm
		}
		num := func(s string) float64 {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		ss = append(ss, sample{T: tm.Sub(t0).Seconds(), V: num(row[1]), I: num(row[2]), P: num(row[3])})
	}
	return ss
}

// cut returns the samples in [from, from+w), with times from the cut's start.
func cut(ss []sample, from, w float64) []sample {
	var out []sample
	for _, s := range ss {
		if s.T >= from && s.T < from+w {
			s.T -= from
			out = append(out, s)
		}
	}
	return out
}

func starts(lv []levelJSON) []float64 {
	var ts []float64
	for _, l := range lv[1:] {
		ts = append(ts, l.StartS)
	}
	return ts
}

// A board that pulses to ~120 mA for ~0.7 s every 4 s on a ~65 mA baseline,
// and nothing else changes: every window is one level.
func TestLevelsPulsingLoad(t *testing.T) {
	ss := capture(t, "pulses-4s-10m.csv.gz")
	for _, w := range []float64{60, 300} {
		for from := 0.0; from+w <= 599; from += w / 12 {
			lv := levels(cut(ss, from, w), w)
			if len(lv) != 1 || lv[0].StartS != 0 || lv[0].EndS != w {
				t.Errorf("%v s window at %v s: levels start at %v", w, from, starts(lv))
			}
		}
	}
}

// An ESP32 parked twice in its ROM bootloader (reset at 60.9 and 181.0 s):
// the pulse stops and the current drops from ~65 to ~61 mA until the chip
// reboots itself after ~7 s. Reboot surges of ~3 s (at 68, 120, 189 and
// 210 s) are not levels.
func TestLevelsBootloader(t *testing.T) {
	ss := capture(t, "bootloader-steps-5m.csv.gz")
	lv := levels(ss, 300)
	want := []float64{61, 68, 181, 189}
	if len(lv) != 5 {
		t.Fatalf("want 5 levels, got starts %v", starts(lv))
	}
	for k, at := range want {
		if math.Abs(lv[k+1].StartS-at) > 1 {
			t.Errorf("level %d starts at %v s, want %v ± 1", k+1, lv[k+1].StartS, at)
		}
	}
	for k, l := range lv {
		p50, want := l.IBusA.P50*1000, 65.5
		if k%2 == 1 {
			want = 61
		}
		if math.Abs(p50-want) > 1.5 {
			t.Errorf("level %d: median %.1f mA, want about %v", k, p50, want)
		}
	}
	n := 0
	for _, l := range lv {
		n += l.Samples
	}
	if n != len(ss) || lv[0].StartS != 0 || lv[4].EndS != 300 {
		t.Errorf("levels hold %d of %d samples, %v..%v s", n, len(ss), lv[0].StartS, lv[4].EndS)
	}
}

// A board reboot raised the baseline from ~48 to ~65 mA at ~21 s; reboots
// of two other boards (at ~10 and ~31 s) left it alone.
func TestLevelsReboot(t *testing.T) {
	lv := levels(capture(t, "reboots-40s.csv.gz"), 40)
	if len(lv) != 2 || lv[1].StartS < 20 || lv[1].StartS > 22 {
		t.Errorf("want one change at ~21 s, got %v", starts(lv))
	}
}

// Known changes added to real readings of the pulsing load, in 60 s windows
// at several offsets.
func TestLevelsInjected(t *testing.T) {
	ss := capture(t, "pulses-4s-10m.csv.gz")
	step := func(at, d float64) func(float64) float64 {
		return func(t float64) float64 {
			if t >= at {
				return d
			}
			return 0
		}
	}
	burst := func(at, dur, d float64) func(float64) float64 {
		return func(t float64) float64 {
			if t >= at && t < at+dur {
				return d
			}
			return 0
		}
	}
	for _, c := range []struct {
		name string
		add  func(float64) float64
		want []float64 // level start times
	}{
		{"charger on at 42 s (+300 mA)", step(42, 0.3), []float64{42}},
		{"small drop at 30 s (-40 mA)", step(30, -0.04), []float64{30}},
		{"small rise at 30 s (+20 mA)", step(30, 0.02), []float64{30}},
		{"reversal at 30 s", step(30, -0.13), []float64{30}},
		{"on at 10 s, off at 40 s", func(t float64) float64 { return step(10, 0.3)(t) - step(40, 0.3)(t) }, []float64{10, 40}},
		{"level of 8 s at the end", step(52, 0.3), []float64{52}},
		{"level of 3 s at the end", step(57, 0.3), nil},
		{"three 1 s bursts", func(t float64) float64 { return burst(12, 1, 0.4)(t) + burst(30, 1, 0.4)(t) + burst(47, 1, 0.4)(t) }, nil},
		{"2 s burst at the start", burst(0, 2, 0.4), nil},
		{"2 s burst at the end", burst(58, 2, 0.4), nil},
	} {
		for from := 0.0; from <= 520; from += 65 {
			w := cut(ss, from, 60)
			for i := range w {
				w[i].I += c.add(w[i].T)
			}
			got := starts(levels(w, 60))
			ok := len(got) == len(c.want)
			for k := 0; ok && k < len(got); k++ {
				ok = math.Abs(got[k]-c.want[k]) <= 2
			}
			if !ok {
				t.Errorf("%s (window at %v s): levels start at %v, want %v", c.name, from, got, c.want)
			}
		}
	}
}

// Each rule of the method on a synthetic series sampled every 100 ms.
func TestLevelsRules(t *testing.T) {
	series := func(window, dt float64, cur func(t float64) float64) []sample {
		var ss []sample
		for k := 0; float64(k)*dt < window; k++ {
			tt := float64(k) * dt
			ss = append(ss, sample{T: tt, V: 5, I: cur(tt), P: 5 * cur(tt)})
		}
		return ss
	}
	for _, c := range []struct {
		name   string
		window float64
		dt     float64
		cur    func(t float64) float64
		want   []float64 // level start times
	}{
		// Pulses that start partway, faster than the 1 s averages (0.3 s in
		// every second): the averages step from 100 to 130 mA, but the
		// typical (median) current stays 100 mA, so it is one level; the
		// pulses show in its mean and p95.
		{"fast pulses starting partway", 60, 0.1, func(t float64) float64 {
			if t >= 30 && math.Mod(t+0.05, 1) < 0.3 {
				return 0.2
			}
			return 0.1
		}, nil},
		// 10 -> 11 mA is 10%, but under the 2 mA floor.
		{"1 mA step on a 10 mA load", 60, 0.1, func(t float64) float64 {
			if t >= 30 {
				return 0.011
			}
			return 0.010
		}, nil},
		{"2.5 mA step on a 10 mA load", 60, 0.1, func(t float64) float64 {
			if t >= 30 {
				return 0.0125
			}
			return 0.010
		}, []float64{30}},
		// On a 50 min window: a 4 s burst is not a level, a 12 s one is.
		{"4 s burst on a 50 min window", 3000, 1, func(t float64) float64 {
			if t >= 1501 && t < 1505 {
				return 0.3
			}
			return 0.1
		}, nil},
		{"12 s level on a 50 min window", 3000, 1, func(t float64) float64 {
			if t >= 1500 && t < 1512 {
				return 0.3
			}
			return 0.1
		}, []float64{1500, 1512}},
	} {
		got := starts(levels(series(c.window, c.dt, c.cur), c.window))
		tol := math.Max(1, 3*c.window/levelPoints) // one ED-PELT point
		ok := len(got) == len(c.want)
		for k := 0; ok && k < len(got); k++ {
			ok = math.Abs(got[k]-c.want[k]) <= tol
		}
		if !ok {
			t.Errorf("%s: levels start at %v, want %v", c.name, got, c.want)
		}
	}
}

// A level shorter than the persistence time joins the neighbour whose
// median is closer, so the change stays where it happened.
func TestLevelsShortLevelJoinsCloserNeighbour(t *testing.T) {
	for _, c := range []struct {
		a, b, c float64 // currents before 20 s, 20..23 s, after 23 s
		want    []int
	}{
		{0.1, 0.26, 0.3, []int{0, 200, 600}}, // closer to the right: change at 20 s
		{0.3, 0.26, 0.1, []int{0, 230, 600}}, // closer to the left: change at 23 s
	} {
		var ss []sample
		for k := range 600 {
			tt, i := float64(k)/10, c.a
			if tt >= 20 {
				i = c.b
			}
			if tt >= 23 {
				i = c.c
			}
			ss = append(ss, sample{T: tt, I: i})
		}
		if got := resolve(ss, []int{0, 200, 230, 600}, 1); !slices.Equal(got, c.want) {
			t.Errorf("%v / %v / %v A: bounds %v, want %v", c.a, c.b, c.c, got, c.want)
		}
	}
}

func TestLevelsEdgeCases(t *testing.T) {
	if lv := levels(nil, 10); lv == nil || len(lv) != 0 {
		t.Errorf("no samples: %v", lv)
	}
	one := levels([]sample{{T: 0, V: 5, I: 0.1, P: 0.5}}, 0)
	if len(one) != 1 || one[0].Samples != 1 || one[0].EndS != 0 || one[0].IBusA.P50 != 0.1 {
		t.Errorf("one sample: %+v", one)
	}
	// Failed reads leave a gap: no level is invented in it, and the change
	// after it is still found.
	var ss []sample
	for k := range 600 {
		tt := float64(k) / 10
		if tt >= 20 && tt < 25 {
			continue
		}
		i := 0.1
		if tt >= 40 {
			i = 0.3
		}
		ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
	}
	lv := levels(ss, 60)
	if len(lv) != 2 || math.Abs(lv[1].StartS-40) > 1 || lv[0].Samples+lv[1].Samples != len(ss) {
		t.Errorf("gap: %v", starts(lv))
	}
}

// The largest window measure allows stays fast: ED-PELT sees at most
// levelPoints averages, in short chunks.
func TestLevelsAtTheReadingLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000,000 readings")
	}
	ss := make([]sample, maxSamples)
	for k := range ss {
		tt := float64(k) / 10
		i := 0.065 + 0.001*math.Sin(float64(k))
		if k%40 < 7 {
			i = 0.12
		}
		if tt >= 50_000 {
			i += 0.1
		}
		ss[k] = sample{T: tt, V: 5, I: i, P: 5 * i}
	}
	start := time.Now()
	lv := levels(ss, 100_000)
	if len(lv) != 2 || math.Abs(lv[1].StartS-50_000) > 0.2 {
		t.Errorf("levels start at %v", starts(lv))
	}
	if d := time.Since(start); d > time.Minute { // a quadratic blow-up would take far longer
		t.Errorf("took %v", d)
	}
}
