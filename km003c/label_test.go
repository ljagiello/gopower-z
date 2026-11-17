package km003c

import "testing"

func TestTailLayoutFor(t *testing.T) {
	cases := map[string]TailLayout{
		"1.9.5": TailLineAverages, "1.9.9": TailLineAverages, "2.0.0": TailLineAverages,
		"2.0.6": TailStatus, "2.0.7": TailStatus, "2.1.0": TailStatus,
		"": TailUnknown, "x": TailUnknown,
	}
	for v, want := range cases {
		if got := TailLayoutFor(v); got != want {
			t.Errorf("TailLayoutFor(%q) = %v, want %v", v, got, want)
		}
	}
	// 2.0.7 tail as described by firmware analysis: temp2 absent, rate 1,
	// accelerometer flat (0xF0), battery 0, 0x2A = 0xFF.
	a := ADC{Tail: []byte{0x00, 0x80, 0x01, 0xF0, 0x00, 0x00, 0xFF, 0x00}}
	a.ApplyTail(TailStatus)
	if a.RateIndex != 1 || a.Accel != 0xF0 || a.HasLineAvg || !a.HasStatus || a.Temp2Valid() {
		t.Errorf("status tail %+v", a)
	}
}

func TestDashboardLabels(t *testing.T) {
	cases := []struct {
		v, dp, dm int // mV
		l1, l2    string
	}{
		{0, 0, 0, labelNA, labelNA},      // nothing attached (the meter shows [N/A])
		{5000, 0, 0, "DCP", labelNA},     // idle data lines on 5 V
		{5000, 600, 600, "DCP", labelNA}, // shorted D+/D- (BC1.2 DCP)
		{5000, 2700, 2700, "Apple 2.4A", labelNA},
		{5000, 600, 0, "QC2.0 5V", "FCP | SCP | AFC"},
		{9000, 3300, 600, "QC2.0 9V", labelNA},
		{9000, 1200, 1200, "Samsung 2.0A", "SFCP 9V"},
		{9000, 600, 0, "FCP | AFC", "FCP | SCP | AFC"},
		{12000, 600, 600, "QC2.0 12V", labelNA},
		{20000, 3300, 3300, "QC2.0 20V", labelNA},
		{13000, 600, 600, labelNA, "FCP | SCP"},
		{5000, 600, 3300, "QC3.0 | QC3+", labelNA},
		{5000, 100, 0, "DCP", labelNA}, // |dp-dm| == 100 boundary: first branch (dp<110 && dm<100)
		{5000, 300, 200, labelNA, labelNA},
	}
	for _, c := range cases {
		l1, l2 := dashboardLabel1(c.v, c.dp, c.dm), dashboardLabel2(c.v, c.dp, c.dm)
		if l1 != c.l1 || l2 != c.l2 {
			t.Errorf("labels(%d, %d, %d) = %q, %q; want %q, %q", c.v, c.dp, c.dm, l1, l2, c.l1, c.l2)
		}
	}
	// Captured idle frame: VBUS ~5 mV -> below 3.3 V -> N/A, matching the screen photo.
	a, err := ParseADCFrame(mustHex(t, refADC))
	if err != nil {
		t.Fatal(err)
	}
	if l1, l2 := DashboardLabels(a); l1 != labelNA || l2 != labelNA {
		t.Errorf("idle labels %q %q", l1, l2)
	}
}
