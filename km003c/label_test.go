package km003c

import "testing"

func TestTailLayoutFor(t *testing.T) {
	cases := map[string]TailLayout{
		"1.7.9": TailLineAverages, "1.9.5": TailLineAverages, "1.9.9": TailLineAverages,
		"2.0.0": TailLineAverages,
		"2.0.1": TailStatus, "2.0.5": TailStatus, "2.0.7": TailStatus, "2.1.0": TailStatus,
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

// Labels of the 2.0.7 firmware (FUN_00022098 / FUN_00021bd4).
func TestDashboardLabelsV207(t *testing.T) {
	cases := []struct {
		v, dp, dm int // mV
		l1, l2    string
	}{
		{0, 0, 0, labelNA, labelNA},
		{5000, 0, 0, "DCP", labelNA},
		{5000, 600, 600, "DCP", labelNA},
		{5000, 2700, 2700, "Apple 2.4A", labelNA},
		{5000, 600, 0, "QC2.0 5V", "FCP | SCP | AFC"},
		{9000, 3300, 600, "QC2.0 9V", labelNA},
		{9000, 1200, 1200, "Samsung 2.0A", "SFCP 9V"},
		{9000, 600, 0, "FCP | AFC", "FCP | SCP | AFC"},
		{12000, 600, 600, "QC2.0 12V", labelNA},
		{20000, 3300, 3300, "QC2.0 20V", labelNA},
		{13000, 600, 600, labelNA, "FCP | SCP"},
		{5000, 600, 3300, "QC3.0 | QC3+", labelNA},
		{5000, 100, 0, "DCP", labelNA},
		{5000, 300, 200, labelNA, labelNA},
	}
	for _, c := range cases {
		l1, l2 := label1v207(c.v, c.dp, c.dm), label2v207(c.v, c.dp, c.dm)
		if l1 != c.l1 || l2 != c.l2 {
			t.Errorf("v207(%d, %d, %d) = %q, %q; want %q, %q", c.v, c.dp, c.dm, l1, l2, c.l1, c.l2)
		}
	}
}

// Labels of the 1.9.5 firmware (FUN_0001fb44): inputs include IBUS in mA.
func TestDashboardLabelsV195(t *testing.T) {
	cases := []struct {
		v, i, dp, dm int // mV, mA
		l1, l2       string
	}{
		{0, 0, 0, 0, labelNA, labelNA},
		{5000, 500, 0, 0, labelNA, "DCP 1.5A"}, // idle data lines on 5 V
		{5000, 500, 600, 600, "DCP 1.5A", "DCP 1.5A"},
		{5000, 500, 2700, 2700, "Apple 2.4A", "DCP 1.5A"},
		{5000, 500, 3300, 3300, "DCP 1.5A", "DCP 1.5A"},
		{5000, 2500, 3300, 3300, "DCP 1.5A", "VOOC"}, // VOOC on line 2 needs > 2 A
		{5000, 500, 1200, 1200, "Samsung 2.0A", "DCP 1.5A"},
		{5000, 500, 600, 0, "QC2.0 5V", "FCP | SCP | AFC"},
		{5000, 2500, 600, 0, "QC2.0 | SCP", labelNA},
		{9000, 500, 3300, 600, "QC2.0 9V", labelNA},
		{5000, 500, 3300, 600, "VOOC", labelNA},
		{9000, 500, 3300, 2000, "SVOOC", labelNA},
		{9000, 500, 600, 0, "FCP | AFC", "FCP | SCP | AFC"},
		{9000, 500, 1200, 1200, labelNA, "SFCP 9V"},
		{12000, 500, 600, 600, "QC2.0 12V", labelNA},
		{20000, 500, 3300, 3300, "QC2.0 20V", labelNA},
		{9000, 2000, 3300, 3300, "VFCP | UFCS", "QC2.0 20V Err"},
		{9000, 2500, 3300, 3300, "VFCP | UFCS", "SVOOC"},
		{5000, 500, 600, 3300, "QC3.0 | QC3+", labelNA},
		{13000, 500, 600, 600, labelNA, "FCP | SCP"},
	}
	for _, c := range cases {
		l1, l2 := label1v195(c.v, c.i, c.dp, c.dm), label2v195(c.v, c.i, c.dp, c.dm)
		if l1 != c.l1 || l2 != c.l2 {
			t.Errorf("v195(%d, %d, %d, %d) = %q, %q; want %q, %q", c.v, c.i, c.dp, c.dm, l1, l2, c.l1, c.l2)
		}
	}
}

func TestDashboardLabelsDispatch(t *testing.T) {
	// Captured idle frame on firmware 1.9.5: VBUS ~5 mV -> below 3.3 V -> N/A,
	// matching the meter's screen.
	a, err := ParseADCFrame(mustHex(t, refADC))
	if err != nil {
		t.Fatal(err)
	}
	a.ApplyTail(TailLayoutFor("1.9.5"))
	if l1, l2 := DashboardLabels(a); l1 != labelNA || l2 != labelNA {
		t.Errorf("idle labels %q %q", l1, l2)
	}
	// 5 V with idle averages: 1.9.5 says "[ N/A ]" / "DCP 1.5A", 2.0.7 says "DCP".
	b := ADC{VBusUV: 5_000_000, IBusUA: 300_000, Tail: []byte{0x00, 0x80, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00}}
	b.ApplyTail(TailLineAverages)
	if l1, l2 := DashboardLabels(b); l1 != labelNA || l2 != "DCP 1.5A" {
		t.Errorf("1.9.5 labels %q %q", l1, l2)
	}
	b.ApplyTail(TailStatus)
	if l1, _ := DashboardLabels(b); l1 != "DCP" {
		t.Errorf("2.0.7 label %q", l1)
	}
	// Negative IBUS compares as a large unsigned value on 1.9.5.
	c := ADC{VBusUV: 5_000_000, IBusUA: -100_000, Tail: []byte{0x00, 0x80, 0x02, 0x00, 0x58, 0x02, 0x00, 0x00}}
	c.ApplyTail(TailLineAverages) // D+ avg 600 mV, D- avg 0
	if l1, _ := DashboardLabels(c); l1 != "QC2.0 | SCP" {
		t.Errorf("negative-current label %q", l1)
	}
}
