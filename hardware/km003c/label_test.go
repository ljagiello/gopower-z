package km003c

import "testing"

func TestTailLayoutFor(t *testing.T) {
	cases := map[string]TailLayout{
		"1.7.9": TailLineAverages, "1.9.5": TailLineAverages, "1.9.9": TailLineAverages,
		"2.0.0": TailLineAverages,
		"2.0.1": TailStatus, "2.0.5": TailStatus, "2.0.7": TailStatus,
		// Outside the analysed range: not guessed.
		"1.7.8": TailUnknown, "2.0.8": TailUnknown, "2.1.0": TailUnknown, "3.0.0": TailUnknown,
		"": TailUnknown, "x": TailUnknown, "-1.9.5": TailUnknown,
	}
	for v, want := range cases {
		if got := TailLayoutFor(v); got != want {
			t.Errorf("TailLayoutFor(%q) = %v, want %v", v, got, want)
		}
	}
	// 2.0.7 tail as described by firmware analysis: temp2 absent, rate 1,
	// accelerometer flat (0xF0), battery 87.5 % (875 = 0x036B), 0x2A = 0xFF.
	a := ADC{Tail: []byte{0x00, 0x80, 0x01, 0xF0, 0x6B, 0x03, 0xFF, 0x00}}
	a.ApplyTail(TailStatus)
	if a.RateIndex != 1 || a.Accel != 0xF0 || a.HasLineAvg || !a.HasStatus || a.Temp2Valid() || a.BatteryPermille != 875 {
		t.Errorf("status tail %+v", a)
	}
	// Line-averages tail: temp2 25.5 °C (0x0CC0), CC2 avg 1624 mV with rate
	// index 2 in the low bits (0x065A), D+ 2985 mV, D- 9 mV.
	b := ADC{Tail: []byte{0xC0, 0x0C, 0x5A, 0x06, 0xA9, 0x0B, 0x09, 0x00}}
	b.ApplyTail(TailLineAverages)
	if !b.Temp2Valid() || b.Temp2C() != 25.5 || b.RateIndex != 2 || b.CC2AvgMV != 1624 ||
		b.DPAvgMV != 2985 || b.DMAvgMV != 9 || b.HasStatus {
		t.Errorf("line-averages tail %+v", b)
	}
	// Re-applying another layout clears the fields of the previous one.
	b.ApplyTail(TailStatus)
	if b.HasLineAvg || b.CC2AvgMV != 0 {
		t.Errorf("stale line averages after switching layout: %+v", b)
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
		// Threshold edges (±1).
		{3299, 0, 0, labelNA, labelNA},
		{3300, 0, 0, "DCP", labelNA},
		{5000, 109, 0, "DCP", labelNA},
		{5000, 110, 0, labelNA, labelNA},
		{5000, 2501, 2501, "Apple 2.4A", labelNA},
		{5000, 2500, 2501, "DCP", labelNA},
		{5000, 2501, 2900, labelNA, labelNA},
		{5000, 600, 2801, "QC3.0 | QC3+", labelNA},
		{5000, 600, 2800, labelNA, labelNA},
		{9000, 600, 600, "QC2.0 12V Error", labelNA},
		{9000, 3300, 3300, "QC2.0 20V Error", labelNA},
		{8500, 3300, 600, labelNA, labelNA},
		{8501, 3300, 600, "QC2.0 9V", labelNA},
		{12000, 2400, 2400, labelNA, "SFCP 12V"},
		{12500, 600, 0, "FCP | AFC", "FCP | SCP"},
		{13000, 600, 0, labelNA, "FCP | SCP"},
		{12499, 600, 0, "FCP | AFC", "FCP | SCP | AFC"},
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
		// Threshold edges (±1).
		{3299, 500, 2700, 2700, labelNA, labelNA},
		{3300, 500, 2700, 2700, "Apple 2.4A", labelNA},
		{5000, 500, 2801, 2801, "DCP 1.5A", "DCP 1.5A"},
		{5000, 500, 2800, 2800, "Apple 2.4A", "DCP 1.5A"},
		{5000, 2000, 600, 0, "QC2.0 5V", "FCP | SCP | AFC"},
		{5000, 2001, 600, 0, "QC2.0 | SCP", labelNA},
		{12000, 500, 2400, 2400, labelNA, "SFCP 12V"},
		{12000, 500, 600, 600, "QC2.0 12V", labelNA},
		{19000, 500, 3300, 3300, labelNA, "QC2.0 20V Err"},
		{19001, 500, 3300, 3300, "QC2.0 20V", "QC2.0 20V Err"},
		{19500, 500, 3300, 3300, "QC2.0 20V", labelNA},
	}
	for _, c := range cases {
		l1, l2 := label1v195(c.v, c.i, c.dp, c.dm), label2v195(c.v, c.i, c.dp, c.dm)
		if l1 != c.l1 || l2 != c.l2 {
			t.Errorf("v195(%d, %d, %d, %d) = %q, %q; want %q, %q", c.v, c.i, c.dp, c.dm, l1, l2, c.l1, c.l2)
		}
	}
}

// Bench-verified on firmware 1.9.5: source on the receptacle, 0.44 A load on
// the plug; the meter reported raw -442099 µA and its screen +0.433 A with the
// arrow toward the plug.
func TestFlowDirection(t *testing.T) {
	a := ADC{VBusUV: 5_021_015, IBusUA: -442_099, IBusAvgUA: -440_935, VBusAvgUV: 5_020_406}
	if a.Direction() != FlowToPlug || a.FlowA() != 0.442099 || a.FlowPower() <= 2.2 {
		t.Errorf("normal flow: dir=%s I=%v P=%v", a.Direction(), a.FlowA(), a.FlowPower())
	}
	a.IBusUA = 300_000
	if a.Direction() != FlowToReceptacle || a.FlowA() >= 0 {
		t.Errorf("reverse flow: dir=%s I=%v", a.Direction(), a.FlowA())
	}
	a.IBusUA = -84 // idle noise
	if a.Direction() != FlowNone {
		t.Errorf("idle: dir=%s", a.Direction())
	}
}

func TestDashboardLabelsDispatch(t *testing.T) {
	// open-powerz idle frame from firmware 2.0.6: VBUS ~4 mV -> below 3.3 V
	// -> N/A on both lines.
	a, err := ParseADCFrame(mustHex(t, refADC))
	if err != nil {
		t.Fatal(err)
	}
	a.ApplyTail(TailLayoutFor("2.0.6"))
	if l1, l2, ok := DashboardLabels(a); !ok || l1 != labelNA || l2 != labelNA {
		t.Errorf("idle labels %q %q %v", l1, l2, ok)
	}
	// 5 V with idle averages: 1.9.5 says "[ N/A ]" / "DCP 1.5A", 2.0.7 says "DCP".
	b := ADC{VBusUV: 5_000_000, IBusUA: 300_000, Tail: []byte{0x00, 0x80, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00}}
	b.ApplyTail(TailLineAverages)
	if l1, l2, _ := DashboardLabels(b); l1 != labelNA || l2 != "DCP 1.5A" {
		t.Errorf("1.9.5 labels %q %q", l1, l2)
	}
	b.ApplyTail(TailStatus)
	if l1, _, _ := DashboardLabels(b); l1 != "DCP" {
		t.Errorf("2.0.7 label %q", l1)
	}
	// Unknown firmware: no classifier, no labels.
	b.ApplyTail(TailUnknown)
	if l1, l2, ok := DashboardLabels(b); ok || l1 != "" || l2 != "" {
		t.Errorf("unknown firmware labels %q %q %v", l1, l2, ok)
	}
	// Negative IBUS compares as a large unsigned value on 1.9.5.
	c := ADC{VBusUV: 5_000_000, IBusUA: -100_000, Tail: []byte{0x00, 0x80, 0x02, 0x00, 0x58, 0x02, 0x00, 0x00}}
	c.ApplyTail(TailLineAverages) // D+ avg 600 mV, D- avg 0
	if l1, _, _ := DashboardLabels(c); l1 != "QC2.0 | SCP" {
		t.Errorf("negative-current label %q", l1)
	}
}

// Every label string of both classifiers is reachable; a mistyped or
// unreachable label fails here.
func TestEveryLabelReachable(t *testing.T) {
	volts := []int{0, 3299, 3300, 4501, 5000, 5499, 7001, 8501, 9000, 9499, 10000, 11001, 12000, 12499, 12500, 13000, 18501, 19001, 20000}
	lines := []int{0, 99, 109, 199, 299, 451, 501, 600, 699, 1000, 1101, 1200, 1299, 1699, 2100, 2400, 2501, 2700, 2801, 2899, 2901, 3300}
	amps := []int{0, 1001, 2000, 2001}
	got195, got207 := map[string]bool{}, map[string]bool{}
	for _, v := range volts {
		for _, dp := range lines {
			for _, dm := range lines {
				got207[label1v207(v, dp, dm)] = true
				got207[label2v207(v, dp, dm)] = true
				for _, i := range amps {
					got195[label1v195(v, i, dp, dm)] = true
					got195[label2v195(v, i, dp, dm)] = true
				}
			}
		}
	}
	want195 := []string{labelNA, "QC2.0 20V", "VFCP | UFCS", "DCP 1.5A", "QC2.0 9V", "VOOC", "SVOOC", "Apple 2.4A",
		"Samsung 2.0A", "QC3.0 | QC3+", "QC2.0 12V", "QC2.0 | SCP", "FCP | AFC", "QC2.0 5V",
		"SFCP 9V", "SFCP 12V", "FCP | SCP", "QC2.0 20V Err", "FCP | SCP | AFC"}
	want207 := []string{labelNA, "QC3.0 | QC3+", "DCP", "Apple 2.4A", "QC2.0 5V", "QC2.0 9V", "Samsung 2.0A",
		"FCP | AFC", "QC2.0 12V Error", "QC2.0 20V Error", "QC2.0 12V", "QC2.0 20V",
		"SFCP 9V", "SFCP 12V", "FCP | SCP", "FCP | SCP | AFC"}
	check := func(name string, got map[string]bool, want []string) {
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s: label %q never produced", name, w)
			}
			delete(got, w)
		}
		for g := range got {
			t.Errorf("%s: unexpected label %q", name, g)
		}
	}
	check("1.9.5", got195, want195)
	check("2.0.7", got207, want207)
}
