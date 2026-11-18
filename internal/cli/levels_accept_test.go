package cli

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// Acceptance tests for loads beyond the ones levels were tuned on: on/off
// loads, long windows and slow polling, all on the real noise of the
// pulsing-load capture (tiled when the window is longer than 10 min).

// noise returns `window` seconds of the real pulsing-load capture, repeated
// end to end, keeping every k-th reading from offset off (k = 1: 100 ms).
func noise(t *testing.T, window float64, k, off int) []sample {
	t.Helper()
	base := capture(t, "pulses-4s-10m.csv.gz")
	span := base[len(base)-1].T + 0.1
	var out []sample
	for tile := 0; float64(tile)*span < window; tile++ {
		for _, s := range base {
			s.T += float64(tile) * span
			if s.T >= window {
				break
			}
			out = append(out, s)
		}
	}
	var sub []sample
	for i := off; i < len(out); i += k {
		sub = append(sub, out[i])
	}
	return sub
}

// add returns a copy of ss with f(t) amperes added to the current (and the
// matching power).
func add(ss []sample, f func(t float64) float64) []sample {
	out := make([]sample, len(ss))
	for i, s := range ss {
		d := f(s.T)
		s.I += d
		s.P += d * s.V
		out[i] = s
	}
	return out
}

// A load that switches on and off with a fixed pattern. With each phase
// lasting 8 s or more, every phase is a level (an edge phase too, when it
// lasts 8 s), each level's median is one of the two currents, never between
// them, and each change is found where it happened. (The real noise pulses
// about a third of the time, and pulses inside a phase do not count toward
// its 5 s, so 5-8 s phases can go either way.) With every phase 4 s or
// shorter, the load is one level.
func TestLevelsOnOffLoads(t *testing.T) {
	for _, window := range []float64{120, 600, 3600} {
		base := noise(t, window, 1, 0)
		for _, c := range []struct{ on, off float64 }{{8, 12}, {9, 11}, {10, 10}, {20, 20}, {21, 39}, {30, 30}, {12, 8}, {2, 4}, {4, 3}} {
			for _, amp := range []float64{0.06, 0.3} {
				for _, shift := range []float64{0, 3.7} {
					if window > 600 && (amp != 0.06 || shift != 3.7) {
						continue // long windows: one amplitude and offset (time under -race)
					}
					period := c.on + c.off
					isOn := func(t float64) bool { return math.Mod(t+shift, period) < c.on }
					ss := add(base, func(t float64) float64 {
						if isOn(t) {
							return amp
						}
						return 0
					})
					// The expected changes: every switch, except where an edge phase
					// lasts less than 5 s inside the window.
					var want []float64
					for k := 0; ; k++ {
						for _, at := range []float64{float64(k)*period - shift, float64(k)*period + c.on - shift} {
							if at > 0 && at < window {
								want = append(want, at)
							}
						}
						if float64(k)*period-shift > window {
							break
						}
					}
					if math.Max(c.on, c.off) <= 4 {
						want = nil // fast switching: one level
					}
					if len(want) > 0 && want[0] < 8 {
						if want[0] >= 4 {
							continue // an edge phase in the open band
						}
						want = want[1:]
					}
					if len(want) > 0 && window-want[len(want)-1] < 8 {
						if window-want[len(want)-1] >= 4 {
							continue
						}
						want = want[:len(want)-1]
					}
					lv := levels(ss, window)
					name := fmt.Sprintf("%v s window, %v s on / %v s off, +%v mA, shifted %v s", window, c.on, c.off, amp*1000, shift)
					got := starts(lv)
					if len(got) != len(want) {
						t.Errorf("%s: %d changes, want %d (first: %v)", name, len(got), len(want), got[:min(len(got), 6)])
						continue
					}
					// About 1 s, plus one pulse of the real load (0.7 s): a pulse right
					// after a switch looks like the +60 mA level.
					for k := range got {
						if math.Abs(got[k]-want[k]) > 2 {
							t.Errorf("%s: change %d at %v s, want %v ± 2", name, k, got[k], want[k])
							break
						}
					}
					for k, l := range lv {
						if len(want) == 0 {
							break // one level of a fast-switching load: its median is the mix
						}
						p := l.IBusA.P50
						if math.Abs(p-0.0655) > 0.01 && math.Abs(p-0.0655-amp) > 0.01 {
							t.Errorf("%s: level %d (%v..%v s) median %.1f mA is neither current", name, k, l.StartS, l.EndS, p*1000)
							break
						}
					}
				}
			}
		}
	}
}

// Bursts shorter than the persistence time are not levels on long windows
// either, wherever they fall relative to the averaging grid; longer ones
// are.
func TestLevelsLongWindowBursts(t *testing.T) {
	for _, window := range []float64{1200, 6000} {
		base := noise(t, window, 1, 0)
		grid := math.Max(1, window/levelPoints)
		for _, dur := range []float64{3, 4.9, 8} {
			for k := range 2 {
				at := window/2 + 7.37 + float64(k)*grid/2 // clear of the tiled capture's seams
				ss := add(base, func(t float64) float64 {
					if t >= at && t < at+dur {
						return 0.2
					}
					return 0
				})
				minS := levelMinS
				if math.Abs(dur-minS) < 1 {
					continue // right at the persistence limit
				}
				got := starts(levels(ss, window))
				want := []float64{}
				if dur > minS {
					want = []float64{at, at + dur}
				}
				ok := len(got) == len(want)
				for i := 0; ok && i < len(got); i++ {
					ok = math.Abs(got[i]-want[i]) <= 1
				}
				if !ok {
					t.Errorf("%v s window, %v s burst at %.2f s: levels start at %v, want %v", window, dur, at, got, want)
				}
			}
		}
	}
}

// Slow polling: the pulsing load read every 3 to 10 s is still one level,
// whichever readings the polling happens to catch. (At 2 s, half the 4 s
// pulse period, the polling locks onto the pulse for minutes at a time and
// the readings really do change: documented as aliasing.)
func TestLevelsSlowPolling(t *testing.T) {
	for _, k := range []int{30, 50, 100} {
		for off := 0; off < k; off += max(1, k/10) {
			base := noise(t, 600, k, off)
			for _, window := range []float64{60, 300} {
				for from := 0.0; from+window <= 600; from += window / 2 {
					if lv := levels(cut(base, from, window), window); len(lv) != 1 {
						t.Errorf("every %v s (offset %d), %v s window at %v s: levels start at %v", float64(k)/10, off, window, from, starts(lv))
					}
				}
			}
		}
	}
}

// Long windows: levels are found with their times accurate to a reading or
// two (1 s polling), short ones included.
func TestLevelsLongWindowAccuracy(t *testing.T) {
	window := 20000.0
	base := noise(t, window, 10, 0) // 1 s polling
	ss := add(base, func(t float64) float64 {
		switch {
		case t >= 5003.3 && t < 5013.3: // 10 s
			return 0.1
		case t >= 10007.3 && t < 10030.3: // 23 s
			return 0.1
		case t >= 15000 && t < 15200:
			return 0.1
		}
		return 0
	})
	got := starts(levels(ss, window))
	want := []float64{5003.3, 5013.3, 10007.3, 10030.3, 15000, 15200}
	ok := len(got) == len(want)
	for i := 0; ok && i < len(got); i++ {
		ok = math.Abs(got[i]-want[i]) <= 2 // two readings at 1 s polling
	}
	if !ok {
		t.Errorf("levels start at %v, want %v", got, want)
	}
}

// What reaches the JSON: analyze fills levels, field by field.
func TestAnalyzeLevels(t *testing.T) {
	var ss []sample
	for k := range 400 {
		if k >= 20 && k < 30 {
			continue // failed reads
		}
		v, cur := 5.076, 0.134512
		if k >= 250 {
			v, cur = 4.95, 0.441237 // the voltage sags under load
		}
		ss = append(ss, sample{T: float64(k) / 10, V: v, I: cur, P: v * cur})
	}
	lv := analyze(ss, 40, 1).Levels
	if len(lv) != 2 {
		t.Fatalf("levels %+v", lv)
	}
	for k, w := range []struct {
		start, end float64
		n          int
		v, i       float64
	}{{0, 25, 240, 5.076, 0.134512}, {25, 40, 150, 4.95, 0.441237}} {
		l, p := lv[k], round(w.v*w.i, 6)
		if l.StartS != w.start || l.EndS != w.end || l.Samples != w.n || l.IBusA.P50 != w.i || l.IBusA.Max != w.i ||
			*l.VBusV != (rangeJSON{w.v, w.v, w.v}) || *l.PowerW != (rangeJSON{p, p, p}) {
			t.Errorf("level %d: %+v vbus %+v power %+v ibus %+v", k, l, *l.VBusV, *l.PowerW, *l.IBusA)
		}
	}
}

// A measure run through the fake meter has one level spanning the window,
// with the reading's values.
func TestCLIMeasureLevel(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "measure", "--duration", "300ms", "--interval", "20ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	lvs := m["levels"].([]any)
	if len(lvs) != 1 {
		t.Fatalf("levels %v", lvs)
	}
	l, last := lvs[0].(map[string]any), m["last"].(map[string]any)
	if l["start_s"] != 0.0 || l["end_s"] != 0.3 || l["samples"] != m["samples"] ||
		l["ibus_a"].(map[string]any)["p50"] != last["ibus_a"] ||
		l["vbus_v"].(map[string]any)["mean"] != last["vbus_v"] ||
		l["power_w"].(map[string]any)["mean"] != last["power_w"] {
		t.Errorf("level %v, last %v", l, last)
	}
}

// The 5% side of the size rule, for both directions of flow.
func TestLevelsSizeRuleRelative(t *testing.T) {
	for _, c := range []struct {
		from, to float64
		n        int
	}{{1, 1.03, 1}, {1, 1.07, 2}, {-1, -1.03, 1}, {-1, -1.07, 2}} {
		var ss []sample
		for k := range 600 {
			tt, i := float64(k)/10, c.from
			if tt >= 30 {
				i = c.to
			}
			ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
		}
		if got := len(levels(ss, 60)); got != c.n {
			t.Errorf("%v -> %v A: %d levels, want %d", c.from, c.to, got, c.n)
		}
	}
}

// After a short level joins its neighbour, the size rule sees the merged
// level's median, not the short level's.
func TestLevelsMergedMedian(t *testing.T) {
	var ss []sample
	for k := range 600 {
		tt, i := float64(k)/10, 0.1
		switch {
		case tt < 2:
			i = 0.3
		case tt >= 30:
			i = 0.103
		}
		ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
	}
	if lv := levels(ss, 60); len(lv) != 1 {
		t.Errorf("levels start at %v", starts(lv))
	}
}

// A ramp is not steady anywhere, so it is not a level: it shows as a change
// (or a few, each higher than the one before) inside the ramp, between the
// level before it and the level after it.
func TestLevelsRamp(t *testing.T) {
	base := noise(t, 60, 1, 0)
	ss := add(base, func(t float64) float64 { return 0.3 * math.Min(1, math.Max(0, (t-20)/20)) })
	lv := levels(ss, 60)
	if len(lv) < 2 {
		t.Fatalf("a 20 s ramp gave %d levels", len(lv))
	}
	for k := 1; k < len(lv); k++ {
		if lv[k].IBusA.P50 <= lv[k-1].IBusA.P50 || lv[k].StartS < 20 || lv[k].StartS > 40 {
			t.Errorf("level %d starts at %v with median %v after %v", k, lv[k].StartS, lv[k].IBusA.P50, lv[k-1].IBusA.P50)
		}
	}
	if lv[0].IBusA.P50 > 0.075 || lv[len(lv)-1].IBusA.P50 < 0.35 {
		t.Errorf("ramp levels %v .. %v", lv[0], lv[len(lv)-1])
	}
}
