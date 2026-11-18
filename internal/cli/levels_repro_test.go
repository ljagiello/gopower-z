package cli

import (
	"math"
	"testing"
)

// The cases reviewers found failing, on the real capture's noise.

// An on/off load on a long window: 8 s on / 8 s off for 10,000 s gives every
// phase, each level at one of the two currents.
func TestLevelsReproLongOnOff(t *testing.T) {
	window := 10000.0
	ss := add(noise(t, window, 1, 0), func(t float64) float64 {
		if math.Mod(t, 16) < 8 {
			return 0.2
		}
		return 0
	})
	lv := levels(ss, window)
	if len(lv) != int(window/8) {
		t.Fatalf("%d levels, want %d", len(lv), int(window/8))
	}
	for k, l := range lv {
		if p := l.IBusA.P50; math.Abs(p-0.0655) > 0.01 && math.Abs(p-0.2655) > 0.01 {
			t.Fatalf("level %d (%v..%v s) median %.1f mA is neither current", k, l.StartS, l.EndS, p*1000)
		}
	}
}

// 1 s polling, 15 s on / 15 s off, +30 mA: every phase (15 readings each).
func TestLevelsReproOnOffOneSecond(t *testing.T) {
	ss := add(noise(t, 600, 10, 0), func(t float64) float64 {
		if math.Mod(t, 30) < 15 {
			return 0.03
		}
		return 0
	})
	if lv := levels(ss, 600); len(lv) != 40 {
		t.Errorf("%d levels, want 40 (starts %v)", len(lv), starts(lv)[:min(8, len(lv)-1)])
	}
}

// Bursts shorter than 5 s on a 20,000 s window are not levels, where a
// spurious split beside them used to make one; a 10 s burst is a level.
func TestLevelsReproLongWindowBursts(t *testing.T) {
	window := 20000.0
	base := noise(t, window, 1, 0)
	for _, c := range []struct{ at, dur float64 }{{1690, 4.5}} {
		ss := add(base, func(t float64) float64 {
			if t >= c.at && t < c.at+c.dur {
				return 0.1
			}
			return 0
		})
		if lv := levels(ss, window); len(lv) != 1 {
			t.Errorf("%v s burst at %v s: levels start at %v", c.dur, c.at, starts(lv))
		}
	}
	raw := add(capture(t, "pulses-4s-10m.csv.gz"), func(t float64) float64 {
		if t >= 90 && t < 100 {
			return 0.1
		}
		return 0
	})
	got := starts(levels(raw, window))
	if len(got) != 2 || math.Abs(got[0]-90) > 1 || math.Abs(got[1]-100) > 1 {
		t.Errorf("10 s burst at 90 s: levels start at %v, want [90 100]", got)
	}
}

// A staircase of two 3.5% steps (about 7% in all), up and down: either one
// level or two whose medians differ by at least 5% (both satisfy the rules;
// which one depends on where the merged median lands), never more.
func TestLevelsReproStaircase(t *testing.T) {
	base := noise(t, 600, 1, 0)
	for _, steps := range [][3]float64{{0.3, 0.312, 0.326}, {0.326, 0.312, 0.3}} {
		for _, from := range []float64{0, 195, 455} {
			ss := add(cut(base, from, 60), func(t float64) float64 {
				switch {
				case t >= 40:
					return steps[2]
				case t >= 20:
					return steps[1]
				}
				return steps[0]
			})
			lv := levels(ss, 60)
			if len(lv) > 2 {
				t.Errorf("staircase %v at %v s: %d levels (starts %v)", steps, from, len(lv), starts(lv))
				continue
			}
			for k := 1; k < len(lv); k++ {
				a, b := lv[k-1].IBusA.P50, lv[k].IBusA.P50
				if math.Abs(a-b) < 0.05*math.Min(a, b) {
					t.Errorf("staircase %v at %v s: medians %.1f and %.1f mA differ by under 5%%", steps, from, a*1000, b*1000)
				}
			}
		}
	}
}

// mergeClose judges the pairs either side of a merge again: here merging
// one pair makes the left pair weak (so it merges too), or the right pair
// strong (so it stays).
func TestMergeCloseRejudges(t *testing.T) {
	lv := func(a int, v float64) level {
		xs := make([]float64, 20)
		for i := range xs {
			xs[i] = v
		}
		return level{a, a + 20, xs}
	}
	for _, c := range []struct {
		name    string
		a, b, c float64
		want    int
	}{
		// B-C (2.9%) merges first; A against B+C (median 104.5) is 4.5%: merges.
		{"left pair becomes weak", 0.100, 0.106, 0.103, 1},
		// A-B (2%) merges first; A+B (median 101) against C is 5.15%: stays.
		{"right pair becomes strong", 0.100, 0.102, 0.1062, 2},
	} {
		if got := mergeClose([]level{lv(0, c.a), lv(20, c.b), lv(40, c.c)}); len(got) != c.want {
			t.Errorf("%s: %d levels, want %d", c.name, len(got), c.want)
		}
	}
}

// A very long window (30,000 s, 0.5 s polling): a 4 s burst is not a level,
// a 7.5 s level is, at about the right time.
func TestLevelsReproVeryLongWindow(t *testing.T) {
	window := 30000.0
	base := noise(t, window, 5, 0)
	burst := add(base, func(t float64) float64 {
		if t >= 6420.09 && t < 6424.09 {
			return 0.1
		}
		return 0
	})
	if lv := levels(burst, window); len(lv) != 1 {
		t.Errorf("4 s burst: levels start at %v", starts(lv))
	}
	level := add(base, func(t float64) float64 {
		if t >= 15420.08 && t < 15427.58 {
			return 0.1
		}
		return 0
	})
	got := starts(levels(level, window))
	if len(got) != 2 || math.Abs(got[0]-15420.08) > 2 || math.Abs(got[1]-15427.58) > 2 {
		t.Errorf("7.5 s level: levels start at %v, want [15420.08 15427.58]", got)
	}
}
