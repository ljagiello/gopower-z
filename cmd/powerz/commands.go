package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
)

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe)
}

// checkInterval rejects polling intervals the meter cannot serve (instead of
// silently changing them).
func checkInterval(cmd string, d, minimum time.Duration) error {
	if d < minimum {
		return withHint(usageErr("%s: -interval must be at least %v (got %v)", cmd, minimum, d),
			fmt.Sprintf("use e.g. -interval %v; the meter itself samples at 2-1000 SPS, so faster polling only repeats values", max(minimum, 100*time.Millisecond)))
	}
	return nil
}

// ---------------------------------------------------------------- read

func setupRead(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(ctx context.Context, g globals) error {
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		a, err := m.ReadADC(ctx)
		if err != nil {
			return err
		}
		snap := toSnapshot(a, time.Now(), m.usb.Serial, m.FirmwareVersion())
		if g.jsonMode() {
			return emitIndented(snap)
		}
		printSnapshotText(snap, a)
		return nil
	}
}

func deref(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func printSnapshotText(s snapshot, a km003c.ADC) {
	st := style{on: colorOn()}
	row := func(label, val, extra string) {
		fmt.Printf("  %-6s %s  %s\n", label, val, st.dim(extra))
	}
	fmt.Println(st.bold("POWER-Z KM003C") + st.dim("  "+s.Serial+"  "+s.Time))
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
	fmt.Println()
	fmt.Println("  " + st.dim("label: ") + deref(s.Inferred.DashboardLabel, "[N/A]") + " / " +
		deref(s.Inferred.DashboardLabel2, "[N/A]") + st.dim("  (meter's D+/D- dashboard labels, recomputed)"))
	fmt.Println("  " + st.dim("CC:    ") + s.Inferred.CC.Text + st.dim("  (inferred)"))
	if s.FirmwareVersion != nil {
		fmt.Println("  " + st.dim("firmware "+*s.FirmwareVersion))
	}
}

func avgText(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("meter avg %.3f V", *v)
}

// ---------------------------------------------------------------- measure

type measureStats struct {
	VBusV  *statJSON `json:"vbus_v"`
	IBusA  *statJSON `json:"ibus_a"`
	PowerW *statJSON `json:"power_w"`
	TempC  *statJSON `json:"temp_c"`
}

type measureResult struct {
	SchemaVersion   int          `json:"schema_version"`
	Serial          string       `json:"serial"`
	FirmwareVersion *string      `json:"firmware_version"`
	Start           string       `json:"start"`
	End             string       `json:"end"`
	DurationS       float64      `json:"duration_s"`
	IntegratedS     float64      `json:"integrated_s"`
	IntervalS       float64      `json:"interval_s"`
	Samples         int          `json:"samples"`
	FailedSamples   int          `json:"failed_samples"`
	LastError       *string      `json:"last_error"`
	Direction       string       `json:"direction"`
	Stats           measureStats `json:"stats"`
	EnergyWh        float64      `json:"energy_wh"`
	ChargeMAh       float64      `json:"charge_mah"`
	Last            *snapshot    `json:"last"`
	PD              *pdSummary   `json:"pd"`
}

// integrator accumulates energy and charge, holding each sample until the
// next one arrives (and the last one until the end of the window), so the
// integrated time equals the sampled window.
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

func (in *integrator) finish(end time.Time) (wh, ah, seconds float64) {
	if !in.ok {
		return 0, 0, 0
	}
	in.add(end, 0, 0)
	return in.wh, in.ah, end.Sub(in.first).Seconds()
}

func setupMeasure(fs *flag.FlagSet) func(context.Context, globals) error {
	duration := fs.Duration("duration", 5*time.Second, "how long to sample")
	interval := fs.Duration("interval", 100*time.Millisecond, "sampling interval, at least 20ms (the meter's own sample rate bounds how often values change)")
	withPD := fs.Bool("pd", true, "also capture USB-PD traffic (source capabilities, negotiated contract, records)")
	return func(ctx context.Context, g globals) error {
		if *duration <= 0 {
			return usageErr("measure: -duration must be positive (e.g. 10s)")
		}
		if err := checkInterval("measure", *interval, 20*time.Millisecond); err != nil {
			return err
		}
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()

		var v, i, p, temp stat
		var in integrator
		var last *snapshot
		var lastErr error
		tr := &pdTracker{}
		recs := []pdRecord{}
		start := time.Now()
		res := measureResult{SchemaVersion: schemaVersion, Serial: m.usb.Serial,
			FirmwareVersion: strOrNil(m.FirmwareVersion()), Start: utcTime(start), IntervalS: interval.Seconds()}

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
				return ctx.Err()
			case err != nil:
				res.FailedSamples++
				res.LastError = new(err.Error())
				lastErr = err
				if errors.Is(err, km003c.ErrNotFound) || errors.Is(err, km003c.ErrBusy) || errors.Is(err, km003c.ErrStuck) {
					return err
				}
			default:
				in.add(now, a.FlowPower(), a.FlowA())
				v.add(a.VBus())
				i.add(a.FlowA())
				p.add(a.FlowPower())
				if a.TempValid() {
					temp.add(a.TempC())
				}
				s := toSnapshot(a, now, m.usb.Serial, m.FirmwareVersion())
				last = &s
				res.Samples++
				if pd != nil {
					for _, r := range pd.Records {
						if rec, ok := tr.process(r); ok {
							recs = append(recs, rec)
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				break loop
			case <-tick.C:
			}
		}
		end := time.Now()
		wh, ah, integrated := in.finish(end)
		res.End = utcTime(end)
		res.DurationS = round(end.Sub(start).Seconds(), 3)
		res.IntegratedS = round(integrated, 3)
		res.Stats = measureStats{VBusV: v.json(6), IBusA: i.json(6), PowerW: p.json(6), TempC: temp.json(3)}
		res.Direction = meanDirection(i.avg())
		res.EnergyWh = round(wh, 9)
		res.ChargeMAh = round(ah*1000, 6)
		res.Last = last
		if *withPD {
			res.PD = tr.summary(recs)
		}
		if res.Samples == 0 {
			if lastErr == nil {
				lastErr = km003c.ErrTimeout
			}
			return fmt.Errorf("measure: no samples collected (%d failed): %w", res.FailedSamples, lastErr)
		}
		if g.jsonMode() {
			if err := emitIndented(res); err != nil {
				return err
			}
		} else {
			printMeasureText(res)
		}
		if res.FailedSamples > 0 {
			return partialErr{fmt.Sprintf("%d of %d samples failed (last: %s)",
				res.FailedSamples, res.Samples+res.FailedSamples, deref(res.LastError, ""))}
		}
		return nil
	}
}

func printMeasureText(r measureResult) {
	st := style{on: colorOn()}
	fmt.Printf("%s  %s  %.1fs  %d samples  %d failed  flow %s\n", st.bold("POWER-Z measure"),
		r.Serial, r.DurationS, r.Samples, r.FailedSamples, r.Direction)
	row := func(name, unit string, s *statJSON) {
		if s == nil {
			return
		}
		fmt.Printf("  %-5s min %12.6f  mean %12.6f  max %12.6f  last %12.6f %s\n", name, s.Min, s.Mean, s.Max, s.Last, unit)
	}
	row("VBUS", "V", r.Stats.VBusV)
	row("IBUS", "A", r.Stats.IBusA)
	row("PWR", "W", r.Stats.PowerW)
	row("TEMP", "°C", r.Stats.TempC)
	fmt.Printf("  energy %.6f Wh   charge %.4f mAh   over %.3f s\n", r.EnergyWh, r.ChargeMAh, r.IntegratedS)
	if r.PD != nil {
		fmt.Printf("  PD: %d messages, %d events\n", r.PD.Messages, r.PD.Events)
		for _, p := range r.PD.SourceCapabilities {
			fmt.Printf("    #%d %s\n", p.Index, p.Text)
		}
		switch {
		case r.PD.Contract != nil:
			fmt.Printf("  contract: %s\n", r.PD.Contract.Text)
		case r.PD.LastRequest != nil:
			fmt.Printf("  last request: %s\n", r.PD.LastRequest.Text)
		}
		if r.PD.Note != nil {
			fmt.Println("  " + st.dim(*r.PD.Note))
		}
	}
}

// ---------------------------------------------------------------- log

// csvColumns are the columns of `log -format csv`, in order.
var csvColumns = []string{"time", "vbus_v", "ibus_a", "power_w", "direction", "vbus_avg_v", "ibus_avg_a",
	"power_avg_w", "temp_c", "cc1_v", "cc2_v", "dp_v", "dm_v", "vdd_v"}

func setupLog(fs *flag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 200*time.Millisecond, "sampling interval, at least 20ms")
	format := fs.String("format", "", "jsonl or csv (default: jsonl in JSON mode, csv otherwise); csv columns: "+strings.Join(csvColumns, ","))
	count := fs.Int("n", 0, "stop after n samples (0 = no limit)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; automation should always set -duration or -n)")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("log: -n and -duration must not be negative")
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
			write = func(s snapshot) error { return emit(s) }
		case "csv":
			w := csv.NewWriter(os.Stdout)
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
			return usageErr("log: unknown -format %q (want jsonl or csv)", f)
		}

		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
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
			if err := write(toSnapshot(a, time.Now(), m.usb.Serial, m.FirmwareVersion())); err != nil {
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
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
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
	DurationS          float64 `json:"duration_s"`
	Messages           int     `json:"messages"`
	Events             int     `json:"events"`
	SourceCapabilities []pdo   `json:"source_capabilities"`
	LastRequest        *rdo    `json:"last_request"`
	Contract           *rdo    `json:"contract"`
	Note               *string `json:"note"`
}

func newPDEnd(sum *pdSummary, now time.Time, durationS float64) pdEnd {
	return pdEnd{SchemaVersion: schemaVersion, Kind: "summary", Time: utcTime(now), DurationS: round(durationS, 3),
		Messages: sum.Messages, Events: sum.Events, SourceCapabilities: sum.SourceCapabilities,
		LastRequest: sum.LastRequest, Contract: sum.Contract, Note: sum.Note}
}

func setupPD(fs *flag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 25*time.Millisecond, "polling interval, at least 10ms (the meter buffers PD traffic between polls)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; automation should always set -duration)")
	count := fs.Int("n", 0, "stop after n records (0 = no limit; without PD traffic it waits, so combine with -duration)")
	goodCRC := fs.Bool("goodcrc", false, "include GoodCRC acknowledgements")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("pd: -n and -duration must not be negative")
		}
		if err := checkInterval("pd", *interval, 10*time.Millisecond); err != nil {
			return err
		}
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
			fmt.Fprintln(os.Stderr, "listening for USB-PD traffic (passive; Ctrl-C to stop)…")
		}
		start := time.Now()
		tr := &pdTracker{showGoodCRC: *goodCRC}
		tick := time.NewTicker(*interval)
		defer tick.Stop()
		n := 0
		finish := func() error {
			sum := tr.summary(nil)
			if jsonOut {
				return emit(newPDEnd(sum, time.Now(), time.Since(start).Seconds()))
			}
			fmt.Fprintf(os.Stderr, "%d messages, %d events\n", sum.Messages, sum.Events)
			if sum.Note != nil {
				fmt.Fprintln(os.Stderr, *sum.Note)
			}
			return nil
		}
		for {
			pd, err := m.ReadPD(ctx)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				return err
			}
			for _, r := range pd.Records {
				rec, ok := tr.process(r)
				if !ok {
					continue
				}
				if jsonOut {
					err = emit(rec)
				} else {
					_, err = fmt.Println(rec.text())
				}
				if err != nil {
					if isBrokenPipe(err) {
						return nil
					}
					return err
				}
				if n++; *count > 0 && n >= *count {
					return finish()
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
			return parent.Err()
		}
		return finish()
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
	Model           string  `json:"model"`
	FirmwareVersion string  `json:"firmware_version"`
	FirmwareDate    string  `json:"firmware_date"`
	HardwareVersion string  `json:"hardware_version"`
	HardwareDate    string  `json:"hardware_date"`
	Serial          string  `json:"serial"`
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

func setupInfo(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(ctx context.Context, g globals) error {
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		res := infoResult{SchemaVersion: schemaVersion, USB: m.usb, Errors: []string{}}
		if info, err := m.ReadInfo(ctx); err != nil {
			res.Errors = append(res.Errors, "device: "+err.Error())
		} else {
			res.Device = &deviceJSON{
				Model: info.Model, FirmwareVersion: info.FirmwareVersion, FirmwareDate: isoDate(info.FirmwareDate),
				HardwareVersion: info.HardwareVersion, HardwareDate: isoDate(info.HardwareDate),
				Serial: info.Serial, UUID: strOrNil(info.UUID), TailLayout: m.Layout().String(),
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
			if err := emitIndented(res); err != nil {
				return err
			}
		} else {
			printInfoText(res)
		}
		if len(res.Errors) > 0 {
			return partialErr{strings.Join(res.Errors, "; ")}
		}
		return nil
	}
}

func printInfoText(r infoResult) {
	st := style{on: colorOn()}
	kv := func(k, v string) {
		if v != "" {
			fmt.Printf("  %-18s %s\n", k, v)
		}
	}
	fmt.Println(st.bold("USB"))
	kv("Manufacturer", r.USB.Manufacturer)
	kv("Product", r.USB.Product)
	kv("USB serial", r.USB.Serial)
	kv("Bus/address", fmt.Sprintf("%d/%d", r.USB.Bus, r.USB.Address))
	if d := r.Device; d != nil {
		fmt.Println(st.bold("Device"))
		kv("Model", d.Model)
		kv("Firmware", d.FirmwareVersion)
		kv("Firmware date", d.FirmwareDate)
		kv("Hardware/boot ver", d.HardwareVersion)
		kv("Hardware date", d.HardwareDate)
		kv("Serial", d.Serial)
		kv("UUID", deref(d.UUID, ""))
	}
	if s := r.Settings; s != nil {
		fmt.Println(st.bold("Settings"))
		kv("Device name", s.DeviceName)
		kv("Sample rate", fmt.Sprintf("%d SPS", s.SampleRateSPS))
		kv("Brightness", fmt.Sprintf("%d %%", s.BrightnessPercent))
		kv("Rotation code", strconv.Itoa(s.RotationCode))
		kv("Auto-rotate", strconv.FormatBool(s.AutoRotate))
		kv("Checksum", map[bool]string{true: "ok", false: "mismatch"}[s.ChecksumOK])
	}
}

// ---------------------------------------------------------------- devices

type devicesResult struct {
	SchemaVersion int                     `json:"schema_version"`
	Count         int                     `json:"count"`
	Devices       []km003c.USBDescription `json:"devices"`
}

func setupDevices(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		devs, err := km003c.ListDevices()
		if err != nil {
			return err
		}
		if g.jsonMode() {
			return emitIndented(devicesResult{SchemaVersion: schemaVersion, Count: len(devs), Devices: devs})
		}
		if len(devs) == 0 {
			fmt.Println("no POWER-Z KM003C connected")
			return nil
		}
		for _, d := range devs {
			fmt.Printf("%s  %s %s  bus %d address %d\n", d.Serial, d.Manufacturer, d.Product, d.Bus, d.Address)
		}
		return nil
	}
}
