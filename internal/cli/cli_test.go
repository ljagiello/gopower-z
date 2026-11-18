package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/ljagiello/gopower-z/internal/fakemeter"
)

// These tests run whole commands against the fake meter
// (internal/fakemeter, replaying firmware-1.9.5 captures) and check the real
// stdout against the schema, the exit status and the stderr error object.

var testUSB = km003c.USBDescription{Manufacturer: "ChargerLab", Product: "POWER-Z KM003C", Serial: "050002", Bus: 2, Address: 20}

type cli struct {
	t          *testing.T
	fm         *fakemeter.Meter
	stdout     bytes.Buffer
	stderr     bytes.Buffer
	opens      int
	opened     bool
	busyOpens  int // the first n opens fail with ErrBusy
	notPresent bool
}

func newCLI(t *testing.T) *cli {
	return &cli{t: t, fm: fakemeter.New(t)}
}

func (c *cli) env() *env {
	return &env{stdout: &c.stdout, stderr: &c.stderr,
		openUSB: func(serial string) (km003c.Transport, km003c.USBDescription, error) {
			c.opens++
			switch {
			case c.notPresent:
				return nil, km003c.USBDescription{}, fmt.Errorf("%w: no POWER-Z KM003C connected", km003c.ErrNotFound)
			case c.busyOpens > 0:
				c.busyOpens--
				return nil, km003c.USBDescription{}, fmt.Errorf("%w (claim interface 0)", km003c.ErrBusy)
			}
			c.opened = true
			return c.fm, testUSB, nil
		},
		listUSB: func() ([]km003c.USBDescription, error) { return []km003c.USBDescription{testUSB}, nil },
	}
}

func (c *cli) run(ctx context.Context, args ...string) int {
	c.t.Helper()
	code := run(ctx, c.env(), append([]string{"--timeout", "100ms"}, args...))
	if c.opened && !c.fm.Closed {
		c.t.Errorf("%v: the meter was not closed", args)
	}
	if c.fm.Hung {
		c.t.Errorf("%v: a request was written while a reply was still queued", args)
	}
	return code
}

// lines decodes stdout as JSON values, one per line.
func (c *cli) lines() []any {
	c.t.Helper()
	var out []any
	for l := range strings.SplitSeq(strings.TrimSpace(c.stdout.String()), "\n") {
		if l == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			c.t.Fatalf("stdout line is not JSON: %q (%v)", l, err)
		}
		out = append(out, v)
	}
	return out
}

func (c *cli) one() map[string]any {
	c.t.Helper()
	l := c.lines()
	if len(l) != 1 {
		c.t.Fatalf("stdout has %d JSON values, want 1: %s", len(l), c.stdout.String())
	}
	return l[0].(map[string]any)
}

// errCode returns the code of the stderr error object ("" if none).
func (c *cli) errCode() string {
	c.t.Helper()
	if c.stderr.Len() == 0 {
		return ""
	}
	var e struct {
		Error cliError `json:"error"`
	}
	if err := json.Unmarshal(c.stderr.Bytes(), &e); err != nil {
		c.t.Fatalf("stderr is not an error object: %q", c.stderr.String())
	}
	var v any
	_ = json.Unmarshal(c.stderr.Bytes(), &v)
	checkShape(c.t, "error", v)
	return e.Error.Code
}

func (c *cli) wrote(t km003c.MsgType) int {
	n := 0
	for _, w := range c.fm.Written {
		if km003c.MsgType(w[0]&0x7F) == t {
			n++
		}
	}
	return n
}

func TestCLIRead(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "read"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	s := c.one()
	checkShape(t, "snapshot", s)
	if s["firmware_version"] != "1.9.5" || s["firmware_error"] != nil || s["raw"].(map[string]any)["tail_layout"] != "line-averages" ||
		s["inferred"].(map[string]any)["dashboard_classifier"] != "1.9.5" || s["sample_rate_sps"] != float64(50) {
		t.Errorf("snapshot %v", s)
	}
	if c.fm.Connected {
		t.Error("session left open: no DISCONNECT on close")
	}
}

// When the firmware version cannot be read the reading is still printed, the
// firmware-dependent fields are null and firmware_error says why.
func TestCLIReadFirmwareUnknown(t *testing.T) {
	c := newCLI(t)
	c.fm.Confirm = func(addr uint32, conf []byte) {
		if addr == fakemeter.AddrFirmware {
			binary.LittleEndian.PutUint32(conf[4:], 0x00004430)
		}
	}
	if code := c.run(context.Background(), "read"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	s := c.one()
	checkShape(t, "snapshot", s)
	inf := s["inferred"].(map[string]any)
	if s["firmware_version"] != nil || s["firmware_error"] == nil || s["sample_rate_sps"] != nil ||
		inf["dashboard_classifier"] != nil || s["raw"].(map[string]any)["tail_layout"] != "unknown" {
		t.Errorf("snapshot %v", s)
	}
}

func TestCLIMeasure(t *testing.T) {
	c := newCLI(t)
	c.fm.PD = [][]byte{negotiation()}
	if code := c.run(context.Background(), "measure", "--duration", "300ms", "--interval", "20ms"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	pd := m["pd"].(map[string]any)
	if m["end_reason"] != "completed" || m["samples"].(float64) < 2 || m["failed_samples"] != float64(0) ||
		pd["contract"] == nil || len(pd["records"].([]any)) != 5 {
		t.Errorf("measure %v", m)
	}
	// The fake meter repeats one reading, so every statistic must equal the
	// last reading's value for its own field: this catches fields wired to
	// the wrong ADC value (D+ for D-, power for current).
	stats, last := m["stats"].(map[string]any), m["last"].(map[string]any)
	for _, f := range []string{"vbus_v", "ibus_a", "power_w", "dp_v", "dm_v"} {
		st := stats[f].(map[string]any)
		if st["mean"] != last[f] || st["min"] != last[f] || st["p95"] != last[f] || st["stdev"] != float64(0) {
			t.Errorf("stats.%s = %v, last.%s = %v", f, st, f, last[f])
		}
	}
	if m["bin_s"] != 0.5 {
		t.Errorf("bin_s %v", m["bin_s"])
	}
}

// adcPayload is the captured ADC reading with the current (amperes,
// positive receptacle -> plug), VBUS (volts) and raw temperature replaced
// (-32768 is the meter's "invalid" marker; 3584 is 28 °C).
func adcPayload(t *testing.T, flowA, vbusV float64, tempRaw int16) []byte {
	t.Helper()
	p := slices.Clone(fakemeter.Frame(t, "bulk_W1_1_resp")[8:])
	binary.LittleEndian.PutUint32(p[0x00:], uint32(int32(math.Round(vbusV*1e6))))
	binary.LittleEndian.PutUint32(p[0x04:], uint32(int32(math.Round(-flowA*1e6)))) // the meter's sign is reversed
	binary.LittleEndian.PutUint16(p[0x18:], uint16(tempRaw))
	return p
}

// A step in the meter's reading shows in the bins, field by field: current,
// voltage and the power computed from them.
func TestCLIMeasureStep(t *testing.T) {
	c := newCLI(t)
	for range 20 {
		c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, 0.134, 5.076, 3584))
	}
	c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, 0.441, 5.076, 3584))
	if code := c.run(context.Background(), "measure", "--duration", "1s", "--interval", "20ms", "--bin", "100ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	bins := m["bins"].([]any)
	first, last := bins[0].(map[string]any), bins[len(bins)-1].(map[string]any)
	for k, b := range bins {
		if ts := b.(map[string]any)["t_s"]; ts != round(float64(k)*0.1, 6) {
			t.Errorf("bin %d starts at %v", k, ts)
		}
	}
	want := func(b map[string]any, field string, v float64) {
		t.Helper()
		r, ok := b[field].(map[string]any)
		if !ok {
			t.Fatalf("bin at %v s has no %s: %v", b["t_s"], field, b)
		}
		if r["min"] != v || r["mean"] != v || r["max"] != v {
			t.Errorf("bin at %v s: %s %v, want %v", b["t_s"], field, r, v)
		}
	}
	want(first, "ibus_a", 0.134)
	want(first, "vbus_v", 5.076)
	want(first, "power_w", 0.680184)
	want(last, "ibus_a", 0.441)
	want(last, "power_w", 2.238516)
	st := m["stats"].(map[string]any)
	if p := st["power_w"].(map[string]any); p["min"] != 0.680184 || p["max"] != 2.238516 || m["direction"] != "receptacle_to_plug" {
		t.Errorf("power stats %v, direction %v", p, m["direction"])
	}
	if tc := st["temp_c"].(map[string]any); tc["min"] != 28.0 || tc["max"] != 28.0 {
		t.Errorf("temp_c %v", tc)
	}
}

// Readings the meter marks as having no valid temperature are left out of
// temp_c (they would read -256 °C).
func TestCLIMeasureInvalidTemperature(t *testing.T) {
	c := newCLI(t)
	for range 5 {
		c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, 0.1, 5, 3584))
	}
	c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, 0.1, 5, -32768))
	if code := c.run(context.Background(), "measure", "--duration", "400ms", "--interval", "20ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	tc := m["stats"].(map[string]any)["temp_c"].(map[string]any)
	if tc["min"] != 28.0 || tc["max"] != 28.0 || tc["last"] != 28.0 || m["last"].(map[string]any)["temp_c"] != nil {
		t.Errorf("temp_c %v, last reading temp_c %v", tc, m["last"].(map[string]any)["temp_c"])
	}
}

// direction describes the mean current of the window: here the median and
// the last reading point the other way.
func TestCLIMeasureDirectionFromMean(t *testing.T) {
	c := newCLI(t)
	for range 2 { // the mean stays negative for up to 41 readings; p50 needs 5
		c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, -1.0, 5.076, 3584))
	}
	c.fm.ADCSeq = append(c.fm.ADCSeq, adcPayload(t, 0.05, 5.076, 3584))
	if code := c.run(context.Background(), "measure", "--duration", "300ms", "--interval", "20ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	ibus := m["stats"].(map[string]any)["ibus_a"].(map[string]any)
	if m["direction"] != "plug_to_receptacle" || ibus["p50"] != 0.05 || ibus["mean"].(float64) >= 0 ||
		m["last"].(map[string]any)["direction"] != "receptacle_to_plug" {
		t.Errorf("direction %v, ibus %v, last direction %v", m["direction"], ibus, m["last"].(map[string]any)["direction"])
	}
}

// Failed samples are counted, the result is printed, and the exit status is
// 7 (partial) with the partial error on stderr.
func TestCLIMeasureFailedSamples(t *testing.T) {
	c := newCLI(t)
	c.fm.DropOnly, c.fm.DropReplies = km003c.TypeGetData, 2 // one sample: both attempts
	code := c.run(context.Background(), "measure", "--duration", "700ms", "--interval", "20ms", "--bin", "100ms", "--pd=false")
	if code != exitPartial || c.errCode() != "partial" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["failed_samples"] != float64(1) || m["last_error"] == nil || m["samples"].(float64) < 1 || m["end_reason"] != "completed" || m["pd"] != nil {
		t.Errorf("measure %v", m)
	}
	// Readings are placed by the time they were taken: the first read
	// failed twice (each attempt waits for a 100 ms timeout), so the first
	// bin is empty and the readings sit in the later ones.
	bins, total := m["bins"].([]any), 0.0
	for _, b := range bins {
		total += b.(map[string]any)["samples"].(float64)
	}
	first, last := bins[0].(map[string]any), bins[len(bins)-1].(map[string]any)
	if first["samples"] != 0.0 || first["ibus_a"] != nil || last["samples"].(float64) == 0 || total != m["samples"] {
		t.Errorf("bins %v (total %v of %v samples)", bins, total, m["samples"])
	}
}

// SIGINT/SIGTERM (an agent harness timeout) still prints what was collected.
func TestCLIMeasureInterrupted(t *testing.T) {
	c := newCLI(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(400*time.Millisecond, cancel)
	code := c.run(ctx, "measure", "--duration", "30s", "--interval", "20ms")
	if code != exitInterrupt || c.errCode() != "interrupted" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["end_reason"] != "interrupted" || m["samples"].(float64) < 1 || m["duration_s"].(float64) > 5 {
		t.Errorf("measure %v", m)
	}
	// The bins cover the time actually measured, not the requested 30 s.
	bins := m["bins"].([]any)
	if lastBin := bins[len(bins)-1].(map[string]any); lastBin["t_s"].(float64) >= m["duration_s"].(float64) {
		t.Errorf("%d bins, the last starting at %v s, for a %v s window", len(bins), lastBin["t_s"], m["duration_s"])
	}
	if c.fm.Connected {
		t.Error("session left open after an interrupt")
	}
}

// An interrupt that arrives while a read is waiting for a meter that stopped
// answering still prints what was collected.
func TestCLIMeasureInterruptedDuringRead(t *testing.T) {
	c := newCLI(t)
	c.fm.SilentAfter = 6 // CONNECT, MemoryRead, a few samples, then nothing
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(600*time.Millisecond, cancel)
	code := run(ctx, c.env(), []string{"--timeout", "1s", "measure", "--duration", "30s", "--interval", "20ms"})
	if code != exitInterrupt || c.errCode() != "interrupted" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["end_reason"] != "interrupted" || m["samples"].(float64) < 1 {
		t.Errorf("measure %v", m)
	}
}

// A meter that disappears mid-run ends the window early; the result is
// printed and the exit status is that of the error.
func TestCLIMeasureMeterGone(t *testing.T) {
	c := newCLI(t)
	c.fm.GoneAfter = 8 // CONNECT, MemoryRead, a few samples
	c.fm.ErrGone = fmt.Errorf("%w (bulk write: no device)", km003c.ErrNotFound)
	code := c.run(context.Background(), "measure", "--duration", "10s", "--interval", "20ms")
	if code != exitNotFound || c.errCode() != "not_found" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["end_reason"] != "error" || m["last_error"] == nil || m["samples"].(float64) < 1 {
		t.Errorf("measure %v", m)
	}
}

func TestCLIMeasureNoSamples(t *testing.T) {
	c := newCLI(t)
	c.fm.DropOnly, c.fm.DropReplies = km003c.TypeGetData, 1000
	code := c.run(context.Background(), "measure", "--duration", "250ms", "--interval", "20ms")
	if code != exitTimeout || c.errCode() != "timeout" || c.stdout.Len() != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, c.stdout.String(), c.stderr.String())
	}
}

// measure's bins follow --bin, the automatic width follows --interval, and
// impossible requests are usage errors before the meter is opened.
func TestCLIMeasureBins(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "measure", "--duration", "400ms", "--interval", "20ms", "--bin", "100ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	// A completed window ends a little after --duration; that must not add
	// a near-empty extra bin.
	bins := m["bins"].([]any)
	if m["bin_s"] != 0.1 || len(bins) != 4 {
		t.Fatalf("bin_s %v, bins %v", m["bin_s"], bins)
	}
	if last := bins[3].(map[string]any); last["samples"].(float64) < 2 {
		t.Errorf("last bin %v", last)
	}
	// Automatic bins span at least 5 polling intervals: 1 s at 120 ms
	// (0.5 s would be the width at the default interval).
	c = newCLI(t)
	if code := c.run(context.Background(), "measure", "--duration", "300ms", "--interval", "120ms", "--pd=false"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	if m := c.one(); m["bin_s"] != 1.0 {
		t.Errorf("bin_s %v at a 120 ms interval, want 1", m["bin_s"])
	}
	for _, args := range [][]string{
		{"--bin", "-1s"},
		{"--bin", "10ms", "--interval", "20ms"},
		{"--bin", "20ms", "--interval", "20ms", "--duration", "1m"},
		{"--duration", "28h", "--interval", "100ms"},
	} {
		// If a limit stopped being checked, measure would run for the full
		// --duration; the deadline makes that a quick failure, not a hang.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c := newCLI(t)
		code := c.run(ctx, append([]string{"measure"}, args...)...)
		cancel()
		if code != exitUsage || c.errCode() != "usage" || c.opens != 0 {
			t.Errorf("%v: exit %d, opens %d, stderr %s", args, code, c.opens, c.stderr.String())
		}
	}
}

// The automatic width comes from the time actually measured: a 2 min window
// stopped after about a second gets 0.5 s bins, not the 1 s bins of 2 min.
func TestCLIMeasureAutoBinFromMeasuredTime(t *testing.T) {
	c := newCLI(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1200*time.Millisecond, cancel)
	if code := c.run(ctx, "measure", "--duration", "2m", "--interval", "20ms", "--pd=false"); code != exitInterrupt {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["bin_s"] != 0.5 || len(m["bins"].([]any)) < 2 {
		t.Errorf("bin_s %v, %d bins", m["bin_s"], len(m["bins"].([]any)))
	}
}

func TestMeasureBinAndReadingLimits(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		duration, interval, bin time.Duration
		ok                      bool
	}{
		{20 * time.Second, 20 * ms, 20 * ms, true},     // exactly 1000 bins, bin == interval
		{20*time.Second + ms, 20 * ms, 20 * ms, false}, // 1001 bins
		{time.Second, 20 * ms, 19 * ms, false},         // shorter than the interval
		{time.Second, 20 * ms, 0, true},                // automatic: chosen when the window ends
		{time.Second, 20 * ms, -ms, false},             // negative is shorter than any interval
		{time.Second, 20 * ms, time.Second, true},      // one bin
	} {
		if err := checkBin(c.duration, c.interval, c.bin); (err == nil) != c.ok {
			t.Errorf("checkBin(%v, %v, %v) = %v", c.duration, c.interval, c.bin, err)
		}
	}
	// 1 000 000 readings is the most a window may hold: 99 999.9 s at 100 ms
	// is 999 999 intervals plus the first reading.
	if n, err := measureReadings(99999900*ms, 100*ms); err != nil || n != maxSamples {
		t.Errorf("at the limit: %d, %v", n, err)
	}
	if _, err := measureReadings(100000*time.Second, 100*ms); err == nil {
		t.Error("one reading over the limit accepted")
	}
	if n, _ := measureReadings(300*ms, 20*ms); n != 16 {
		t.Errorf("300 ms at 20 ms: %d readings, want 16", n)
	}
	if n, _ := measureReadings(310*ms, 20*ms); n != 17 { // a partial interval still holds a reading
		t.Errorf("310 ms at 20 ms: %d readings, want 17", n)
	}
}

// The text layout, line by line, from a hand-built result with round
// values: units and scaling of every row, D+ and D- each from their own
// stats, and an empty bin as a gap in the sparkline.
func TestMeasureText(t *testing.T) {
	r := measureResult{Serial: "050002", DurationS: 60, Samples: 600, FailedSamples: 2, Direction: "receptacle_to_plug",
		EndReason: "completed", EnergyWh: 0.0211, ChargeMAh: 4.1667, IntegratedS: 59.9,
		Stats: measureStats{
			VBusV:  &statJSON{Min: 5.07, Mean: 5.076, Max: 5.08, Last: 5.076, Stdev: 0.002, P5: 5.072, P50: 5.076, P95: 5.079},
			IBusA:  &statJSON{Min: 0.12, Mean: 0.25, Max: 0.45, Last: 0.44, Stdev: 0.15, P5: 0.13, P50: 0.2, P95: 0.44},
			PowerW: &statJSON{Min: -0.00003, Mean: 1.27, Max: 2.29, Last: 2.23, Stdev: 0.76, P5: 0.66, P50: 1.01, P95: 2.23},
			TempC:  &statJSON{Min: 27.9, Mean: 28, Max: 28.1, Last: 28, Stdev: 0.05, P5: 27.9, P50: 28, P95: 28.1},
			DPV:    &statJSON{Min: 0.01, Max: 3},
			DMV:    &statJSON{Min: 0.002, Max: 0.004},
		},
		BinS: 20, Bins: []binJSON{
			{TS: 0, Samples: 200, IBusA: &rangeJSON{Mean: 0.13}},
			{TS: 20},
			{TS: 40, Samples: 200, IBusA: &rangeJSON{Mean: 0.44}},
		},
	}
	var b strings.Builder
	printMeasureText(&b, style{}, r)
	want := strings.Join([]string{
		"POWER-Z measure  050002  60.0s  600 samples  2 failed  flow receptacle_to_plug  (completed)",
		"  VBUS  mean 5.0760 V   min 5.0700  p5 5.0720  p50 5.0760  p95 5.0790  max 5.0800  sd 0.0020",
		"  IBUS  mean 250.000 mA   min 120.000  p5 130.000  p50 200.000  p95 440.000  max 450.000  sd 150.000",
		"  PWR   mean 1.2700 W   min 0.0000  p5 0.6600  p50 1.0100  p95 2.2300  max 2.2900  sd 0.7600",
		"  TEMP  mean 28.00 °C   min 27.90  p5 27.90  p50 28.00  p95 28.10  max 28.10  sd 0.05",
		"  D+ 0.010..3.000 V   D- 0.002..0.004 V",
		"  energy 0.021100 Wh   charge 4.1667 mAh   over 59.900 s",
		"  IBUS per 20s  ▁ █",
		"",
	}, "\n")
	if got := b.String(); got != want {
		t.Errorf("text output:\n%s\nwant:\n%s", got, want)
	}
	// Levels are listed only when the current changed, at most 10 of them.
	lv := func(start, end, p50 float64) levelJSON {
		return levelJSON{StartS: start, EndS: end, IBusA: &statJSON{P50: p50}}
	}
	text := func(r measureResult) string {
		var b strings.Builder
		printMeasureText(&b, style{}, r)
		return b.String()
	}
	one := r
	one.Levels = []levelJSON{lv(0, 60, 0.25)}
	if s := text(one); strings.Contains(s, "levels") {
		t.Errorf("one level is not listed:\n%s", s)
	}
	two := r
	two.Levels = []levelJSON{lv(0, 42.3, 0.134), lv(42.3, 60, 0.441)}
	if s := text(two); !strings.HasSuffix(s, "  2 levels (typical current)\n         0.0 .. 42.3     s  134.000 mA\n        42.3 .. 60.0     s  441.000 mA\n") {
		t.Errorf("two levels:\n%s", s)
	}
	three := r
	three.Levels = []levelJSON{lv(0, 16, 0.065484), lv(16, 23, 0.0614625), lv(23, 60, -0.0000001)}
	wantLevels := "  IBUS per 20s  ▁ █\n" +
		"  3 levels (typical current)\n" +
		"         0.0 .. 16.0     s  65.484 mA\n" +
		"        16.0 .. 23.0     s  61.463 mA\n" +
		"        23.0 .. 60.0     s  0.000 mA\n"
	if s := text(three); !strings.HasSuffix(s, wantLevels) {
		t.Errorf("levels:\n%s\nwant the end:\n%s", s, wantLevels)
	}
	dozen := r
	for k := range 12 {
		dozen.Levels = append(dozen.Levels, lv(float64(k*5), float64(k*5+5), 0.1*float64(k%2+1)))
	}
	if s := text(dozen); strings.Count(s, " mA\n") != 10 || !strings.HasSuffix(s, "    ... 2 more (all in the JSON output)\n") {
		t.Errorf("12 levels:\n%s", s)
	}
	spark := func(r measureResult) string {
		var b strings.Builder
		printMeasureText(&b, style{}, r)
		for line := range strings.SplitSeq(b.String(), "\n") {
			if strings.HasPrefix(line, "  IBUS per ") {
				return line
			}
		}
		return ""
	}
	// The label is rounded: 3 x 0.1 s is not printed as 0.30000000000000004.
	var tenths measureResult
	tenths.BinS = 0.1
	for k := range 180 { // 60 columns of 3 bins
		tenths.Bins = append(tenths.Bins, binJSON{TS: float64(k) / 10, Samples: 5, IBusA: &rangeJSON{Mean: float64(k)}})
	}
	if line := spark(tenths); !strings.HasPrefix(line, "  IBUS per 0.3s  ") {
		t.Errorf("label %q", line)
	}
	// A leading empty bin (the first reads failed) is a gap, and does not
	// flatten the rest of the line.
	var lead measureResult
	lead.BinS = 20
	lead.Bins = []binJSON{{TS: 0}, {TS: 20, Samples: 200, IBusA: &rangeJSON{Mean: 0.13}}, {TS: 40, Samples: 200, IBusA: &rangeJSON{Mean: 0.44}}}
	if line := spark(lead); line != "  IBUS per 20s   ▁█" {
		t.Errorf("leading gap: %q", line)
	}
	// 120 bins become 60 columns of 2, each the sample-weighted mean of its
	// pair, so the whole window shows: 0 in the first half, 8 in the second,
	// and one pair (1 reading at 0, 99 at 8) near the top.
	var pairs measureResult
	pairs.BinS = 1
	for k := range 120 {
		b := binJSON{TS: float64(k), Samples: 10, IBusA: &rangeJSON{Mean: 0}}
		if k >= 60 {
			b.IBusA.Mean = 8
		}
		if k == 58 {
			b.Samples = 1
		}
		if k == 59 {
			b.Samples, b.IBusA.Mean = 99, 8
		}
		pairs.Bins = append(pairs.Bins, b)
	}
	cols := []rune(strings.TrimPrefix(spark(pairs), "  IBUS per 2s  "))
	if len(cols) != 60 || cols[0] != '▁' || cols[28] != '▁' || cols[29] != '▇' || cols[30] != '█' || cols[59] != '█' {
		t.Errorf("grouped sparkline %q", string(cols))
	}
	// Many bins are averaged down to the sparkline width, not cut.
	var many measureResult
	many.BinS = 1
	for k := range 1000 {
		many.Bins = append(many.Bins, binJSON{TS: float64(k), Samples: 10, IBusA: &rangeJSON{Mean: float64(k)}})
	}
	b.Reset()
	printMeasureText(&b, style{}, many)
	if !strings.Contains(b.String(), "  IBUS per 17s  ▁") || !strings.Contains(b.String(), "█\n") {
		t.Errorf("1000 bins:\n%s", b.String())
	}
}

func TestCLIPD(t *testing.T) {
	c := newCLI(t)
	c.fm.PD = [][]byte{
		negotiation(),
		pdPayload(pdMsg(10_000, 5, hdrAccept), []byte{0x81, 0xFF}), // reserved SOP, trailing bytes
	}
	if code := c.run(context.Background(), "pd", "--duration", "300ms"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	lines := c.lines()
	if len(lines) != 8 {
		t.Fatalf("%d lines, want 7 records + summary:\n%s", len(lines), c.stdout.String())
	}
	for _, l := range lines[:len(lines)-1] {
		checkShape(t, "pd_record", l)
	}
	end := lines[len(lines)-1].(map[string]any)
	checkShape(t, "pd_end", end)
	if end["end_reason"] != "completed" || end["contract"] == nil || end["undecoded"] != float64(1) || end["messages"] != float64(5) {
		t.Errorf("summary %v", end)
	}
	// pd needs no session: only GET_DATA for the PD buffer goes out.
	if n := c.wrote(km003c.TypeConnect) + c.wrote(km003c.TypeMemoryRead) + c.wrote(km003c.TypeDisconnect); n != 0 {
		t.Errorf("pd sent %d session/memory frames", n)
	}
	for _, w := range c.fm.Written {
		if h, _ := km003c.ParseHeader(w); km003c.Attribute(h.Word>>1) != km003c.AttPDPacket {
			t.Errorf("pd sent % X", w)
		}
	}
}

// A meter that disappears during pd ends the capture with the summary line
// (end_reason error) and the error's exit status.
func TestCLIPDMeterGone(t *testing.T) {
	c := newCLI(t)
	c.fm.PD = [][]byte{negotiation()}
	c.fm.GoneAfter = 3
	c.fm.ErrGone = fmt.Errorf("%w (bulk write: no device)", km003c.ErrNotFound)
	code := c.run(context.Background(), "pd", "--duration", "10s")
	if code != exitNotFound || c.errCode() != "not_found" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	lines := c.lines()
	end := lines[len(lines)-1].(map[string]any)
	checkShape(t, "pd_end", end)
	if end["end_reason"] != "error" || end["contract"] == nil || len(lines) != 6 {
		t.Errorf("%d lines, summary %v", len(lines), end)
	}
}

func TestCLIPDInterrupted(t *testing.T) {
	c := newCLI(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	code := c.run(ctx, "pd", "--duration", "30s")
	if code != exitInterrupt || c.errCode() != "interrupted" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	lines := c.lines()
	end := lines[len(lines)-1].(map[string]any)
	checkShape(t, "pd_end", end)
	if end["end_reason"] != "interrupted" {
		t.Errorf("summary %v", end)
	}
}

func TestCLIPDCount(t *testing.T) {
	c := newCLI(t)
	c.fm.PD = [][]byte{negotiation()}
	if code := c.run(context.Background(), "pd", "--count", "2", "--duration", "5s"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	if lines := c.lines(); len(lines) != 3 || lines[2].(map[string]any)["end_reason"] != "completed" {
		t.Fatalf("lines %v", lines)
	}
}

func TestCLILog(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "log", "--count", "3", "--interval", "20ms"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	lines := c.lines()
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3", len(lines))
	}
	for _, l := range lines {
		checkShape(t, "snapshot", l)
	}
	c2 := newCLI(t)
	if code := c2.run(context.Background(), "log", "--count", "2", "--interval", "20ms", "--format", "csv"); code != exitOK {
		t.Fatalf("csv exit %d", code)
	}
	rows := strings.Split(strings.TrimSpace(c2.stdout.String()), "\n")
	if len(rows) != 3 || rows[0] != strings.Join(csvColumns, ",") {
		t.Fatalf("csv %q", c2.stdout.String())
	}
}

func TestCLIInfo(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "info"); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	i := c.one()
	checkShape(t, "info", i)
	d := i["device"].(map[string]any)
	if d["firmware_version"] != "1.9.5" || d["serial"] != "050002" || d["firmware_date"] != "2025-01-07" ||
		d["adc_tail_layout"] != "line-averages" || len(i["errors"].([]any)) != 0 {
		t.Errorf("info %v", i)
	}
	if n := c.wrote(km003c.TypeMemoryRead); n != 3 {
		t.Errorf("%d MemoryReads, want 3 (no separate firmware detection)", n)
	}
}

// One unreadable block (the OTP redirect of docs/firmware.html §11) keeps the
// rest of the device information; exit 7.
func TestCLIInfoPartial(t *testing.T) {
	c := newCLI(t)
	c.fm.Confirm = func(addr uint32, conf []byte) {
		if addr == fakemeter.AddrOTP {
			binary.LittleEndian.PutUint32(conf[4:], 0x03000D80)
		}
	}
	if code := c.run(context.Background(), "info"); code != exitPartial || c.errCode() != "partial" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	i := c.one()
	checkShape(t, "info", i)
	d := i["device"].(map[string]any)
	errs := i["errors"].([]any)
	if d["model"] != "KM003C" || d["firmware_version"] != "1.9.5" || d["serial"] != nil || d["uuid"] != nil ||
		len(errs) != 1 || !strings.Contains(errs[0].(string), "0x03000C00") || i["settings"] == nil {
		t.Errorf("info %v", i)
	}
}

func TestCLIWaitForBusyMeter(t *testing.T) {
	c := newCLI(t)
	c.busyOpens = 2
	if code := c.run(context.Background(), "read", "--wait", "3s"); code != exitOK || c.opens != 3 {
		t.Fatalf("exit %d after %d opens, stderr %s", code, c.opens, c.stderr.String())
	}
	checkShape(t, "snapshot", c.one())

	busy := newCLI(t)
	busy.busyOpens = 1
	if code := busy.run(context.Background(), "read"); code != exitBusy || busy.errCode() != "busy" || busy.opens != 1 {
		t.Fatalf("without -wait: exit %d after %d opens", code, busy.opens)
	}
}

func TestCLINotFound(t *testing.T) {
	c := newCLI(t)
	c.notPresent = true
	if code := c.run(context.Background(), "read"); code != exitNotFound || c.errCode() != "not_found" || c.stdout.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, c.stdout.String())
	}
}

func TestCLIDevicesSchemaVersion(t *testing.T) {
	c := newCLI(t)
	if code := c.run(context.Background(), "devices"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	checkShape(t, "devices", c.one())
	if c.opens != 0 {
		t.Error("devices opened the meter")
	}
	v := newCLI(t)
	if code := v.run(context.Background(), "version"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	checkShape(t, "version", v.one())
	s := newCLI(t)
	if code := s.run(context.Background(), "schema"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if s.one()["schema_version"] != float64(schemaVersion) {
		t.Error("schema without schema_version")
	}
}
