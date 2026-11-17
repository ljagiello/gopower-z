package usbpd

import "testing"

func TestHeader(t *testing.T) {
	h := Header(0x11A1) // Source_Capabilities, rev 3.x, SRC, DFP, 1 object
	if h.Name() != "Source_Capabilities" || h.NumObjects() != 1 || h.Role() != "SRC" || h.Rev() != "3.x" {
		t.Errorf("got %s n=%d %s %s", h.Name(), h.NumObjects(), h.Role(), h.Rev())
	}
	if Header(0x0041).Name() != "GoodCRC" {
		t.Errorf("GoodCRC: %s", Header(0x0041).Name())
	}
	if Header(0x0046).Name() != "PS_RDY" {
		t.Errorf("PS_RDY: %s", Header(0x0046).Name())
	}
}

func TestPDOs(t *testing.T) {
	cases := []struct {
		v    uint32
		want string
	}{
		{0x0001912C, "Fixed 5.00V 3.00A (15.0W)"},
		{0x0002D12C, "Fixed 9.00V 3.00A (27.0W)"},
		{0x00064145, "Fixed 20.00V 3.25A (65.0W)"},
		// PPS 3.3-21V 3A: 11 | 00 | max 210 <<17 | min 33 <<8 | 60
		{0xC0000000 | 210<<17 | 33<<8 | 60, "PPS 3.3-21.0V 3.00A"},
		// EPR AVS 15-48V 140W
		{0xC0000000 | 1<<28 | 480<<17 | 150<<8 | 140, "EPR AVS 15.0-48.0V 140W"},
	}
	for _, c := range cases {
		if got := DecodeSourcePDO(c.v).String(); got != c.want {
			t.Errorf("PDO %08X = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestRDO(t *testing.T) {
	fixed := DecodeSourcePDO(0x0002D12C)
	// Request PDO#2, 3A op / 3A max.
	r := DecodeRDO(2<<28|300<<10|300, &fixed)
	if r.Position != 2 || r.OpAmps != 3 || r.MaxAmps != 3 {
		t.Errorf("fixed RDO %+v", r)
	}
	pps := DecodeSourcePDO(0xC0000000 | 210<<17 | 33<<8 | 60)
	// PPS 9.00 V (450 × 20 mV), 2.0 A (40 × 50 mA), position 4.
	r = DecodeRDO(4<<28|450<<9|40, &pps)
	if r.Volts != 9 || r.OpAmps != 2 {
		t.Errorf("pps RDO %+v", r)
	}
}
