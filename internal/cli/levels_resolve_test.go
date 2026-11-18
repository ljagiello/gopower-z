package cli

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// resolve must give the same levels whatever extra candidates it is
// offered: that is what lets candidate search over-propose. Each scenario
// runs once with the true change times as candidates and then with random
// extra ones added; the levels must match (changes within 2 s: a pulse at a
// change moves it by its length).
func TestResolveIgnoresExtraCandidates(t *testing.T) {
	base := noise(t, 600, 1, 0)
	type scenario struct {
		name string
		add  func(t float64) float64
	}
	onoff := func(on, off, amp, shift float64) func(float64) float64 {
		return func(t float64) float64 {
			if math.Mod(t+shift, on+off) < on {
				return amp
			}
			return 0
		}
	}
	var scenarios []scenario
	for _, c := range [][3]float64{{12, 9, 0.06}, {9, 13, 0.3}, {9, 9, 0.06}, {10, 9, 0.06}, {3, 3, 0.3}, {20, 20, 0.015}} {
		scenarios = append(scenarios, scenario{fmt.Sprintf("on/off %v/%v +%v mA", c[0], c[1], c[2]*1000), onoff(c[0], c[1], c[2], 0)})
	}
	scenarios = append(scenarios,
		scenario{"burst 3 s", func(t float64) float64 {
			if t >= 300 && t < 303 {
				return 0.2
			}
			return 0
		}},
		scenario{"staircase 3.5% + 3.5%", func(t float64) float64 {
			switch {
			case t >= 400:
				return 0.326
			case t >= 200:
				return 0.312
			}
			return 0.3
		}},
	)
	rng := rand.New(rand.NewPCG(5, 6))
	for _, sc := range scenarios {
		ss := add(base, sc.add)
		// true change indexes: where the added current changes
		exact := []int{0}
		for i := 1; i < len(ss); i++ {
			if sc.add(ss[i].T) != sc.add(ss[i-1].T) {
				exact = append(exact, i)
			}
		}
		exact = append(exact, len(ss))
		want := resolve(ss, exact, 1)
		for trial := range 5 {
			cands := slices.Clone(exact[1 : len(exact)-1])
			for range len(ss) / 30 { // about one extra every 3 s
				cands = append(cands, 1+rng.IntN(len(ss)-1))
			}
			slices.Sort(cands)
			cands = slices.Compact(cands)
			got := resolve(ss, append(append([]int{0}, cands...), len(ss)), 1)
			if !sameLevels(ss, got, want) {
				at := 0
				for at < min(len(got), len(want)) && math.Abs(ss[min(got[at], len(ss)-1)].T-ss[min(want[at], len(ss)-1)].T) <= 2 {
					at++
				}
				t.Errorf("%s, trial %d: with extra candidates %d levels, with the true ones %d; first difference at change %d: %v vs %v",
					sc.name, trial, len(got)-1, len(want)-1, at, times(ss, got[at:min(at+3, len(got))]), times(ss, want[at:min(at+3, len(want))]))
			}
		}
	}
}

func times(ss []sample, bounds []int) []float64 {
	var ts []float64
	for _, b := range bounds {
		if b < len(ss) {
			ts = append(ts, ss[b].T)
		}
	}
	return ts
}

func sameLevels(ss []sample, a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for k := 1; k+1 < len(a); k++ {
		if math.Abs(ss[a[k]].T-ss[b[k]].T) > 2 { // a pulse at a change moves it by its length
			return false
		}
	}
	return true
}
