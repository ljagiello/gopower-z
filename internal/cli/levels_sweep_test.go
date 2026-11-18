package cli

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
)

// A seeded sweep of levels over loads, amplitudes, polling and windows, on
// the real pulsing-load capture's noise (tiled), with every injected event
// kept away from the tiles' seams. Expectations follow the definition, with
// margins: phases of 8 s and 12 readings or more are levels (pulses inside a
// phase do not count toward its 5 s), phases of 4 s or less are not;
// anything in between is not asserted.
//
// The full sweep is slow; it runs with POWERZ_LEVELS_SWEEP=<cases> (e.g.
// 2000). Without it, a fixed sample of sweepSample cases runs with the
// other tests.
const sweepSample = 40

type sweepCase struct {
	kind         string
	on, off, amp float64 // on/off load (amp: added current, A)
	at, dur      float64 // burst or step start, and burst length
	poll, window float64
	shift        float64
}

func (c sweepCase) String() string {
	switch c.kind {
	case "onoff":
		return fmt.Sprintf("on/off %.1f/%.1f s +%.0f mA, every %v s, %v s window, shift %.2f", c.on, c.off, c.amp*1000, c.poll, c.window, c.shift)
	case "burst":
		return fmt.Sprintf("burst %.2f s +%.0f mA at %.2f s, every %v s, %v s window", c.dur, c.amp*1000, c.at, c.poll, c.window)
	}
	return fmt.Sprintf("step %+.1f mA at %.2f s, every %v s, %v s window", c.amp*1000, c.at, c.poll, c.window)
}

// offSeam reports whether [a, b] stays 3 s clear of the tiled capture's seams.
func offSeam(a, b float64) bool {
	const tile = 599.999
	for k := math.Floor(a/tile) * tile; k <= b+3; k += tile {
		if k > 0 && k >= a-3 && k <= b+3 {
			return false
		}
	}
	return true
}

func randomCase(rng *rand.Rand) sweepCase {
	pick := func(xs ...float64) float64 { return xs[rng.IntN(len(xs))] }
	c := sweepCase{poll: pick(0.1, 0.1, 0.5, 1, 3), window: pick(120, 600, 3600, 20000)}
	switch rng.IntN(3) {
	case 0:
		c.kind = "onoff"
		c.on, c.off = 3+rng.Float64()*37, 3+rng.Float64()*37
		c.amp = pick(0.015, 0.03, 0.06, 0.3)
		c.shift = rng.Float64() * (c.on + c.off)
		c.window = math.Min(c.window, 3600)
	case 1:
		c.kind = "burst"
		c.dur = pick(0.5+rng.Float64()*3.5, 6+rng.Float64()*24)
		c.amp = pick(0.03, 0.1, 0.3)
		c.window = pick(600, 3600, 20000, 100000)
		if c.window == 100000 {
			c.poll = 1
		}
		for c.at = 10 + rng.Float64()*(c.window-20-c.dur); !offSeam(c.at, c.at+c.dur); {
			c.at = 10 + rng.Float64()*(c.window-20-c.dur)
		}
	default:
		c.kind = "step"
		c.amp = pick(-0.03, -0.015, 0.015, 0.03, 0.3)
		for c.at = 10 + rng.Float64()*(c.window-20); !offSeam(c.at, c.at); {
			c.at = 10 + rng.Float64()*(c.window-20)
		}
	}
	return c
}

// check runs one case and returns why it fails, or "" (also "" when the case
// is in the band the definition leaves open).
func check(t *testing.T, c sweepCase, cache map[[2]float64][]sample) string {
	key := [2]float64{c.window, c.poll}
	base, ok := cache[key]
	if !ok {
		// Lifted by 100 mA: the capture's own slow wander (about 3 mA) then stays
		// under the 5% size rule whatever is added or taken away.
		base = add(noise(t, c.window, int(math.Round(c.poll*10)), 0), func(float64) float64 { return 0.1 })
		cache[key] = base
	}
	var want []float64 // expected change times
	var currents []float64
	var ss []sample
	tol := math.Max(3, 4*c.poll) // at slow polling an aliased pulse at a change moves it a few readings
	switch c.kind {
	case "onoff":
		period := c.on + c.off
		isOn := func(t float64) bool { return math.Mod(t+c.shift, period) < c.on }
		ss = add(base, func(t float64) float64 {
			if isOn(t) {
				return c.amp
			}
			return 0
		})
		minPhase, maxPhase := math.Min(c.on, c.off), math.Max(c.on, c.off)
		switch {
		case minPhase >= 8 && minPhase/c.poll >= levelFewReadings:
			// every phase, except edge phases too short to be levels
			for k := -1; float64(k)*period-c.shift < c.window+period; k++ {
				for _, at := range []float64{float64(k)*period - c.shift, float64(k)*period + c.on - c.shift} {
					if at > 0 && at < c.window {
						want = append(want, at)
					}
				}
			}
			slices.Sort(want)
			if len(want) > 0 && (want[0] < 8 || want[0]/c.poll < levelFewReadings) {
				if want[0] < 4 {
					want = want[1:]
				} else {
					return "" // an edge phase at the persistence limit
				}
			}
			if len(want) > 0 && (c.window-want[len(want)-1] < 8 || (c.window-want[len(want)-1])/c.poll < levelFewReadings) {
				if c.window-want[len(want)-1] < 4 {
					want = want[:len(want)-1]
				} else {
					return ""
				}
			}
			currents = []float64{0.1655, 0.1655 + c.amp}
		case maxPhase <= 4:
			want = nil // fast switching: one level
		default:
			return ""
		}
		for _, at := range want {
			if !offSeam(at, at) {
				return ""
			}
		}
	case "burst":
		ss = add(base, func(t float64) float64 {
			if t >= c.at && t < c.at+c.dur {
				return c.amp
			}
			return 0
		})
		switch {
		case c.dur <= levelMinS-math.Max(0.5, 2*c.poll):
		case c.dur >= 8 && c.dur/c.poll >= levelFewReadings:
			want = []float64{c.at, c.at + c.dur}
			currents = []float64{0.1655, 0.1655 + c.amp}
		default:
			return ""
		}
	case "step":
		ss = add(base, func(t float64) float64 {
			if t >= c.at {
				return c.amp
			}
			return 0
		})
		// relative to the lifted baseline: a change near the 5% size rule is
		// in the open band
		if rel := math.Abs(c.amp) / math.Min(0.1655, 0.1655+c.amp); rel < 0.065 || math.Min(c.at, c.window-c.at) < math.Max(8, levelFewReadings*c.poll) {
			return ""
		}
		want = []float64{c.at}
		currents = []float64{0.1655, 0.1655 + c.amp}
	}
	lv := levels(ss, c.window)
	got := starts(lv)
	if len(got) != len(want) {
		return fmt.Sprintf("%d changes, want %d (first %v, want %v)", len(got), len(want), got[:min(4, len(got))], want[:min(4, len(want))])
	}
	for k := range got {
		if math.Abs(got[k]-want[k]) > tol {
			return fmt.Sprintf("change %d at %.2f s, want %.2f ± %v", k, got[k], want[k], tol)
		}
	}
	for k, l := range lv {
		ok := len(currents) == 0
		for _, cur := range currents {
			ok = ok || math.Abs(l.IBusA.P50-cur) <= math.Max(0.006, 0.1*math.Abs(cur))
		}
		if !ok {
			return fmt.Sprintf("level %d (%.1f-%.1f s) median %.1f mA is neither %v", k, l.StartS, l.EndS, l.IBusA.P50*1000, currents)
		}
	}
	return ""
}

func TestLevelsSweep(t *testing.T) {
	n := sweepSample
	if v := os.Getenv("POWERZ_LEVELS_SWEEP"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	rng := rand.New(rand.NewPCG(2026, 11))
	cache := map[[2]float64][]sample{}
	type tally struct{ cases, failed int }
	regimes := map[string]*tally{}
	for range n {
		c := randomCase(rng)
		if os.Getenv("POWERZ_LEVELS_SWEEP") == "" && c.window > 3600 {
			continue // long windows: full sweep only (slow under -race)
		}
		key := fmt.Sprintf("%-5s every %3.1f s  %+4.0f mA", c.kind, c.poll, c.amp*1000)
		if regimes[key] == nil {
			regimes[key] = &tally{}
		}
		regimes[key].cases++
		if why := check(t, c, cache); why != "" {
			regimes[key].failed++
			t.Errorf("%v: %s", c, why)
		}
	}
	if os.Getenv("POWERZ_LEVELS_SWEEP") != "" {
		keys := make([]string, 0, len(regimes))
		for k := range regimes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			t.Logf("%s  %3d cases  %3d failed", k, regimes[k].cases, regimes[k].failed)
		}
	}
}
