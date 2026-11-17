package usbpd

import (
	"math"
	"testing"
)

// PD traffic is sniffed from arbitrary chargers and devices, so every decoder
// here takes untrusted input.

func FuzzParseMessage(f *testing.F) {
	f.Add([]byte{0xA1, 0x11, 0x2C, 0x91, 0x01, 0x00})                                     // Source_Capabilities
	f.Add([]byte{0x46, 0x01})                                                             // PS_RDY
	f.Add(extMsg(17, 0, 40, true, make([]byte, 26)))                                      // EPR caps chunk
	f.Add(extMsg(17, 1, 40, true, make([]byte, 14)))                                      // second chunk
	f.Add([]byte{0x91, 0x80, 0x00, 0x84})                                                 // chunk request
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // all ones
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseMessage(b)
		if err != nil {
			return
		}
		if len(m.Objects) > m.Header.NumObjects() {
			t.Fatalf("%d objects, header says %d", len(m.Objects), m.Header.NumObjects())
		}
		if len(m.ExtData) > len(b) || len(m.ExtData) > m.Ext.DataSize() {
			t.Fatalf("ext data %d bytes (size %d, input %d)", len(m.ExtData), m.Ext.DataSize(), len(b))
		}
		if m.Header.Name() == "" || m.Header.Role() == "" || m.Header.Rev() == "" {
			t.Fatal("empty header description")
		}
	})
}

func FuzzReassembler(f *testing.F) {
	block := make([]byte, 40)
	f.Add(append(extMsg(17, 0, 40, true, block[:26]), extMsg(17, 1, 40, true, block[26:])...), uint8(32))
	f.Add(extMsg(17, 0, 8, false, block[:8]), uint8(12))
	f.Fuzz(func(t *testing.T, stream []byte, chunkLen uint8) {
		var r Reassembler
		n := int(chunkLen%64) + 2
		for len(stream) > 0 {
			k := min(n, len(stream))
			m, err := ParseMessage(stream[:k])
			stream = stream[k:]
			if err != nil {
				continue
			}
			if out, ok := r.Add(m); ok {
				if len(out) != m.Ext.DataSize() && !m.Ext.Chunked() {
					t.Fatalf("unchunked block %d bytes, size %d", len(out), m.Ext.DataSize())
				}
				_ = Objects(out)
			}
		}
	})
}

func finite(t *testing.T, name string, v float64) {
	t.Helper()
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		t.Fatalf("%s = %v", name, v)
	}
}

func FuzzPDO(f *testing.F) {
	for _, v := range []uint32{0x0001912C, 0x0002D12C, 0x00064145, 0xC0000000 | 210<<17 | 33<<8 | 60,
		0xC0000000 | 1<<28 | 480<<17 | 150<<8 | 140, 3<<30 | 2<<28 | 300<<10 | 225, 0xFFFFFFFF, 0} {
		f.Add(v, v)
	}
	f.Fuzz(func(t *testing.T, pdoRaw, rdoRaw uint32) {
		p := DecodeSourcePDO(pdoRaw)
		_ = p.String()
		finite(t, "min volts", p.MinVolts)
		finite(t, "max volts", p.MaxVolts)
		finite(t, "max amps", p.MaxAmps)
		finite(t, "max watts", p.MaxWatts)
		for _, ref := range []*PDO{nil, &p} {
			r := DecodeRDO(rdoRaw, ref)
			_ = r.String()
			finite(t, "rdo volts", r.Volts)
			finite(t, "rdo amps", r.OpAmps)
			if r.Position < 0 || r.Position > 15 {
				t.Fatalf("position %d", r.Position)
			}
		}
	})
}
