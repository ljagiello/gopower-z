package cli

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/spf13/pflag"
)

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe)
}

// checkInterval rejects polling intervals the meter cannot serve (instead of
// silently changing them).
func checkInterval(cmd string, d, minimum time.Duration) error {
	if d < minimum {
		return withHint(usageErr("%s: --interval must be at least %v (got %v)", cmd, minimum, d),
			fmt.Sprintf("use e.g. --interval %v; the meter itself samples at 2-1000 SPS, so faster polling only repeats values", max(minimum, 100*time.Millisecond)))
	}
	return nil
}

// endReason says how a measure or pd window ended.
func endReason(stop error) string {
	switch {
	case stop == nil:
		return "completed"
	case errors.Is(stop, context.Canceled):
		return "interrupted"
	}
	return "error"
}

// ---------------------------------------------------------------- read

func setupRead(fs *pflag.FlagSet) func(context.Context, globals) error {
	return func(ctx context.Context, g globals) error {
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		m.detect(ctx)
		a, err := m.ReadADC(ctx)
		if err != nil {
			return err
		}
		snap := toSnapshot(a, time.Now(), m.usb.Serial, m.FirmwareVersion(), m.firmwareError())
		if g.jsonMode() {
			return g.emitIndented(snap)
		}
		printSnapshotText(g.out(), g.style(), snap, a)
		return nil
	}
}

func deref(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func printSnapshotText(w io.Writer, st style, s snapshot, a km003c.ADC) {
	row := func(label, val, extra string) {
		_, _ = fmt.Fprintf(w, "  %-6s %s  %s\n", label, val, st.dim(extra))
	}
	_, _ = fmt.Fprintln(w, st.bold("POWER-Z KM003C")+st.dim("  "+s.Serial+"  "+s.Time))
	row("VBUS", st.green(grouped(a.VBus())+" V"), fmt.Sprintf("meter avg %.6f V", s.VBusAvgV))
	row("IBUS", st.pink(grouped(a.FlowA())+" A"), fmt.Sprintf("meter avg %.6f A · flow %s", s.IBusAvgA, s.Direction))
	row("PWR", st.blue(grouped(a.FlowPower())+" W"), fmt.Sprintf("meter avg %.6f W", s.PowerAvgW))
	temp := "invalid"
	if s.TempC != nil {
		temp = fmt.Sprintf("%.2f °C", *s.TempC)
	}
	row("TEMP", temp, "")
	row("CC1", st.cyan(fmt.Sprintf("%.4f V", s.CC1V)), "")
	row("CC2", st.purple(fmt.Sprintf("%.4f V", s.CC2V)), avgText(s.CC2AvgV))
	row("D+", st.green(fmt.Sprintf("%.4f V", s.DPV)), avgText(s.DPAvgV))
	row("D-", st.yellow(fmt.Sprintf("%.4f V", s.DMV)), avgText(s.DMAvgV))
	row("VDD", fmt.Sprintf("%.4f V", s.VDDV), "meter internal supply")
	_, _ = fmt.Fprintln(w)
	if s.Inferred.DashboardClassifier != nil {
		_, _ = fmt.Fprintln(w, "  "+st.dim("label: ")+deref(s.Inferred.DashboardLabel, "[N/A]")+" / "+
			deref(s.Inferred.DashboardLabel2, "[N/A]")+st.dim("  (meter's D+/D- dashboard labels, recomputed)"))
	} else {
		_, _ = fmt.Fprintln(w, "  "+st.dim("label: unknown for this firmware"))
	}
	_, _ = fmt.Fprintln(w, "  "+st.dim("CC:    ")+s.Inferred.CC.Text+st.dim("  (inferred)"))
	switch {
	case s.FirmwareVersion != nil:
		_, _ = fmt.Fprintln(w, "  "+st.dim("firmware "+*s.FirmwareVersion))
	case s.FirmwareError != nil:
		_, _ = fmt.Fprintln(w, "  "+st.dim("firmware unknown: "+*s.FirmwareError))
	}
}

func avgText(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("meter avg %.3f V", *v)
}

// ---------------------------------------------------------------- measure

type measureResult struct {
	SchemaVersion   int          `json:"schema_version"`
	Serial          string       `json:"serial"`
	FirmwareVersion *string      `json:"firmware_version"`
	FirmwareError   *string      `json:"firmware_error"`
	Start           string       `json:"start"`
	End             string       `json:"end"`
	EndReason       string       `json:"end_reason"`
	DurationS       float64      `json:"duration_s"`
	IntegratedS     float64      `json:"integrated_s"`
	IntervalS       float64      `json:"interval_s"`
	Samples         int          `json:"samples"`
	FailedSamples   int          `json:"failed_samples"`
	LastError       *string      `json:"last_error"`
	Direction       string       `json:"direction"`
	Stats           measureStats `json:"stats"`
	BinS            float64      `json:"bin_s"`
	Bins            []binJSON    `json:"bins"`
	Shifts          []shiftJSON  `json:"shifts"`
	EnergyWh        float64      `json:"energy_wh"`
	ChargeMAh       float64      `json:"charge_mah"`
	Last            *snapshot    `json:"last"`
	PD              *pdSummary   `json:"pd"`
}

// integrator accumulates energy and charge, holding each sample until the
// next one arrives (and the last one until the end of the window), so the
// integrated time equals the sampled window. measure and watch share it.
type integrator struct {
	first, prevT time.Time
	prevW, prevA float64
	wh, ah       float64
	ok           bool
}

func (in *integrator) add(t time.Time, w, a float64) {
	if in.ok {
		dt := t.Sub(in.prevT).Hours()
		in.wh += in.prevW * dt
		in.ah += in.prevA * dt
	} else {
		in.first = t
	}
	in.prevT, in.prevW, in.prevA, in.ok = t, w, a, true
}

// at returns the totals up to t (the last sample held until t) without
// changing the integrator.
func (in integrator) at(t time.Time) (wh, ah, seconds float64) {
	if !in.ok {
		return 0, 0, 0
	}
	in.add(t, 0, 0)
	return in.wh, in.ah, t.Sub(in.first).Seconds()
}

// fatalMidRun reports errors that end a sampling window early: the meter
// went away, was taken by another program, or cannot be talked to safely.
func fatalMidRun(err error) bool {
	return errors.Is(err, km003c.ErrNotFound) || errors.Is(err, km003c.ErrBusy) || errors.Is(err, km003c.ErrStuck)
}

func setupMeasure(fs *pflag.FlagSet) func(context.Context, globals) error {
	duration := fs.Duration("duration", 5*time.Second, "how long to sample")
	interval := fs.Duration("interval", 100*time.Millisecond, "sampling interval, at least 20ms (the meter's own sample rate bounds how often values change)")
	withPD := fs.Bool("pd", true, "also capture USB-PD traffic (source capabilities, negotiated contract, records)")
	bin := fs.Duration("bin", 0, fmt.Sprintf("width of the time bins in the output (0 = automatic: at least 5 samples per bin, at most %d bins)", maxAutoBins))
	return func(ctx context.Context, g globals) error {
		if *duration <= 0 {
			return usageErr("measure: --duration must be positive (e.g. 10s)")
		}
		if err := checkInterval("measure", *interval, 20*time.Millisecond); err != nil {
			return err
		}
		binW := *bin
		switch {
		case binW < 0:
			return usageErr("measure: --bin must not be negative")
		case binW == 0:
			binW = autoBin(*duration, *interval)
		case binW < *interval:
			return usageErr("measure: --bin %s is shorter than --interval %s", binW, *interval)
		case math.Ceil(duration.Seconds()/binW.Seconds()) > maxBins:
			return usageErr("measure: --bin %s gives more than %d bins over --duration %s; use a wider bin", binW, maxBins, *duration)
		}
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		m.detect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var samples []sample
		var in integrator
		var last *snapshot
		var lastErr, stop error
		tr := &pdTracker{}
		recs := []pdRecord{}
		start := time.Now()
		res := measureResult{SchemaVersion: schemaVersion, Serial: m.usb.Serial,
			FirmwareVersion: strOrNil(m.FirmwareVersion()), FirmwareError: m.firmwareError(),
			Start: utcTime(start), IntervalS: interval.Seconds()}

		deadline := time.NewTimer(*duration)
		defer deadline.Stop()
		tick := time.NewTicker(*interval)
		defer tick.Stop()
	loop:
		for {
			var a km003c.ADC
			var pd *km003c.PDPacket
			if *withPD {
				a, pd, err = m.ReadADCAndPD(ctx)
			} else {
				a, err = m.ReadADC(ctx)
			}
			now := time.Now()
			switch {
			case ctx.Err() != nil:
				stop = ctx.Err()
				break loop
			case err != nil:
				res.FailedSamples++
				res.LastError = new(err.Error())
				lastErr = err
				if fatalMidRun(err) {
					stop = err
					break loop
				}
			default:
				in.add(now, a.FlowPower(), a.FlowA())
				samples = append(samples, sample{T: now.Sub(start).Seconds(), V: a.VBus(), I: a.FlowA(), P: a.FlowPower(),
					DP: a.DPV(), DM: a.DMV(), Temp: a.TempC(), TempOK: a.TempValid()})
				s := toSnapshot(a, now, m.usb.Serial, m.FirmwareVersion(), m.firmwareError())
				last = &s
				res.Samples++
				recs = append(recs, tr.packet(pd)...)
			}
			select {
			case <-ctx.Done():
				stop = ctx.Err()
				break loop
			case <-deadline.C:
				break loop
			case <-tick.C:
			}
		}
		end := time.Now()
		wh, ah, integrated := in.at(end)
		res.End = utcTime(end)
		res.EndReason = endReason(stop)
		res.DurationS = round(end.Sub(start).Seconds(), 3)
		res.IntegratedS = round(integrated, 3)
		an := analyze(samples, end.Sub(start).Seconds(), binW.Seconds())
		res.Stats, res.BinS, res.Bins, res.Shifts = an.Stats, an.BinS, an.Bins, an.Shifts
		res.Direction = string(km003c.FlowNone)
		if an.Stats.IBusA != nil {
			res.Direction = meanDirection(an.Stats.IBusA.Mean)
		}
		res.EnergyWh = round(wh, 9)
		res.ChargeMAh = round(ah*1000, 6)
		res.Last = last
		if *withPD {
			res.PD = tr.summary(recs)
		}
		if res.Samples == 0 {
			if stop != nil {
				return stop
			}
			if lastErr == nil {
				lastErr = km003c.ErrTimeout
			}
			return fmt.Errorf("measure: no samples collected (%d failed): %w", res.FailedSamples, lastErr)
		}
		// Print what was collected even when the window ended early (an
		// agent harness timeout sends SIGTERM): end_reason says why.
		var perr error
		if g.jsonMode() {
			perr = g.emitIndented(res)
		} else {
			printMeasureText(g.out(), g.style(), res)
		}
		switch {
		case stop != nil:
			return stop
		case perr != nil:
			return perr
		case res.FailedSamples > 0:
			return partialErr{fmt.Sprintf("%d of %d samples failed (last: %s)",
				res.FailedSamples, res.Samples+res.FailedSamples, deref(res.LastError, ""))}
		}
		return nil
	}
}

func printMeasureText(w io.Writer, st style, r measureResult) {
	_, _ = fmt.Fprintf(w, "%s  %s  %.1fs  %d samples  %d failed  flow %s  (%s)\n", st.bold("POWER-Z measure"),
		r.Serial, r.DurationS, r.Samples, r.FailedSamples, r.Direction, r.EndReason)
	// Text mode is for people: current in mA, the mean with its 95% interval,
	// then the spread. JSON keeps the documented units.
	row := func(name, unit string, scale float64, prec int, s *statJSON) {
		if s == nil {
			return
		}
		f := func(v float64) string { return strconv.FormatFloat(v*scale, 'f', prec, 64) }
		ci := ""
		if s.CI95 != nil {
			ci = " ±" + f(*s.CI95)
		}
		_, _ = fmt.Fprintf(w, "  %-5s mean %s%s %s   min %s  p5 %s  p50 %s  p95 %s  max %s  sd %s\n", name,
			f(s.Mean), ci, unit, f(s.Min), f(s.P5), f(s.P50), f(s.P95), f(s.Max), f(s.Stdev))
	}
	row("VBUS", "V", 1, 4, r.Stats.VBusV)
	row("IBUS", "mA", 1000, 3, r.Stats.IBusA)
	row("PWR", "W", 1, 4, r.Stats.PowerW)
	row("TEMP", "°C", 1, 2, r.Stats.TempC)
	if r.Stats.DPV != nil && r.Stats.DMV != nil {
		_, _ = fmt.Fprintf(w, "  D+ %.3f..%.3f V   D- %.3f..%.3f V\n", r.Stats.DPV.Min, r.Stats.DPV.Max, r.Stats.DMV.Min, r.Stats.DMV.Max)
	}
	_, _ = fmt.Fprintf(w, "  energy %.6f Wh   charge %.4f mAh   over %.3f s\n", r.EnergyWh, r.ChargeMAh, r.IntegratedS)
	var means []float64
	for _, b := range r.Bins {
		if b.IBusA != nil {
			means = append(means, b.IBusA.Mean)
		}
	}
	if len(means) > 1 {
		_, _ = fmt.Fprintf(w, "  IBUS per %gs bin  %s\n", r.BinS, sparkline(means, 120))
	}
	for _, s := range r.Shifts {
		unit, scale, prec := "V", 1.0, 3
		if s.Field == "ibus_a" {
			unit, scale, prec = "mA", 1000, 1
		}
		_, _ = fmt.Fprintf(w, "  shift at %.3fs  %s  %.*f -> %.*f %s\n", s.TS, s.Field, prec, s.From*scale, prec, s.To*scale, unit)
	}
	if r.PD != nil {
		_, _ = fmt.Fprintf(w, "  PD: %d messages, %d events\n", r.PD.Messages, r.PD.Events)
		for _, p := range r.PD.SourceCapabilities {
			_, _ = fmt.Fprintf(w, "    #%d %s\n", p.Index, p.Text)
		}
		switch {
		case r.PD.Contract != nil:
			_, _ = fmt.Fprintf(w, "  contract: %s\n", r.PD.Contract.Text)
		case r.PD.LastRequest != nil:
			_, _ = fmt.Fprintf(w, "  last request: %s\n", r.PD.LastRequest.Text)
		}
		if r.PD.Note != nil {
			_, _ = fmt.Fprintln(w, "  "+st.dim(*r.PD.Note))
		}
	}
}

// ---------------------------------------------------------------- log

// csvColumns are the columns of `log --format csv`, in order.
var csvColumns = []string{"time", "vbus_v", "ibus_a", "power_w", "direction", "vbus_avg_v", "ibus_avg_a",
	"power_avg_w", "temp_c", "cc1_v", "cc2_v", "dp_v", "dm_v", "vdd_v"}

func setupLog(fs *pflag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 200*time.Millisecond, "sampling interval, at least 20ms")
	format := fs.String("format", "", "jsonl or csv (default: jsonl in JSON mode, csv otherwise); csv columns: "+strings.Join(csvColumns, ","))
	count := fs.IntP("count", "n", 0, "stop after this many samples (0 = no limit)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; automation should always set --duration or --count)")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("log: --count and --duration must not be negative")
		}
		if err := checkInterval("log", *interval, 20*time.Millisecond); err != nil {
			return err
		}
		f := *format
		if f == "" {
			f = map[bool]string{true: "jsonl", false: "csv"}[g.jsonMode()]
		}
		var write func(snapshot) error
		switch f {
		case "jsonl", "json":
			write = func(s snapshot) error { return g.emit(s) }
		case "csv":
			w := csv.NewWriter(g.out())
			defer w.Flush()
			if err := w.Write(csvColumns); err != nil {
				return err
			}
			ff := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
			write = func(s snapshot) error {
				temp := ""
				if s.TempC != nil {
					temp = ff(*s.TempC)
				}
				_ = w.Write([]string{s.Time, ff(s.VBusV), ff(s.IBusA), ff(s.PowerW), s.Direction,
					ff(s.VBusAvgV), ff(s.IBusAvgA), ff(s.PowerAvgW), temp,
					ff(s.CC1V), ff(s.CC2V), ff(s.DPV), ff(s.DMV), ff(s.VDDV)})
				w.Flush()
				return w.Error()
			}
		default:
			return usageErr("log: unknown --format %q (want jsonl or csv)", f)
		}

		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		m.detect(ctx)
		parent := ctx
		if *duration > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *duration)
			defer cancel()
		}
		tick := time.NewTicker(*interval)
		defer tick.Stop()
		for n := 0; *count == 0 || n < *count; n++ {
			a, err := m.ReadADC(ctx)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				return err
			}
			if err := write(toSnapshot(a, time.Now(), m.usb.Serial, m.FirmwareVersion(), m.firmwareError())); err != nil {
				if isBrokenPipe(err) {
					return nil
				}
				return err
			}
			if *count > 0 && n+1 >= *count {
				break
			}
			select {
			case <-ctx.Done():
			case <-tick.C:
				continue
			}
			break
		}
		if errors.Is(parent.Err(), context.Canceled) {
			return parent.Err()
		}
		return nil
	}
}

// ---------------------------------------------------------------- pd

// pdEnd is the final line of `pd` output in JSON mode.
type pdEnd struct {
	SchemaVersion      int     `json:"schema_version"`
	Kind               string  `json:"kind"` // "summary"
	Time               string  `json:"time"`
	EndReason          string  `json:"end_reason"`
	DurationS          float64 `json:"duration_s"`
	Messages           int     `json:"messages"`
	Events             int     `json:"events"`
	Undecoded          int     `json:"undecoded"`
	SourceCapabilities []pdo   `json:"source_capabilities"`
	LastRequest        *rdo    `json:"last_request"`
	Contract           *rdo    `json:"contract"`
	Note               *string `json:"note"`
}

func newPDEnd(sum *pdSummary, now time.Time, durationS float64, reason string) pdEnd {
	return pdEnd{SchemaVersion: schemaVersion, Kind: "summary", Time: utcTime(now), EndReason: reason,
		DurationS: round(durationS, 3), Messages: sum.Messages, Events: sum.Events, Undecoded: sum.Undecoded,
		SourceCapabilities: sum.SourceCapabilities, LastRequest: sum.LastRequest, Contract: sum.Contract, Note: sum.Note}
}

func setupPD(fs *pflag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 25*time.Millisecond, "polling interval, at least 10ms (the meter buffers PD traffic between polls)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; automation should always set --duration)")
	count := fs.IntP("count", "n", 0, "stop after this many records (0 = no limit; without PD traffic it waits, so combine with --duration)")
	goodCRC := fs.Bool("goodcrc", false, "include GoodCRC acknowledgements")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("pd: --count and --duration must not be negative")
		}
		if err := checkInterval("pd", *interval, 10*time.Millisecond); err != nil {
			return err
		}
		// The PD stream does not depend on the firmware version, so no
		// CONNECT or MemoryRead is sent: only GET_DATA for PD_PACKET.
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		parent := ctx
		if *duration > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *duration)
			defer cancel()
		}
		jsonOut := g.jsonMode()
		if !jsonOut {
			_, _ = fmt.Fprintln(g.env.stderr, "listening for USB-PD traffic (passive; Ctrl-C to stop)…")
		}
		start := time.Now()
		tr := &pdTracker{showGoodCRC: *goodCRC}
		tick := time.NewTicker(*interval)
		defer tick.Stop()
		n := 0
		// finish prints the summary; it runs however the capture ends.
		finish := func(stop error) error {
			sum := tr.summary(nil)
			var err error
			if jsonOut {
				err = g.emit(newPDEnd(sum, time.Now(), time.Since(start).Seconds(), endReason(stop)))
			} else {
				_, _ = fmt.Fprintf(g.env.stderr, "%d messages, %d events (%s)\n", sum.Messages, sum.Events, endReason(stop))
				if sum.Note != nil {
					_, _ = fmt.Fprintln(g.env.stderr, *sum.Note)
				}
			}
			switch {
			case stop != nil:
				return stop
			case isBrokenPipe(err):
				return nil
			}
			return err
		}
		for {
			pd, err := m.ReadPD(ctx)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				return finish(err)
			}
			for _, rec := range tr.packet(&pd) {
				if jsonOut {
					err = g.emit(rec)
				} else {
					_, err = fmt.Fprintln(g.out(), rec.text())
				}
				if err != nil {
					if isBrokenPipe(err) {
						return nil
					}
					return err
				}
				if n++; *count > 0 && n >= *count {
					return finish(nil)
				}
			}
			select {
			case <-ctx.Done():
			case <-tick.C:
				continue
			}
			break
		}
		if errors.Is(parent.Err(), context.Canceled) {
			return finish(parent.Err())
		}
		return finish(nil)
	}
}

// ---------------------------------------------------------------- info

type infoResult struct {
	SchemaVersion int                   `json:"schema_version"`
	USB           km003c.USBDescription `json:"usb"`
	Device        *deviceJSON           `json:"device"`
	Settings      *settingsJSON         `json:"settings"`
	Errors        []string              `json:"errors"`
}

type deviceJSON struct {
	Model           *string `json:"model"`
	FirmwareVersion *string `json:"firmware_version"`
	FirmwareDate    *string `json:"firmware_date"`
	HardwareVersion *string `json:"hardware_version"`
	HardwareDate    *string `json:"hardware_date"`
	Serial          *string `json:"serial"`
	UUID            *string `json:"uuid"`
	TailLayout      string  `json:"adc_tail_layout"`
}

type settingsJSON struct {
	DeviceName        string `json:"device_name"`
	SampleRateSPS     int    `json:"sample_rate_sps"`
	BrightnessPercent int    `json:"brightness_percent"`
	RotationCode      int    `json:"rotation_code"`
	AutoRotate        bool   `json:"auto_rotate"`
	LanguageCode      int    `json:"language_code"`
	ThemeCode         int    `json:"theme_code"`
	ChecksumOK        bool   `json:"checksum_ok"`
}

// splitErrors lists the errors joined by errors.Join.
func splitErrors(err error) []error {
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return u.Unwrap()
	}
	return []error{err}
}

func setupInfo(fs *pflag.FlagSet) func(context.Context, globals) error {
	return func(ctx context.Context, g globals) error {
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		res := infoResult{SchemaVersion: schemaVersion, USB: m.usb, Errors: []string{}}
		info, err := m.ReadInfo(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			for _, e := range splitErrors(err) {
				res.Errors = append(res.Errors, "device: "+e.Error())
			}
		}
		if info != (km003c.DeviceInfo{}) {
			res.Device = &deviceJSON{
				Model: strOrNil(info.Model), FirmwareVersion: strOrNil(info.FirmwareVersion),
				FirmwareDate: strOrNil(isoDate(info.FirmwareDate)), HardwareVersion: strOrNil(info.HardwareVersion),
				HardwareDate: strOrNil(isoDate(info.HardwareDate)), Serial: strOrNil(info.Serial),
				UUID: strOrNil(info.UUID), TailLayout: m.Layout().String(),
			}
		}
		if set, err := m.ReadSettings(ctx); err != nil {
			res.Errors = append(res.Errors, "settings: "+err.Error())
		} else {
			res.Settings = &settingsJSON{
				DeviceName: set.DeviceName, SampleRateSPS: set.SampleRate.SamplesPerSecond(),
				BrightnessPercent: set.Brightness, RotationCode: set.Rotation, AutoRotate: set.AutoRotate,
				LanguageCode: set.Language, ThemeCode: set.Theme, ChecksumOK: set.CRCAOK && set.CRCBOK,
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if g.jsonMode() {
			if err := g.emitIndented(res); err != nil {
				return err
			}
		} else {
			printInfoText(g.out(), g.style(), res)
		}
		if len(res.Errors) > 0 {
			return partialErr{strings.Join(res.Errors, "; ")}
		}
		return nil
	}
}

func printInfoText(w io.Writer, st style, r infoResult) {
	kv := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(w, "  %-18s %s\n", k, v)
		}
	}
	_, _ = fmt.Fprintln(w, st.bold("USB"))
	kv("Manufacturer", r.USB.Manufacturer)
	kv("Product", r.USB.Product)
	kv("USB serial", r.USB.Serial)
	kv("Bus/address", fmt.Sprintf("%d/%d", r.USB.Bus, r.USB.Address))
	if d := r.Device; d != nil {
		_, _ = fmt.Fprintln(w, st.bold("Device"))
		kv("Model", deref(d.Model, ""))
		kv("Firmware", deref(d.FirmwareVersion, ""))
		kv("Firmware date", deref(d.FirmwareDate, ""))
		kv("Hardware/boot ver", deref(d.HardwareVersion, ""))
		kv("Hardware date", deref(d.HardwareDate, ""))
		kv("Serial", deref(d.Serial, ""))
		kv("UUID", deref(d.UUID, ""))
	}
	if s := r.Settings; s != nil {
		_, _ = fmt.Fprintln(w, st.bold("Settings"))
		kv("Device name", s.DeviceName)
		kv("Sample rate", fmt.Sprintf("%d SPS", s.SampleRateSPS))
		kv("Brightness", fmt.Sprintf("%d %%", s.BrightnessPercent))
		kv("Rotation code", strconv.Itoa(s.RotationCode))
		kv("Auto-rotate", strconv.FormatBool(s.AutoRotate))
		kv("Checksum", map[bool]string{true: "ok", false: "mismatch"}[s.ChecksumOK])
	}
	for _, e := range r.Errors {
		_, _ = fmt.Fprintln(w, st.red("  error: "+e))
	}
}

// ---------------------------------------------------------------- devices

type devicesResult struct {
	SchemaVersion int                     `json:"schema_version"`
	Count         int                     `json:"count"`
	Devices       []km003c.USBDescription `json:"devices"`
}

func setupDevices(fs *pflag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		devs, err := g.env.listUSB()
		if err != nil {
			return err
		}
		if g.jsonMode() {
			return g.emitIndented(devicesResult{SchemaVersion: schemaVersion, Count: len(devs), Devices: devs})
		}
		if len(devs) == 0 {
			_, err := fmt.Fprintln(g.out(), "no POWER-Z KM003C connected")
			return err
		}
		for _, d := range devs {
			_, _ = fmt.Fprintf(g.out(), "%s  %s %s  bus %d address %d\n", d.Serial, d.Manufacturer, d.Product, d.Bus, d.Address)
		}
		return nil
	}
}
