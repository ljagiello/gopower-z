package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
)

// The CLI turns untrusted meter data into JSON for agents: whatever the meter
// or a sniffed charger sends, rendering must not panic and every object must
// match the schema served by `powerz schema` (keys, types, nullability,
// enums), not merely be valid JSON.

func cliSeeds(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "hardware", "km003c", "testdata", "fw1.9.5", "*_resp.hex"))
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
	f.Add(negotiation())
	f.Add(pdPayload(pdMsg(1, 5, hdrAccept), pdMsg(2, 0, hdrEPRReq, rdoAVS8, pdoEPRAVS), pdLong(3, []byte{1}), []byte{0x81}))
}

// shape decodes v from its JSON form and checks it against a definition.
func shape(t *testing.T, def string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s JSON: %v", def, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	checkShape(t, def, out)
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
			shape(t, "snapshot", toSnapshot(b, time.Unix(0, 0).UTC(), "000000", "1.9.5", nil))
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
			recs := tr.packet(&pd)
			for _, rec := range recs {
				_ = rec.text()
				shape(t, "pd_record", rec)
			}
			_ = tr.capsText()
			sum := tr.summary(recs)
			shape(t, "pd_summary", sum)
			shape(t, "pd_end", newPDEnd(sum, time.Unix(0, 0), 1, "completed"))
		}
	})
}
