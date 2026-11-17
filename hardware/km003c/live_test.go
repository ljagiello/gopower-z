package km003c

import "testing"

// Real PD event records captured on firmware 1.9.5 while the charger was
// unplugged and replugged twice (GET_DATA 0x11: ADC + PD in one reply).
func TestFixtureLivePDEvents(t *testing.T) {
	want := []struct {
		file   string
		loaded bool        // current was flowing in this reply
		events [][2]uint32 // {meter time ms, event code}
	}{
		{"live_pd_events_1", true, [][2]uint32{{4418758, 2}, {4421556, 1}}}, // detach, attach
		{"live_pd_events_2", false, [][2]uint32{{4432561, 2}}},
		{"live_pd_events_3", true, [][2]uint32{{4435249, 1}}},
	}
	for _, w := range want {
		_, secs, err := ParsePutData(fixture(t, w.file))
		if err != nil {
			t.Fatalf("%s: %v", w.file, err)
		}
		as, ok := FindSection(secs, AttADC)
		if !ok {
			t.Fatalf("%s: no ADC section", w.file)
		}
		a, err := ParseADC(as.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if w.loaded && a.Direction() != FlowToPlug {
			t.Errorf("%s: direction %s (%d µA), want receptacle_to_plug", w.file, a.Direction(), a.IBusUA)
		}
		ps, ok := FindSection(secs, AttPDPacket)
		if !ok {
			t.Fatalf("%s: no PD section", w.file)
		}
		pd, err := ParsePDPacket(ps.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(pd.Records) != len(w.events) || len(pd.Trailing) != 0 {
			t.Fatalf("%s: %d records, trailing % X", w.file, len(pd.Records), pd.Trailing)
		}
		for i, r := range pd.Records {
			if r.IsMessage || r.TimestampMS != w.events[i][0] || uint32(r.Event) != w.events[i][1] || r.EventParam != 2 {
				t.Errorf("%s record %d: %+v, want t=%d event=%d on CC2", w.file, i, r, w.events[i][0], w.events[i][1])
			}
		}
	}
}
