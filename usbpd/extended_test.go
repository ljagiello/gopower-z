package usbpd

import (
	"encoding/binary"
	"testing"
)

func TestSPRAVSMaxPower(t *testing.T) {
	// SPR AVS: 3.00 A at 15 V, 2.25 A at 20 V -> max(45 W, 45 W) = 45 W.
	v := uint32(3)<<30 | 2<<28 | 300<<10 | 225
	p := DecodeSourcePDO(v)
	if p.Kind != PDOSPRAVS || p.MaxAmps != 3 || p.MaxAmps2 != 2.25 || p.MaxWatts != 45 {
		t.Fatalf("%+v", p)
	}
}

// extMsg builds an extended message (header + ext header + data chunk).
func extMsg(typ, chunk, size int, chunked bool, data []byte) []byte {
	ndo := (2 + len(data) + 3) / 4
	h := uint16(typ) | uint16(ndo)<<12 | 0x8000 | 2<<6 | 1<<8
	e := uint16(size) | uint16(chunk)<<11
	if chunked {
		e |= 0x8000
	}
	b := make([]byte, 4, 4+ndo*4)
	binary.LittleEndian.PutUint16(b, h)
	binary.LittleEndian.PutUint16(b[2:], e)
	b = append(b, data...)
	for len(b) < 2+ndo*4 {
		b = append(b, 0)
	}
	return b
}

func TestReassembleEPRSourceCaps(t *testing.T) {
	// 10 PDOs = 40 bytes -> two chunks (26 + 14).
	block := make([]byte, 40)
	for i := range 10 {
		binary.LittleEndian.PutUint32(block[i*4:], 0x0001912C+uint32(i))
	}
	var r Reassembler
	m0, _ := ParseMessage(extMsg(17, 0, 40, true, block[:26]))
	if m0.Header.Name() != "EPR_Source_Capabilities" {
		t.Fatalf("name %s", m0.Header.Name())
	}
	if _, ok := r.Add(m0); ok {
		t.Fatal("complete after first chunk")
	}
	m1, _ := ParseMessage(extMsg(17, 1, 40, true, block[26:]))
	got, ok := r.Add(m1)
	if !ok || len(got) != 40 {
		t.Fatalf("reassembly: ok=%v len=%d", ok, len(got))
	}
	objs := Objects(got)
	if len(objs) != 10 || objs[9] != 0x0001912C+9 {
		t.Fatalf("objects %08X", objs)
	}
	// Unchunked extended message.
	mu, _ := ParseMessage(extMsg(17, 0, 8, false, block[:8]))
	if got, ok := r.Add(mu); !ok || len(got) != 8 {
		t.Fatalf("unchunked: ok=%v len=%d", ok, len(got))
	}
}
