package cli

import (
	"math"
	"testing"
)

// The persistence rule counts a level's own readings: at least 5 of them
// (at 10 s polling a clearly separate 4-reading stretch is not a level, a
// 6-reading one is), and failed reads after a level do not lengthen it.
func TestLevelsPersistenceCountsReadings(t *testing.T) {
	series := func(dt float64, cur func(t float64) (float64, bool)) []sample {
		var ss []sample
		for k := 0; float64(k)*dt < 600; k++ {
			tt := float64(k) * dt
			if i, ok := cur(tt); ok {
				ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
			}
		}
		return ss
	}
	for _, c := range []struct {
		name string
		ss   []sample
		want int
	}{
		{"4 readings at 10 s polling", series(10, func(t float64) (float64, bool) {
			if t >= 300 && t < 340 {
				return 0.3, true
			}
			return 0.1, true
		}), 1},
		{"6 readings at 10 s polling", series(10, func(t float64) (float64, bool) {
			if t >= 300 && t < 360 {
				return 0.3, true
			}
			return 0.1, true
		}), 3},
		{"4.5 s level, then 3.5 s of failed reads", series(0.1, func(t float64) (float64, bool) {
			switch {
			case t >= 300 && t < 304.5:
				return 0.3, true
			case t >= 304.5 && t < 308:
				return 0, false
			}
			return 0.1, true
		}), 1},
		{"6 s level, then 3.5 s of failed reads", series(0.1, func(t float64) (float64, bool) {
			switch {
			case t >= 300 && t < 306:
				return 0.3, true
			case t >= 306 && t < 309.5:
				return 0, false
			}
			return 0.1, true
		}), 3},
	} {
		if lv := levels(c.ss, 600); len(lv) != c.want {
			t.Errorf("%s: levels start at %v, want %d levels", c.name, starts(lv), c.want)
		}
	}
}

// When two levels merge, the change to their left is judged again against
// the merged level: 100 mA, then 106 mA for 10 s, then 103 mA. 106 and 103
// merge (under 5%), and 100 against the merged ~103 mA is under 5% too.
func TestLevelsMergeRejudgesNeighbours(t *testing.T) {
	var ss []sample
	for k := range 600 {
		tt, i := float64(k)/10, 0.1
		switch {
		case tt >= 20 && tt < 30:
			i = 0.106
		case tt >= 30:
			i = 0.103
		}
		ss = append(ss, sample{T: tt, V: 5, I: i, P: 5 * i})
	}
	if lv := levels(ss, 60); len(lv) != 1 {
		t.Errorf("levels start at %v, want one level", starts(lv))
	}
}

// rankZ: the Mann-Whitney z score, with ties.
func TestRankZ(t *testing.T) {
	for _, c := range []struct {
		a, b []float64
		want float64
	}{
		{[]float64{1, 2, 3}, []float64{4, 5, 6}, 1.964},  // U = 9: all of b above a
		{[]float64{4, 5, 6}, []float64{1, 2, 3}, -1.964}, // and the other way
		{[]float64{1, 2, 3}, []float64{1, 2, 3}, 0},
		{[]float64{1, 1, 2}, []float64{2, 3, 3}, 1.826}, // ties: U = 8.5, variance 4.8
		{[]float64{5, 5}, []float64{5, 5}, 0},           // all tied
	} {
		if got := rankZ(c.a, c.b); math.Abs(got-c.want) > 0.001 {
			t.Errorf("rankZ(%v, %v) = %.4f, want %v", c.a, c.b, got, c.want)
		}
	}
}
