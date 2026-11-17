package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
)

// The CLI turns untrusted meter data into JSON for agents: whatever the meter
// or a sniffed charger sends, the output must stay valid JSON and rendering
// must not panic.

func cliSeeds(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "km003c", "testdata", "fw1.9.5", "*_resp.hex"))
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	pd, _ := hex.DecodeString("1027000088130000" + "2C010000" + "4520270000" + "11" + "8B302700" + "00" + "A111" + "2C910100" +
		"8B312700" + "00" + "8210" + "2C2C0120" + "86322700" + "00" + "A601")
	f.Add(pd)
}

func FuzzSnapshotJSON(f *testing.F) {
	cliSeeds(f)
	f.Fuzz(func(t *testing.T, frame []byte) {
		a, err := km003c.ParseADCFrame(frame)
		if err != nil {
			return
		}
		for _, l := range []km003c.TailLayout{km003c.TailUnknown, km003c.TailLineAverages, km003c.TailStatus} {
			b := a
			b.ApplyTail(l)
			s := toSnapshot(b, time.Unix(0, 0).UTC(), "000000", "1.9.5")
			out, err := json.Marshal(s)
			if err != nil || !json.Valid(out) {
				t.Fatalf("snapshot JSON: %v %s", err, out)
			}
		}
	})
}

func FuzzPDTracker(f *testing.F) {
	cliSeeds(f)
	f.Fuzz(func(t *testing.T, payload []byte) {
		pd, err := km003c.ParsePDPacket(payload)
		if err != nil {
			return
		}
		for _, show := range []bool{false, true} {
			tr := &pdTracker{showGoodCRC: show}
			var recs []pdRecord
			for _, r := range pd.Records {
				rec, ok := tr.process(r)
				if !ok {
					continue
				}
				_ = rec.text()
				recs = append(recs, rec)
				if out, err := json.Marshal(rec); err != nil || !json.Valid(out) {
					t.Fatalf("record JSON: %v", err)
				}
			}
			_ = tr.capsText()
			if out, err := json.Marshal(tr.summary(recs)); err != nil || !json.Valid(out) {
				t.Fatalf("summary JSON: %v", err)
			}
		}
	})
}
