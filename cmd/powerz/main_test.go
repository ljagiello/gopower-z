package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err       error
		code      string
		exit      int
		retryable bool
	}{
		{fmt.Errorf("x: %w", km003c.ErrNotFound), "not_found", exitNotFound, false},
		{fmt.Errorf("x: %w", km003c.ErrBusy), "busy", exitBusy, true},
		{fmt.Errorf("x: %w", km003c.ErrTimeout), "timeout", exitTimeout, true},
		{fmt.Errorf("x: %w", km003c.ErrPermission), "permission_denied", exitPermission, false},
		{context.Canceled, "interrupted", exitInterrupt, false},
		{partialErr{"half"}, "partial", exitPartial, true},
		{usageErr("bad"), "usage", exitUsage, false},
		{errors.New("boom"), "error", exitError, false},
	}
	for _, c := range cases {
		ce := classify(c.err)
		if ce.Code != c.code || ce.exit != c.exit || ce.Retryable != c.retryable || ce.Hint == "" {
			t.Errorf("classify(%v) = %+v, want %s/%d/%v with a hint", c.err, ce, c.code, c.exit, c.retryable)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"},
		{"read", "-bogus"},
		{"read", "extra-arg"},
		{"measure", "-duration", "0s"},
		{"measure", "-interval", "5ms"},
		{"log", "-format", "xml"},
		{"log", "-n", "-1"},
		{"pd", "-interval", "0s"},
		{"read", "-timeout", "50ms"},
		{"measure", "-duration", "banana"},
	} {
		if got := run(append([]string{"-json"}, args...)); got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, got, exitUsage)
		}
	}
}

func TestUsageHints(t *testing.T) {
	if got := nearestCommand("mesure"); got != "measure" {
		t.Errorf("nearestCommand(mesure) = %q", got)
	}
	if got := nearestCommand("zzzzzzzz"); got != "" {
		t.Errorf("nearestCommand(zzzzzzzz) = %q", got)
	}
}

// Flag help must not contain backquotes: Go's flag package turns
// `quoted` text into the argument name shown by -h.
func TestFlagUsageHasNoBackquotes(t *testing.T) {
	for name, c := range commands {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		var g globals
		addCommon(fs, &g)
		c.setup(fs)
		fs.VisitAll(func(f *flag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s -%s usage contains a backquote: %q", name, f.Name, f.Usage)
			}
		})
	}
}

func loadADC(t *testing.T) km003c.ADC {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "km003c", "testdata", "fw1.9.5", "bulk_W1_1_resp.hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatal(err)
	}
	a, err := km003c.ParseADCFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSnapshotValues(t *testing.T) {
	a := loadADC(t)
	a.ApplyTail(km003c.TailLayoutFor("1.9.5"))
	s := toSnapshot(a, time.Unix(0, 0), "050002", "1.9.5")
	if s.VBusV < 0.004 || s.VBusV > 0.007 || s.TempC == nil || s.Temp2C != nil {
		t.Errorf("values: %+v", s)
	}
	if s.SampleRateSPS == nil || *s.SampleRateSPS != 50 || s.CC2AvgV == nil || s.Raw.TailLayout != "line-averages" {
		t.Errorf("firmware-dependent fields: %+v", s)
	}
	if s.Inferred.DashboardLabel != nil { // "[ N/A ]" on screen -> null
		t.Errorf("dashboard label %q, want null", *s.Inferred.DashboardLabel)
	}
	if s.Time != "1970-01-01T00:00:00.000Z" {
		t.Errorf("time %q, want UTC with milliseconds", s.Time)
	}
	if s.Direction != "none" || s.Inferred.CC.Attached != "none" {
		t.Errorf("idle direction/cc: %s %+v", s.Direction, s.Inferred.CC)
	}
}

func TestIntegrator(t *testing.T) {
	var in integrator
	t0 := time.Unix(0, 0)
	for i := range 10 { // 2 W for 10 samples, 100 ms apart
		in.add(t0.Add(time.Duration(i)*100*time.Millisecond), 2, 0.4)
	}
	wh, ah, s := in.finish(t0.Add(time.Second))
	if s != 1 || wh != 2.0/3600 || ah != 0.4/3600 {
		t.Errorf("integrated %v s, %v Wh, %v Ah; want 1 s, %v Wh", s, wh, ah, 2.0/3600)
	}
}

func TestISODate(t *testing.T) {
	for in, want := range map[string]string{"2025.1.7": "2025-01-07", "2024.9.23": "2024-09-23", "bogus": "bogus"} {
		if got := isoDate(in); got != want {
			t.Errorf("isoDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPDTrackerResolvesRequest(t *testing.T) {
	tr := &pdTracker{}
	// Source_Capabilities: 5V 3A, 9V 3A.
	caps := km003c.PDRecord{IsMessage: true, Raw: []byte{0}}
	caps.Message.Header = 0x21A1 // data, 2 objects, SRC, rev 3.x, Source_Capabilities
	caps.Message.Objects = []uint32{0x0001912C, 0x0002D12C}
	rec, ok := tr.process(caps)
	if !ok || len(rec.SourceCaps) != 2 || rec.SourceCaps[1].MaxVoltageV != 9 || rec.SourceCaps[1].MaxPowerW != 27 {
		t.Fatalf("caps %+v", rec)
	}
	req := km003c.PDRecord{IsMessage: true, Raw: []byte{0}}
	req.Message.Header = 0x1082 // data, 1 object, SNK, rev 3.x, Request
	req.Message.Objects = []uint32{2<<28 | 300<<10 | 300}
	rec, _ = tr.process(req)
	if rec.Request == nil || rec.Request.PDO == nil || rec.Request.PDO.MaxVoltageV != 9 ||
		rec.Request.OperatingCurrentA == nil || *rec.Request.OperatingCurrentA != 3 {
		t.Fatalf("request %+v", rec.Request)
	}
	ready := km003c.PDRecord{IsMessage: true, Raw: []byte{0}}
	ready.Message.Header = 0x01A6 // PS_RDY from source
	tr.process(ready)
	sum := tr.summary(nil)
	if sum.Contract == nil || sum.Contract.ObjectPosition != 2 || len(sum.SourceCapabilities) != 2 || sum.Note != nil {
		t.Fatalf("summary %+v", sum)
	}
	// A long (undecoded) message record is reported as a message, renders as
	// text without panicking, and does not end the contract.
	long := km003c.PDRecord{IsMessage: true, Long: true, SOP: 2, Raw: []byte{0x25}}
	rec, ok = tr.process(long)
	if !ok || rec.Kind != "message" || rec.Event != "" || rec.text() == "" {
		t.Fatalf("long record %+v", rec)
	}
	if tr.summary(nil).Contract == nil {
		t.Fatal("long record cleared the contract")
	}
	// GoodCRC is hidden by default.
	good := km003c.PDRecord{IsMessage: true, Raw: []byte{0}}
	good.Message.Header = 0x0041
	if _, ok := tr.process(good); ok {
		t.Error("GoodCRC not filtered")
	}
	// An empty capture explains itself, and the explanation matches the data.
	if n := (&pdTracker{}).summary(nil); n.Note == nil || *n.Note != noPDNote || n.Records == nil {
		t.Errorf("empty summary %+v", n)
	}
	replug := &pdTracker{}
	replug.process(km003c.PDRecord{Event: 2, EventParam: 2, Raw: []byte{0x45}})
	replug.process(km003c.PDRecord{Event: 1, EventParam: 2, Raw: []byte{0x45}})
	if n := replug.summary(nil); n.Note == nil || *n.Note != noPDMessagesNote || n.Events != 2 {
		t.Errorf("replug without PD: %+v", n)
	}
}
