package main

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
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

// samplePD runs realistic PD traffic through the tracker: capabilities, a
// request, PS_RDY, an attach event and a long record.
func samplePD(t *testing.T) (*pdTracker, []pdRecord) {
	t.Helper()
	tr := &pdTracker{}
	var recs []pdRecord
	add := func(r km003c.PDRecord) {
		if rec, ok := tr.process(r); ok {
			recs = append(recs, rec)
		}
	}
	add(km003c.PDRecord{Event: 1, EventParam: 2, Raw: []byte{0x45, 0, 0, 0, 0, 0x21}})
	caps := km003c.PDRecord{IsMessage: true, Raw: []byte{0x8B}}
	caps.Message.Header = 0x31A1 // Source_Capabilities, 3 objects
	caps.Message.Objects = []uint32{0x0001912C, 0x0002D12C, 0xC0000000 | 210<<17 | 33<<8 | 60}
	caps.Message.Objects = append(caps.Message.Objects, 3<<30|2<<28|300<<10|225)
	add(caps)
	req := km003c.PDRecord{IsMessage: true, Raw: []byte{0x8B}}
	req.Message.Header = 0x1082
	req.Message.Objects = []uint32{2<<28 | 300<<10 | 300}
	add(req)
	pps := km003c.PDRecord{IsMessage: true, Raw: []byte{0x8B}}
	pps.Message.Header = 0x1082
	pps.Message.Objects = []uint32{3<<28 | 450<<9 | 40}
	add(pps)
	ready := km003c.PDRecord{IsMessage: true, Raw: []byte{0x86}}
	ready.Message.Header = 0x01A6
	add(ready)
	add(km003c.PDRecord{IsMessage: true, Long: true, SOP: 0, Raw: []byte{0x25}})
	add(km003c.PDRecord{Event: 6, EventParam: 3, Raw: []byte{0x45, 0, 0, 0, 0, 0x36}})
	return tr, recs
}

func TestSchemaContract(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_000_000)
	a := loadADC(t)
	// Snapshots for every tail layout (null and non-null variants).
	for _, l := range []km003c.TailLayout{km003c.TailUnknown, km003c.TailLineAverages, km003c.TailStatus} {
		b := a
		b.ApplyTail(l)
		b.IBusUA = -442_099
		checkShape(t, "snapshot", toAny(t, toSnapshot(b, now, "050002", map[bool]string{true: "", false: "1.9.5"}[l == km003c.TailUnknown])))
	}
	// A loaded reading with a dashboard label.
	loaded := km003c.ADC{VBusUV: 5_021_015, IBusUA: -442_099, DP: 29797, DM: 726, CC2: 16237,
		Tail: []byte{0x00, 0x80, 0x56, 0x06, 0xA9, 0x0B, 0x3F, 0x00}, TempRaw: 3600}
	loaded.ApplyTail(km003c.TailLineAverages)
	snap := toSnapshot(loaded, now, "050002", "1.9.5")
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
	if sum.Contract == nil || len(sum.SourceCapabilities) != 4 {
		t.Fatalf("sample PD summary %+v", sum)
	}
	var v, i, p, temp stat
	for k := range 5 {
		v.add(5 + float64(k)/1000)
		i.add(0.44)
		p.add(2.2)
		temp.add(28)
	}
	m := measureResult{SchemaVersion: schemaVersion, Serial: "050002", FirmwareVersion: new("1.9.5"),
		Start: utcTime(now), End: utcTime(now.Add(10 * time.Second)), DurationS: 10, IntegratedS: 9.9,
		IntervalS: 0.1, Samples: 100, FailedSamples: 1, LastError: new("timeout"), Direction: "receptacle_to_plug",
		Stats:    measureStats{VBusV: v.json(6), IBusA: i.json(6), PowerW: p.json(6), TempC: temp.json(3)},
		EnergyWh: 0.006, ChargeMAh: 1.2, Last: &snap, PD: sum}
	checkShape(t, "measure", toAny(t, m))
	empty := measureResult{SchemaVersion: schemaVersion, Serial: "050002", Stats: measureStats{},
		Direction: "none", PD: (&pdTracker{}).summary(nil)}
	checkShape(t, "measure", toAny(t, empty))

	checkShape(t, "pd_end", toAny(t, newPDEnd(tr.summary(nil), now, 20)))
	checkShape(t, "pd_end", toAny(t, newPDEnd((&pdTracker{}).summary(nil), now, 3)))

	usb := km003c.USBDescription{Manufacturer: "ChargerLab", Product: "POWER-Z KM003C", Serial: "050002", Bus: 2, Address: 20}
	checkShape(t, "info", toAny(t, infoResult{SchemaVersion: schemaVersion, USB: usb, Errors: []string{},
		Device: &deviceJSON{Model: "KM003C", FirmwareVersion: "1.9.5", FirmwareDate: "2025-01-07", HardwareVersion: "2.2",
			HardwareDate: "2024-09-23", Serial: "050002", UUID: new("0123456789ABCDEF0123456789ABCDEF"), TailLayout: "line-averages"},
		Settings: &settingsJSON{DeviceName: "POWER-Z", SampleRateSPS: 50, BrightnessPercent: 49, RotationCode: 2}}))
	checkShape(t, "info", toAny(t, infoResult{SchemaVersion: schemaVersion, USB: usb, Errors: []string{"device: timeout"}}))
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
	s := buildSchema()
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
