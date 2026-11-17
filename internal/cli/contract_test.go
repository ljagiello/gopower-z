package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
)

// The schema is the machine contract agents rely on. These tests marshal the
// real output types with realistic values and compare every key against the
// definitions served by `powerz schema`, in both directions, including JSON
// types and nullability.

func toAny(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return tBool
	case string:
		return tStr
	case float64:
		if x == math.Trunc(x) {
			return tInt // also acceptable where a number is documented
		}
		return tNum
	case []any:
		return tArr
	case map[string]any:
		return tObj
	}
	return fmt.Sprintf("%T", v)
}

// checkShape verifies a decoded JSON object against definitions[def].
func checkShape(t *testing.T, def string, v any) {
	t.Helper()
	docs, ok := definitions[def]
	if !ok {
		t.Fatalf("no definition %q", def)
	}
	byName := map[string]fieldDoc{}
	for _, d := range docs {
		byName[d.Name] = d
	}
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: not an object: %v", def, v)
	}
	seen := map[string]bool{}
	present := map[string]bool{"": true} // object paths that are present and non-null
	var walk func(prefix string, o map[string]any)
	walk = func(prefix string, o map[string]any) {
		for k, val := range o {
			name := prefix + k
			d, ok := byName[name]
			if !ok {
				t.Errorf("%s: field %q is not documented in the schema", def, name)
				continue
			}
			seen[name] = true
			jt := jsonType(val)
			switch {
			case jt == "null":
				if !d.Nullable {
					t.Errorf("%s: %q is null but documented as non-nullable", def, name)
				}
				continue
			case d.Type == tNum && (jt == tNum || jt == tInt):
			case d.Type != jt:
				t.Errorf("%s: %q is %s, documented as %s", def, name, jt, d.Type)
				continue
			}
			if len(d.Enum) > 0 && !slices.Contains(d.Enum, val.(string)) {
				t.Errorf("%s: %q = %q is not one of %v", def, name, val, d.Enum)
			}
			switch d.Type {
			case tObj:
				if d.Ref != "" {
					checkShape(t, d.Ref, val)
				} else {
					present[name] = true
					walk(name+".", val.(map[string]any))
				}
			case tArr:
				if d.Items == "" {
					t.Errorf("%s: array %q has no items type", def, name)
				}
				for _, e := range val.([]any) {
					switch {
					case d.Ref != "":
						checkShape(t, d.Ref, e)
					case jsonType(e) != d.Items && (d.Items != tNum || jsonType(e) != tInt):
						t.Errorf("%s: %q element %v is %s, documented items %s", def, name, e, jsonType(e), d.Items)
					}
				}
			}
		}
	}
	walk("", obj)
	for _, d := range docs {
		parent := ""
		if i := strings.LastIndex(d.Name, "."); i >= 0 {
			parent = d.Name[:i]
		}
		if present[parent] && !seen[d.Name] {
			t.Errorf("%s: documented field %q is missing from the output", def, d.Name)
		}
	}
}

// samplePD runs realistic PD traffic, framed as the meter frames it, through
// the parser and the tracker: attach, capabilities (fixed, PPS, battery),
// two requests, Accept, PS_RDY, an EPR request, a long record, a reserved
// SOP, a receive-error event and unparseable trailing bytes.
func samplePD(t *testing.T) (*pdTracker, []pdRecord) {
	t.Helper()
	tr := &pdTracker{}
	p := parsePD(t, pdPayload(
		pdEvent(1, 0x21),
		pdMsg(2, 0, hdrSrcCaps|4<<12, pdoFixed5V3A, pdoFixed9V3A, pdoPPS3311V3A, 1<<30|200<<20|100<<10|60),
		pdMsg(3, 0, hdrRequest, rdoFixed2),
		pdMsg(4, 0, hdrRequest, rdoPPS3),
		pdMsg(5, 0, hdrAccept),
		pdMsg(6, 0, hdrPSRdy),
		pdMsg(7, 0, hdrEPRReq, rdoAVS8, pdoEPRAVS),
		pdLong(8, []byte{0xAA, 0xBB}),
		pdMsg(9, 6, hdrAccept),
		pdEvent(10, 0x36),
		[]byte{0x9F, 0x00},
	))
	return tr, tr.packet(&p)
}

func TestSchemaContract(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_000_000)
	a := loadADC(t)
	// Snapshots for every tail layout (null and non-null variants).
	for _, l := range []km003c.TailLayout{km003c.TailUnknown, km003c.TailLineAverages, km003c.TailStatus} {
		b := a
		b.ApplyTail(l)
		b.IBusUA = -442_099
		fw, fwErr := "1.9.5", (*string)(nil)
		if l == km003c.TailUnknown {
			fw, fwErr = "", new("km003c: read timeout")
		}
		checkShape(t, "snapshot", toAny(t, toSnapshot(b, now, "050002", fw, fwErr)))
	}
	// A loaded reading with a dashboard label.
	loaded := km003c.ADC{VBusUV: 5_021_015, IBusUA: -442_099, DP: 29797, DM: 726, CC2: 16237,
		Tail: []byte{0x00, 0x80, 0x56, 0x06, 0xA9, 0x0B, 0x3F, 0x00}, TempRaw: 3600}
	loaded.ApplyTail(km003c.TailLineAverages)
	snap := toSnapshot(loaded, now, "050002", "1.9.5", nil)
	if snap.Inferred.DashboardLabel2 == nil || *snap.Inferred.DashboardLabel2 != "VOOC" ||
		snap.Inferred.CC.AdvertisedCurrentA == nil || *snap.Inferred.CC.AdvertisedCurrentA != 3.0 ||
		snap.Direction != "receptacle_to_plug" || snap.IBusA != 0.442099 {
		t.Errorf("loaded snapshot does not match the bench verification: %+v", snap)
	}
	checkShape(t, "snapshot", toAny(t, snap))

	tr, recs := samplePD(t)
	for _, r := range recs {
		checkShape(t, "pd_record", toAny(t, r))
	}
	sum := tr.summary(recs)
	if sum.Contract == nil || len(sum.SourceCapabilities) != 4 || sum.Undecoded != 1 || len(recs) != 11 {
		t.Fatalf("sample PD summary %+v (%d records)", sum, len(recs))
	}
	kinds := map[string]bool{}
	for _, r := range recs {
		kinds[r.Kind] = true
	}
	if !kinds["message"] || !kinds["event"] || !kinds["undecoded"] {
		t.Fatalf("sample does not cover every record kind: %v", kinds)
	}
	// A 40 s window with a current step, a gap of failed readings (so one bin
	// is empty and its value objects are null) and a temperature that is
	// only sometimes valid.
	var ss []sample
	for k := range 400 {
		if k >= 20 && k < 30 {
			continue
		}
		cur := 0.134
		if k >= 250 {
			cur = 0.441
		}
		ss = append(ss, sample{T: float64(k) / 10, V: 5.076, I: cur, P: 5.076 * cur, DP: 3, DM: 0.002, Temp: 28, TempOK: k%2 == 0})
	}
	an := analyze(ss, 40, 1)
	if an.Bins[2].Samples != 0 || an.Stats.TempC == nil {
		t.Fatalf("sample analysis does not cover an empty bin and temperature: %+v", an)
	}
	m := measureResult{SchemaVersion: schemaVersion, Serial: "050002", FirmwareVersion: new("1.9.5"),
		Start: utcTime(now), End: utcTime(now.Add(40 * time.Second)), EndReason: "completed", DurationS: 40, IntegratedS: 39.9,
		IntervalS: 0.1, Samples: 390, FailedSamples: 10, LastError: new("timeout"), Direction: "receptacle_to_plug",
		analysis: an, EnergyWh: 0.006, ChargeMAh: 1.2, Last: &snap, PD: sum}
	checkShape(t, "measure", toAny(t, m))
	empty := measureResult{SchemaVersion: schemaVersion, Serial: "050002", analysis: analyze(nil, 0, 1),
		FirmwareError: new("km003c: read timeout"), EndReason: "interrupted",
		Direction: "none", PD: (&pdTracker{}).summary(nil)}
	checkShape(t, "measure", toAny(t, empty))

	checkShape(t, "pd_end", toAny(t, newPDEnd(tr.summary(nil), now, 20, "completed")))
	checkShape(t, "pd_end", toAny(t, newPDEnd((&pdTracker{}).summary(nil), now, 3, "interrupted")))

	usb := km003c.USBDescription{Manufacturer: "ChargerLab", Product: "POWER-Z KM003C", Serial: "050002", Bus: 2, Address: 20}
	checkShape(t, "info", toAny(t, infoResult{SchemaVersion: schemaVersion, USB: usb, Errors: []string{},
		Device: &deviceJSON{Model: new("KM003C"), FirmwareVersion: new("1.9.5"), FirmwareDate: new("2025-01-07"), HardwareVersion: new("2.2"),
			HardwareDate: new("2024-09-23"), Serial: new("050002"), UUID: new("0123456789ABCDEF0123456789ABCDEF"), TailLayout: "line-averages"},
		Settings: &settingsJSON{DeviceName: "POWER-Z", SampleRateSPS: 50, BrightnessPercent: 49, RotationCode: 2}}))
	checkShape(t, "info", toAny(t, infoResult{SchemaVersion: schemaVersion, USB: usb, Errors: []string{"device: timeout"}}))
	checkShape(t, "info", toAny(t, infoResult{SchemaVersion: schemaVersion, USB: usb, Errors: []string{"device: read block 0x03000C00: x"},
		Device: &deviceJSON{Model: new("KM003C"), FirmwareVersion: new("1.9.5"), TailLayout: "line-averages"}}))
	checkShape(t, "devices", toAny(t, devicesResult{SchemaVersion: schemaVersion, Count: 1, Devices: []km003c.USBDescription{usb}}))
	checkShape(t, "version", toAny(t, versionResult{SchemaVersion: schemaVersion, Version: "dev"}))

	ce := classify(fmt.Errorf("x: %w", km003c.ErrBusy))
	ce.ExitCode = ce.exit
	var e any
	if err := json.Unmarshal(errorJSON(ce), &e); err != nil {
		t.Fatal(err)
	}
	checkShape(t, "error", e)
}

// Every command's output reference must exist, and every definition must be
// reachable from some command.
func TestSchemaReferences(t *testing.T) {
	s := buildSchema(newRootCmd(&globals{env: &env{}}))
	reached := map[string]bool{}
	var mark func(string)
	mark = func(def string) {
		if reached[def] {
			return
		}
		reached[def] = true
		for _, d := range definitions[def] {
			if d.Ref != "" {
				if _, ok := definitions[d.Ref]; !ok {
					t.Errorf("%s.%s references unknown definition %q", def, d.Name, d.Ref)
				}
				mark(d.Ref)
			}
		}
	}
	for _, c := range s["commands"].([]commandDoc) {
		for _, ref := range append(c.Output.Refs, c.Output.Stderr) {
			if _, ok := definitions[ref]; !ok {
				t.Errorf("command %s references unknown definition %q", c.Name, ref)
			}
			mark(ref)
		}
	}
	for def := range definitions {
		if !reached[def] {
			t.Errorf("definition %q is not reachable from any command", def)
		}
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
}
