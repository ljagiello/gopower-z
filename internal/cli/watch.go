package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

type session struct {
	start      time.Time
	last       time.Time
	v, i, p    stat
	energy     integrator // same rule as measure
	history    []float64
	pd         pdTracker
	pdLines    []string
	samples    int
	errors     int
	lastErr    string
	sampleRate time.Duration
}

func (s *session) reset() {
	*s = session{pd: s.pd, pdLines: s.pdLines, sampleRate: s.sampleRate}
}

func (s *session) add(now time.Time, a km003c.ADC) {
	if s.start.IsZero() {
		s.start = now
	}
	s.energy.add(now, a.FlowPower(), a.FlowA())
	s.last = now
	s.v.add(a.VBus())
	s.i.add(a.FlowA())
	s.p.add(a.FlowPower())
	s.history = append(s.history, math.Abs(a.FlowPower()))
	if len(s.history) > 512 {
		s.history = s.history[len(s.history)-512:]
	}
	s.samples++
}

func (s *session) addPD(pd *km003c.PDPacket) {
	for _, rec := range s.pd.packet(pd) {
		s.pdLines = append(s.pdLines, strings.Split(rec.text(), "\n")...)
	}
	if len(s.pdLines) > 200 {
		s.pdLines = s.pdLines[len(s.pdLines)-200:]
	}
}

func setupWatch(fs *pflag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 200*time.Millisecond, "refresh interval (min 50ms)")
	noPD := fs.Bool("no-pd", false, "do not read the PD event stream")
	return func(ctx context.Context, g globals) error {
		if err := checkInterval("watch", *interval, 50*time.Millisecond); err != nil {
			return err
		}
		return runWatch(ctx, g, *interval, *noPD)
	}
}

func runWatch(ctx context.Context, g globals, interval time.Duration, noPD bool) error {
	if !g.env.tty || !isTTY(os.Stdin) {
		return usageErr("watch needs an interactive terminal; use `powerz measure --duration 10s` or `powerz log --duration 10s` instead")
	}

	m, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer m.close()

	// ReadInfo also selects the firmware-dependent ADC decoding. A partial
	// result still names what could be read.
	info, _ := m.ReadInfo(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Raw keyboard input for single-key commands.
	keys := make(chan byte, 8)
	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), old) }()
		go func() {
			b := make([]byte, 1)
			for {
				if n, err := os.Stdin.Read(b); err != nil || n == 0 {
					return
				}
				keys <- b[0]
			}
		}()
	}

	out := os.Stdout
	_, _ = fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer func() { _, _ = fmt.Fprint(out, "\x1b[?25h\x1b[?1049l") }()

	st := style{on: true}
	s := &session{sampleRate: interval}
	var lastADC *km003c.ADC
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		var a km003c.ADC
		var pd *km003c.PDPacket
		var err error
		if noPD {
			a, err = m.ReadADC(ctx)
		} else {
			a, pd, err = m.ReadADCAndPD(ctx)
		}
		now := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.errors++
			s.lastErr = err.Error()
		} else {
			s.add(now, a)
			s.addPD(pd)
			lastADC = &a
		}

		w, h, _ := term.GetSize(int(out.Fd()))
		_, _ = fmt.Fprint(out, render(st, s, lastADC, info, m.usb, w, h))

		select {
		case <-ctx.Done():
			return ctx.Err() // SIGINT/SIGTERM: exit 130 like every other command
		case k := <-keys:
			switch k {
			case 'q', 'Q', 3: // q, Ctrl-C
				return nil
			case 'r', 'R':
				s.reset()
			case 'c', 'C':
				s.pdLines = nil
			}
		case <-tick.C:
		}
	}
}

func render(st style, s *session, a *km003c.ADC, info km003c.DeviceInfo,
	usb km003c.USBDescription, w, h int) string {
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\x1b[K\r\n")
	}
	b.WriteString("\x1b[H")

	title := st.bold("POWER-Z " + orDefault(info.Model, "KM003C"))
	meta := []string{}
	if info.FirmwareVersion != "" {
		meta = append(meta, "fw "+info.FirmwareVersion)
	}
	if sn := orDefault(info.Serial, usb.Serial); sn != "" {
		meta = append(meta, "SN "+sn)
	}
	meta = append(meta, fmt.Sprintf("%d Hz", int(time.Second/s.sampleRate)))
	line(" %s  %s", title, st.dim(strings.Join(meta, " · ")))
	line("")

	if a == nil {
		line("  waiting for data…")
		if s.lastErr != "" {
			line("  %s", st.red(s.lastErr))
		}
		b.WriteString("\x1b[J")
		return b.String()
	}

	big := func(label, val, unit string, color func(string) string, sv stat, avgDev float64) {
		line("  %s %s %s   %s", st.dim(fmt.Sprintf("%-4s", label)), color(fmt.Sprintf("%12s", val)), color(unit),
			st.dim(fmt.Sprintf("meter-avg %s  min %s  max %s", f6(avgDev), f6(sv.min), f6(sv.max))))
	}
	big("VBUS", grouped(a.VBus()), "V", st.green, s.v, a.VBusAvg())
	big("IBUS", grouped(a.FlowA()), "A", st.pink, s.i, a.FlowAvgA())
	big("PWR", grouped(a.FlowPower()), "W", st.blue, s.p, a.FlowPowerAvg())
	line("")

	el := time.Duration(0)
	if !s.start.IsZero() {
		el = s.last.Sub(s.start).Round(time.Second)
	}
	wh, ah, _ := s.energy.at(s.last)
	line("  %s %-14s %s %-14s %s %-10s %s %s",
		st.dim("energy"), fmt.Sprintf("%.4f Wh", nz(wh, 5e-5)),
		st.dim("charge"), fmt.Sprintf("%.2f mAh", nz(ah*1000, 5e-3)),
		st.dim("time"), el.String(),
		st.dim("avg P"), fmt.Sprintf("%.3f W", nz(s.p.avg(), 5e-4)))
	temp := st.red("invalid")
	if a.TempValid() {
		temp = fmt.Sprintf("%.1f °C", a.TempC())
	}
	line("  %s %-14s %s %-14s %s %d %s",
		st.dim("temp  "), temp, st.dim("VDD   "), fmt.Sprintf("%.4f V", a.VDDV()),
		st.dim("samples"), s.samples, st.dim(fmt.Sprintf("errors %d", s.errors)))
	line("")
	line("  %s %s   %s %s   %s", st.cyan("CC1"), fmt.Sprintf("%.4f V", a.CC1V()),
		st.purple("CC2"), fmt.Sprintf("%.4f V", a.CC2V()), st.dim(ccHint(a.CC1V(), a.CC2V())))
	l1, l2, ok := km003c.DashboardLabels(*a)
	if !ok {
		l1, l2 = "unknown for this firmware", ""
	}
	line("  %s %s   %s %s   %s %s", st.green("D+ "), fmt.Sprintf("%.4f V", a.DPV()),
		st.yellow("D- "), fmt.Sprintf("%.4f V", a.DMV()), st.bold("Protocol "+l1), st.dim(l2))
	line("")
	sw := max(w-12, 10)
	line("  %s %s", st.dim("|P| "), st.blue(sparkline(s.history, sw)))
	line("")

	if caps := s.pd.capsText(); caps != "" {
		line("  %s %s", st.bold("Source caps"), truncate(caps, w-16))
	}
	if s.pd.contract != nil {
		line("  %s %s", st.bold("Contract   "), truncate(s.pd.contract.Text, w-16))
	}
	line("  %s", st.bold("USB-PD log")+st.dim("  (c clears)"))
	used := strings.Count(b.String(), "\r\n") + 2
	avail := max(h-used, 0)
	pdl := s.pdLines
	if len(pdl) > avail {
		pdl = pdl[len(pdl)-avail:]
	}
	if len(pdl) == 0 && avail > 0 {
		line("  %s", st.dim("no PD traffic captured yet"))
	}
	for _, l := range pdl {
		line("  %s", truncate(l, w-3))
	}
	b.WriteString("\x1b[J")
	footer := " q quit · r reset stats · c clear PD log"
	if s.lastErr != "" {
		footer += " · last error: " + s.lastErr
	}
	fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[K", h, st.dim(truncate(footer, w-1)))
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
