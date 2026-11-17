package usbpd

import "testing"

// Field-by-field decoding of every PDO kind, with asymmetric values so that
// swapped fields, wrong shifts or min/max mix-ups show up.
func TestDecodeSourcePDOFields(t *testing.T) {
	cases := []struct {
		name string
		v    uint32
		want PDO
	}{
		{"fixed 9 V 2 A with flags", 1<<29 | 1<<26 | 1<<23 | 180<<10 | 200,
			PDO{Kind: PDOFixed, MinVolts: 9, MaxVolts: 9, MaxAmps: 2, DualRole: true, USBComms: true, EPRCapable: true}},
		{"fixed with the other flags", 1<<28 | 1<<27 | 1<<25 | 1<<24 | 100<<10 | 50,
			PDO{Kind: PDOFixed, MinVolts: 5, MaxVolts: 5, MaxAmps: 0.5, USBSuspend: true, Unconstrained: true, DualRoleData: true, UnchunkedExt: true}},
		{"battery 5-10 V 15 W", 1<<30 | 200<<20 | 100<<10 | 60,
			PDO{Kind: PDOBattery, MinVolts: 5, MaxVolts: 10, MaxWatts: 15}},
		{"variable 5-15 V 1.5 A", 2<<30 | 300<<20 | 100<<10 | 150,
			PDO{Kind: PDOVariable, MinVolts: 5, MaxVolts: 15, MaxAmps: 1.5}},
		{"PPS 3.3-11 V 3 A", 3<<30 | 110<<17 | 33<<8 | 60,
			PDO{Kind: PDOPPS, MinVolts: 3.3, MaxVolts: 11, MaxAmps: 3}},
		{"EPR AVS 15-48 V 140 W", 3<<30 | 1<<28 | 480<<17 | 150<<8 | 140,
			PDO{Kind: PDOEPRAVS, MinVolts: 15, MaxVolts: 48, MaxWatts: 140}},
		// SPR AVS power is the larger of 15 V x I15 and 20 V x I20.
		{"SPR AVS 3 A @15 V, 1 A @20 V", 3<<30 | 2<<28 | 300<<10 | 100,
			PDO{Kind: PDOSPRAVS, MinVolts: 9, MaxVolts: 20, MaxAmps: 3, MaxAmps2: 1, MaxWatts: 45}},
		{"SPR AVS 2 A @15 V, 2.25 A @20 V", 3<<30 | 2<<28 | 200<<10 | 225,
			PDO{Kind: PDOSPRAVS, MinVolts: 9, MaxVolts: 20, MaxAmps: 2, MaxAmps2: 2.25, MaxWatts: 45}},
		{"reserved APDO", 3<<30 | 3<<28 | 0x1234, PDO{Kind: PDOUnknown}},
	}
	for _, c := range cases {
		c.want.Raw = c.v
		got := DecodeSourcePDO(c.v)
		if !approx(got.MinVolts, c.want.MinVolts) || !approx(got.MaxVolts, c.want.MaxVolts) ||
			!approx(got.MaxAmps, c.want.MaxAmps) || !approx(got.MaxAmps2, c.want.MaxAmps2) || !approx(got.MaxWatts, c.want.MaxWatts) {
			t.Errorf("%s: got %+v\nwant %+v", c.name, got, c.want)
			continue
		}
		got.MinVolts, got.MaxVolts, got.MaxAmps, got.MaxAmps2, got.MaxWatts = c.want.MinVolts, c.want.MaxVolts, c.want.MaxAmps, c.want.MaxAmps2, c.want.MaxWatts
		if got != c.want {
			t.Errorf("%s: got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// Request decoding depends on the PDO the request selects.
func TestDecodeRDOFields(t *testing.T) {
	fixed := DecodeSourcePDO(100<<10 | 300)
	battery := DecodeSourcePDO(1<<30 | 200<<20 | 100<<10 | 60)
	pps := DecodeSourcePDO(3<<30 | 110<<17 | 33<<8 | 60)
	avs := DecodeSourcePDO(3<<30 | 1<<28 | 480<<17 | 150<<8 | 140)
	cases := []struct {
		name string
		v    uint32
		ref  *PDO
		want RDO
	}{
		{"fixed op 1 A max 1.5 A, capability mismatch", 1<<28 | 1<<26 | 100<<10 | 150, &fixed,
			RDO{Position: 1, CapMismatch: true, OpAmps: 1, MaxAmps: 1.5}},
		{"unknown PDO decodes as fixed", 4<<28 | 250<<10 | 300, nil,
			RDO{Position: 4, OpAmps: 2.5, MaxAmps: 3}},
		{"battery op 15 W", 2<<28 | 60<<10 | 80, &battery,
			RDO{Position: 2, OpWatts: 15}},
		{"PPS 9 V 2 A", 3<<28 | 450<<9 | 40, &pps,
			RDO{Position: 3, Volts: 9, OpAmps: 2}},
		{"EPR AVS 28 V 3 A, EPR mode", 8<<28 | 1<<22 | 1120<<9 | 60, &avs,
			RDO{Position: 8, EPRMode: true, Volts: 28, OpAmps: 3}},
	}
	for _, c := range cases {
		got := DecodeRDO(c.v, c.ref)
		if got.Position != c.want.Position || got.CapMismatch != c.want.CapMismatch || got.EPRMode != c.want.EPRMode ||
			!approx(got.Volts, c.want.Volts) || !approx(got.OpAmps, c.want.OpAmps) ||
			!approx(got.MaxAmps, c.want.MaxAmps) || !approx(got.OpWatts, c.want.OpWatts) {
			t.Errorf("%s: got %+v\nwant %+v", c.name, got, c.want)
		}
		if got.String() == "" {
			t.Errorf("%s: empty text", c.name)
		}
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d > -1e-9 && d < 1e-9
}

// Chunks that arrive out of order, skip a chunk or switch message type never
// produce a block; a new chunk 0 starts over.
func TestReassemblerRejectsBrokenSequences(t *testing.T) {
	block := make([]byte, 60) // three chunks: 26 + 26 + 8
	for i := range block {
		block[i] = byte(i + 1)
	}
	chunk := func(typ, n int) Message {
		lo := n * MaxChunk
		hi := min(lo+MaxChunk, len(block))
		m, err := ParseMessage(extMsg(typ, n, len(block), true, block[lo:hi]))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	feed := func(r *Reassembler, ms ...Message) ([]byte, bool) {
		var out []byte
		var ok bool
		for _, m := range ms {
			out, ok = r.Add(m)
		}
		return out, ok
	}
	if _, ok := feed(&Reassembler{}, chunk(17, 1), chunk(17, 2)); ok {
		t.Error("completed without chunk 0")
	}
	if _, ok := feed(&Reassembler{}, chunk(17, 0), chunk(17, 2)); ok {
		t.Error("completed with chunk 1 missing")
	}
	// All data present but out of order: must not be glued together.
	if out, ok := feed(&Reassembler{}, chunk(17, 0), chunk(17, 2), chunk(17, 1)); ok {
		t.Errorf("completed from chunks 0, 2, 1: % X", out)
	}
	if _, ok := feed(&Reassembler{}, chunk(17, 0), chunk(15, 1), chunk(17, 2)); ok {
		t.Error("completed across a message-type switch")
	}
	r := &Reassembler{}
	out, ok := feed(r, chunk(17, 0), chunk(17, 1), chunk(17, 0), chunk(17, 1), chunk(17, 2))
	if !ok || len(out) != len(block) || out[0] != 1 || out[59] != 60 {
		t.Fatalf("restart at chunk 0: ok=%v %d bytes", ok, len(out))
	}
	// A chunk request (the sink asking for the next chunk) carries no data.
	req := chunk(17, 1)
	req.Ext |= 1 << 10
	if _, ok := (&Reassembler{}).Add(req); ok {
		t.Error("chunk request produced a block")
	}
}
