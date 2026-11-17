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

func printSnapshotText(s snapshot, a km003c.ADC) {
	st := style{on: colorOn()}
	row := func(label, val, extra string) {
		fmt.Printf("  %-6s %s  %s\n", label, val, st.dim(extra))
	}
	fmt.Println(st.bold("POWER-Z KM003C") + st.dim("  "+s.Serial+"  "+s.Time.Format(time.RFC3339)))
	row("VBUS", st.green(grouped(a.VBus())+" V"), fmt.Sprintf("meter avg %.6f V", s.VBusAvgV))
	row("IBUS", st.pink(grouped(a.IBus())+" A"), fmt.Sprintf("meter avg %.6f A", s.IBusAvgA))
	row("PWR", st.blue(grouped(a.Power())+" W"), fmt.Sprintf("meter avg %.6f W", s.PowerAvgW))
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
	fmt.Println("  " + st.dim("label: ") + s.Inferred.DashboardLabel + " / " + s.Inferred.DashboardLabel2 + st.dim("  (meter's D+/D- dashboard labels, recomputed)"))
	fmt.Println("  " + st.dim("CC:    ") + s.Inferred.CC + st.dim("  (inferred)"))
	if s.FirmwareVersion != "" {
		fmt.Println("  " + st.dim("firmware "+s.FirmwareVersion))
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
	SchemaVersion int        `json:"schema_version"`
	Serial        string     `json:"serial"`
	Start         time.Time  `json:"start"`
	End           time.Time  `json:"end"`
	DurationS     float64    `json:"duration_s"`
	IntervalS     float64    `json:"interval_s"`
	Samples       int        `json:"samples"`
	Errors        int        `json:"errors"`
	LastError     string     `json:"last_error,omitempty"`
	VBusV         *statJSON  `json:"vbus_v"`
	IBusA         *statJSON  `json:"ibus_a"`
	PowerW        *statJSON  `json:"power_w"`
	TempC         *statJSON  `json:"temp_c"`
	EnergyWh      float64    `json:"energy_wh"`
	ChargeMAh     float64    `json:"charge_mah"`
	Last          *snapshot  `json:"last"`
	PD            *pdSummary `json:"pd,omitempty"`
}

func setupMeasure(fs *flag.FlagSet) func(context.Context, globals) error {
	duration := fs.Duration("duration", 5*time.Second, "how long to sample")
	interval := fs.Duration("interval", 100*time.Millisecond, "sampling interval (min 20ms; the meter's own sample rate bounds how often values change)")
	withPD := fs.Bool("pd", true, "also capture USB-PD traffic (source capabilities, negotiated contract)")
	records := fs.Bool("pd-records", false, "include every captured PD record in the output")
	return func(ctx context.Context, g globals) error {
		if *duration <= 0 {
			return usageErr("measure: -duration must be positive")
		}
		*interval = max(*interval, 20*time.Millisecond)
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()

		var v, i, p, temp stat
		var wh, ah float64
		var last *snapshot
		var lastErr error
		var lastT time.Time
		tr := &pdTracker{}
		var recs []pdRecord
		res := measureResult{SchemaVersion: schemaVersion, Serial: m.usb.Serial, Start: time.Now(), IntervalS: interval.Seconds()}

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
				res.Errors++
				res.LastError = err.Error()
				lastErr = err
				if errors.Is(err, km003c.ErrNotFound) || errors.Is(err, km003c.ErrBusy) || errors.Is(err, km003c.ErrStuck) {
					return err
				}
			default:
				if !lastT.IsZero() {
					dt := now.Sub(lastT).Hours()
					wh += a.Power() * dt
					ah += a.IBus() * dt
				}
				lastT = now
				v.add(a.VBus())
				i.add(a.IBus())
				p.add(a.Power())
				if a.TempValid() {
					temp.add(a.TempC())
				}
				s := toSnapshot(a, now, m.usb.Serial, m.FirmwareVersion())
				last = &s
				res.Samples++
				if pd != nil {
					for _, r := range pd.Records {
						if rec, ok := tr.process(r); ok && *records {
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
		res.End = time.Now()
		res.DurationS = round(res.End.Sub(res.Start).Seconds(), 3)
		res.VBusV, res.IBusA, res.PowerW, res.TempC = v.json(6), i.json(6), p.json(6), temp.json(3)
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
			return fmt.Errorf("measure: no samples collected (%d errors): %w", res.Errors, lastErr)
		}
		if g.jsonMode() {
			if err := emitIndented(res); err != nil {
				return err
			}
		} else {
			printMeasureText(res)
		}
		if res.Errors > 0 {
			return partialErr{fmt.Sprintf("%d of %d samples failed (last: %s)", res.Errors, res.Samples+res.Errors, res.LastError)}
		}
		return nil
	}
}

func printMeasureText(r measureResult) {
	st := style{on: colorOn()}
	fmt.Printf("%s  %s  %.1fs  %d samples  %d errors\n", st.bold("POWER-Z measure"), r.Serial, r.DurationS, r.Samples, r.Errors)
	row := func(name, unit string, s *statJSON) {
		if s == nil {
			return
		}
		fmt.Printf("  %-5s min %12.6f  mean %12.6f  max %12.6f  last %12.6f %s\n", name, s.Min, s.Mean, s.Max, s.Last, unit)
	}
	row("VBUS", "V", r.VBusV)
	row("IBUS", "A", r.IBusA)
	row("PWR", "W", r.PowerW)
	row("TEMP", "°C", r.TempC)
	fmt.Printf("  energy %.6f Wh   charge %.4f mAh\n", r.EnergyWh, r.ChargeMAh)
	if r.PD != nil {
		fmt.Printf("  PD: %d messages, %d events\n", r.PD.Messages, r.PD.Events)
		for _, p := range r.PD.SourceCapabilities {
			fmt.Printf("    #%d %s\n", p.Index, p.Text)
		}
		if r.PD.Contract != nil {
			fmt.Printf("  contract: %s\n", r.PD.Contract.Text)
		} else if r.PD.LastRequest != nil {
			fmt.Printf("  last request: %s\n", r.PD.LastRequest.Text)
		}
	}
}

// ---------------------------------------------------------------- log

func setupLog(fs *flag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 200*time.Millisecond, "sampling interval (min 20ms)")
	format := fs.String("format", "", "jsonl or csv (default: jsonl in JSON mode, csv otherwise)")
	count := fs.Int("n", 0, "stop after n samples (0 = no limit)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; agents should always set -duration or -n)")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("log: -n and -duration must not be negative")
		}
		*interval = max(*interval, 20*time.Millisecond)
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
			cols := []string{"time", "vbus_v", "ibus_a", "power_w", "vbus_avg_v", "ibus_avg_a",
				"temp_c", "cc1_v", "cc2_v", "dp_v", "dm_v", "vdd_v"}
			if err := w.Write(cols); err != nil {
				return err
			}
			ff := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
			write = func(s snapshot) error {
				temp := ""
				if s.TempC != nil {
					temp = ff(*s.TempC)
				}
				_ = w.Write([]string{s.Time.Format(time.RFC3339Nano), ff(s.VBusV), ff(s.IBusA), ff(s.PowerW),
					ff(s.VBusAvgV), ff(s.IBusAvgA), temp, ff(s.CC1V), ff(s.CC2V), ff(s.DPV), ff(s.DMV), ff(s.VDDV)})
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

func setupPD(fs *flag.FlagSet) func(context.Context, globals) error {
	interval := fs.Duration("interval", 25*time.Millisecond, "polling interval (the meter buffers PD traffic between polls)")
	duration := fs.Duration("duration", 0, "stop after this long (0 = no limit; agents should always set -duration or -n)")
	count := fs.Int("n", 0, "stop after n records (0 = no limit; waits indefinitely without PD traffic, so combine with -duration)")
	goodCRC := fs.Bool("goodcrc", false, "include GoodCRC acknowledgements")
	return func(ctx context.Context, g globals) error {
		if *count < 0 || *duration < 0 {
			return usageErr("pd: -n and -duration must not be negative")
		}
		*interval = max(*interval, 10*time.Millisecond)
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
		jsonOut := g.jsonMode()
		if !jsonOut {
			fmt.Fprintln(os.Stderr, "listening for USB-PD traffic (passive; Ctrl-C to stop)…")
		}
		tr := &pdTracker{showGoodCRC: *goodCRC}
		tick := time.NewTicker(*interval)
		defer tick.Stop()
		n := 0
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
					return nil
				}
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

// ---------------------------------------------------------------- info

type infoResult struct {
	SchemaVersion int                   `json:"schema_version"`
	USB           km003c.USBDescription `json:"usb"`
	Device        *deviceJSON           `json:"device"`
	Settings      *settingsJSON         `json:"settings"`
	Errors        []string              `json:"errors,omitempty"`
}

type deviceJSON struct {
	Model           string `json:"model"`
	FirmwareVersion string `json:"firmware_version"`
	FirmwareDate    string `json:"firmware_date"`
	HardwareVersion string `json:"hardware_version"`
	HardwareDate    string `json:"hardware_date"`
	Serial          string `json:"serial"`
	UUID            string `json:"uuid,omitempty"`
}

type settingsJSON struct {
	DeviceName        string `json:"device_name"`
	SampleRateSPS     int    `json:"sample_rate_sps"`
	BrightnessPercent int    `json:"brightness_percent"`
	Rotation          int    `json:"rotation"`
	AutoRotate        bool   `json:"auto_rotate"`
	Language          int    `json:"language"`
	Theme             int    `json:"theme"`
	ChecksumOK        bool   `json:"checksum_ok"`
}

func setupInfo(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(ctx context.Context, g globals) error {
		m, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer m.close()
		res := infoResult{SchemaVersion: schemaVersion, USB: m.usb}
		if info, err := m.ReadInfo(ctx); err != nil {
			res.Errors = append(res.Errors, "device: "+err.Error())
		} else {
			res.Device = &deviceJSON{
				Model: info.Model, FirmwareVersion: info.FirmwareVersion, FirmwareDate: info.FirmwareDate,
				HardwareVersion: info.HardwareVersion, HardwareDate: info.HardwareDate,
				Serial: info.Serial, UUID: info.UUID,
			}
		}
		if set, err := m.ReadSettings(ctx); err != nil {
			res.Errors = append(res.Errors, "settings: "+err.Error())
		} else {
			res.Settings = &settingsJSON{
				DeviceName: set.DeviceName, SampleRateSPS: set.SampleRate.SamplesPerSecond(),
				BrightnessPercent: set.Brightness, Rotation: set.Rotation, AutoRotate: set.AutoRotate,
				Language: set.Language, Theme: set.Theme, ChecksumOK: set.CRCAOK && set.CRCBOK,
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
		kv("UUID", d.UUID)
	}
	if s := r.Settings; s != nil {
		fmt.Println(st.bold("Settings"))
		kv("Device name", s.DeviceName)
		kv("Sample rate", fmt.Sprintf("%d SPS", s.SampleRateSPS))
		kv("Brightness", fmt.Sprintf("%d %%", s.BrightnessPercent))
		kv("Rotation", strconv.Itoa(s.Rotation))
		kv("Auto-rotate", strconv.FormatBool(s.AutoRotate))
		kv("Checksum", map[bool]string{true: "ok", false: "mismatch"}[s.ChecksumOK])
	}
}

// ---------------------------------------------------------------- devices

func setupDevices(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		devs, err := km003c.ListDevices()
		if err != nil {
			return err
		}
		if g.jsonMode() {
			return emitIndented(map[string]any{"schema_version": schemaVersion, "count": len(devs), "devices": devs})
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
