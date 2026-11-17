package cli

import (
	"fmt"
	"math"
	"strings"
)

// grouped formats v with 6 decimals and a thin gap every three digits after
// the point, like the meter's own display ("5.073 412").
func grouped(v float64) string {
	if math.Abs(v) < 5e-7 {
		v = 0 // avoid "-0.000 000"
	}
	s := fmt.Sprintf("%.6f", v)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return s
	}
	return s[:i+4] + " " + s[i+4:]
}

// nz returns 0 for values that would print as a signed zero at the given
// rounding threshold.
func nz(v, eps float64) float64 {
	if math.Abs(v) < eps {
		return 0
	}
	return v
}

// f6 formats with 6 decimals in a fixed 10-character column, without "-0".
func f6(v float64) string {
	if math.Abs(v) < 5e-7 {
		v = 0
	}
	return fmt.Sprintf("%10.6f", v)
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline draws the last width values; NaN marks a gap (drawn as a space).
func sparkline(vals []float64, width int) string {
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	if len(vals) == 0 {
		return ""
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if !math.IsNaN(v) {
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
	}
	var b strings.Builder
	for _, v := range vals {
		if math.IsNaN(v) {
			b.WriteByte(' ')
			continue
		}
		idx := 0
		if hi > lo {
			idx = int((v - lo) / (hi - lo) * float64(len(sparkRunes)-1))
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

func hexdump(b []byte) string {
	var s strings.Builder
	for i, c := range b {
		if i > 0 {
			s.WriteByte(' ')
		}
		fmt.Fprintf(&s, "%02X", c)
	}
	return s.String()
}

// ANSI styling, disabled when not writing to a terminal.
type style struct{ on bool }

func (s style) wrap(code, txt string) string {
	if !s.on {
		return txt
	}
	return "\x1b[" + code + "m" + txt + "\x1b[0m"
}

func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) green(t string) string  { return s.wrap("1;38;5;42", t) }
func (s style) pink(t string) string   { return s.wrap("1;38;5;211", t) }
func (s style) blue(t string) string   { return s.wrap("1;38;5;39", t) }
func (s style) yellow(t string) string { return s.wrap("38;5;221", t) }
func (s style) purple(t string) string { return s.wrap("38;5;177", t) }
func (s style) cyan(t string) string   { return s.wrap("38;5;80", t) }
func (s style) red(t string) string    { return s.wrap("38;5;203", t) }
