package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
}

// Failed samples are counted, the result is printed, and the exit status is
// 7 (partial) with the partial error on stderr.
func TestCLIMeasureFailedSamples(t *testing.T) {
	c := newCLI(t)
	c.fm.DropOnly, c.fm.DropReplies = km003c.TypeGetData, 2 // one sample: both attempts
	code := c.run(context.Background(), "measure", "--duration", "700ms", "--interval", "20ms", "--pd=false")
	if code != exitPartial || c.errCode() != "partial" {
		t.Fatalf("exit %d, stderr %s", code, c.stderr.String())
	}
	m := c.one()
	checkShape(t, "measure", m)
	if m["failed_samples"] != float64(1) || m["last_error"] == nil || m["samples"].(float64) < 1 || m["end_reason"] != "completed" || m["pd"] != nil {
		t.Errorf("measure %v", m)
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
