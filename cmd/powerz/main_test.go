package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		if ce.Code != c.code || ce.exit != c.exit || ce.Retryable != c.retryable {
			t.Errorf("classify(%v) = %+v, want %s/%d/%v", c.err, ce, c.code, c.exit, c.retryable)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"},
		{"read", "-bogus"},
		{"read", "extra-arg"},
		{"measure", "-duration", "0s"},
		{"-json", "log", "-format", "xml"},
	} {
		if got := run(append([]string{"-json"}, args...)); got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, got, exitUsage)
		}
	}
}

// Every command must be documented in the schema, with flags as a list and
// all exit codes present.
func TestSchemaComplete(t *testing.T) {
	b, err := json.Marshal(buildSchema())
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		SchemaVersion int `json:"schema_version"`
		Commands      []struct {
			Name    string            `json:"name"`
			Flags   []json.RawMessage `json:"flags"`
			Example string            `json:"example"`
			Output  string            `json:"output"`
		} `json:"commands"`
		ExitCodes []map[string]any `json:"exit_codes"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != schemaVersion {
		t.Errorf("schema_version %d", s.SchemaVersion)
	}
	seen := map[string]bool{}
	for _, c := range s.Commands {
		seen[c.Name] = true
		if c.Flags == nil || c.Example == "" || c.Output == "" {
			t.Errorf("command %s incompletely documented", c.Name)
		}
	}
	for name := range commands {
		if !seen[name] {
			t.Errorf("command %s missing from schema", name)
		}
	}
	if len(s.ExitCodes) != 9 {
		t.Errorf("exit codes: %d", len(s.ExitCodes))
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

// The JSON field names are a contract; renaming one requires bumping
// schemaVersion.
func TestSnapshotFieldNames(t *testing.T) {
	a := loadADC(t)
	a.ApplyTail(km003c.TailLayoutFor("1.9.5"))
	s := toSnapshot(a, time.Unix(0, 0).UTC(), "050002", "1.9.5")
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	want := []string{"schema_version", "time", "serial", "firmware_version", "vbus_v", "ibus_a", "power_w",
		"vbus_avg_v", "ibus_avg_a", "power_avg_w", "temp_c", "temp2_c", "cc1_v", "cc2_v", "dp_v", "dm_v",
		"vdd_v", "cc2_avg_v", "dp_avg_v", "dm_avg_v", "sample_rate_sps", "sink_battery_percent",
		"inferred", "raw"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("unexpected fields: %v", m)
	}
	for k := range snapshotFields {
		if strings.Contains(k, ".") {
			continue
		}
		if _, ok := m[k]; !ok {
			t.Errorf("schema documents %q but snapshot lacks it", k)
		}
	}
	if s.VBusV < 0.004 || s.VBusV > 0.007 || s.TempC == nil || s.Temp2C != nil {
		t.Errorf("values: %+v", s)
	}
	if s.SampleRateSPS == nil || *s.SampleRateSPS != 50 || s.CC2AvgV == nil || s.Raw.TailLayout != "line-averages" {
		t.Errorf("firmware-dependent fields: %+v", s)
	}
	if s.Inferred.DashboardLabel != "[ N/A ]" { // VBUS ~5 mV, as on the meter's screen
		t.Errorf("dashboard label %q", s.Inferred.DashboardLabel)
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
	if rec.Request == nil || rec.Request.PDO == nil || rec.Request.PDO.MaxVoltageV != 9 || rec.Request.OperatingCurrentA != 3 {
		t.Fatalf("request %+v", rec.Request)
	}
	ready := km003c.PDRecord{IsMessage: true, Raw: []byte{0}}
	ready.Message.Header = 0x01A6 // PS_RDY from source
	tr.process(ready)
	sum := tr.summary(nil)
	if sum.Contract == nil || sum.Contract.ObjectPosition != 2 || len(sum.SourceCapabilities) != 2 {
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
}
