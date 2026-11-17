package km003c

import (
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestControlHeader(t *testing.T) {
	cases := []struct {
		typ  MsgType
		id   uint8
		att  Attribute
		want string
	}{
		{TypeGetData, 0, AttADC, "0c000200"},
		{TypeGetData, 7, AttSettings, "0c071000"},
		{TypeGetData, 1, AttPDPacket, "0c012000"},
		{TypeGetData, 2, AttADC | AttPDPacket, "0c022200"},
		{TypeConnect, 3, 0, "02030000"},
	}
	for _, c := range cases {
		h := ControlHeader(c.typ, c.id, c.att)
		if got := hex.EncodeToString(h[:]); got != c.want {
			t.Errorf("ControlHeader(%v,%d,%v) = %s, want %s", c.typ, c.id, c.att, got, c.want)
		}
	}
}

// Reference ADC frame from open-powerz tests/vectors/adc_52.hex (firmware
// 2.0.6, idle, nothing attached).
const refADC = `41 01 80 02  01 00 00 0B  64 10 00 00  A1 FF FF FF  91 10 00 00  B0 FF FF FF
F5 10 00 00  00 00 00 00  BA 0E 25 03  26 01 D4 02  F2 02 30 7D  00 80 02 F0
00 00 FF 00`

func TestParseADCFrame(t *testing.T) {
	a, err := ParseADCFrame(mustHex(t, refADC))
	if err != nil {
		t.Fatal(err)
	}
	if a.VBusUV != 4196 || a.IBusUA != -95 || a.VBusAvgUV != 4241 || a.IBusAvgUA != -80 {
		t.Errorf("bus values: %+v", a)
	}
	if a.VBusRawUV != 4341 || a.IBusRawUA != 0 {
		t.Errorf("uncalibrated: %d %d", a.VBusRawUV, a.IBusRawUA)
	}
	if math.Abs(a.TempC()-29.453125) > 1e-9 || !a.TempValid() {
		t.Errorf("temp = %v", a.TempC())
	}
	if a.CC1 != 805 || a.CC2 != 294 || a.DP != 724 || a.DM != 754 || a.VDD != 32048 {
		t.Errorf("lines: cc1=%d cc2=%d dp=%d dm=%d vdd=%d", a.CC1, a.CC2, a.DP, a.DM, a.VDD)
	}
	if math.Abs(a.CC1V()-0.0805) > 1e-12 || math.Abs(a.VDDV()-3.2048) > 1e-12 {
		t.Errorf("scaling: cc1=%v vdd=%v", a.CC1V(), a.VDDV())
	}
	if len(a.Tail) != 8 {
		t.Errorf("tail len %d", len(a.Tail))
	}
}

func TestParseADC40(t *testing.T) {
	// Older firmware: 40-byte payload, size field 10 words.
	f := mustHex(t, "41 09 02 02 01 00 00 0A"+strings.Repeat(" 00", 40))
	if _, err := ParseADCFrame(f); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyPutData(t *testing.T) {
	if _, _, err := ParsePutData(mustHex(t, "41 05 02 00")); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestGuard(t *testing.T) {
	ok := func(frame string, k TransportKind) {
		t.Helper()
		if err := CheckRequest(mustHex(t, frame), k); err != nil {
			t.Errorf("CheckRequest(%s, %v) = %v, want ok", frame, k, err)
		}
	}
	bad := func(frame string, k TransportKind) {
		t.Helper()
		if err := CheckRequest(mustHex(t, frame), k); err == nil {
			t.Errorf("CheckRequest(%s, %v) allowed, want refusal", frame, k)
		}
	}
	ok("0c000200", Bulk)
	ok("0c000200", HID)
	ok("0c000200", CDC)
	ok("0c011000", Bulk)
	ok("0c012200", Bulk)
	ok("02010000", Bulk)
	bad("0c011000", HID)  // settings over HID hangs the meter
	bad("0c012200", HID)  // >64-byte reply over HID
	bad("0c012000", CDC)  // PD over CDC
	bad("0c010400", Bulk) // ADC queue (consumes, needs auth)
	bad("02010000", CDC)  // CONNECT on CDC starts a stream
	bad("02010000", HID)
	ok("03050000", Bulk)  // DISCONNECT ends the session
	bad("03050000", CDC)  // stops a stream on CDC
	bad("03050100", Bulk) // malformed
	for _, typ := range []byte{0x01, 0x04, 0x08, 0x09, 0x0A, 0x0E, 0x0F, 0x10, 0x11, 0x40, 0x42, 0x48, 0x4A, 0x4B, 0x4C, 0x4D} {
		bad(hex.EncodeToString([]byte{typ, 1, 0, 0}), Bulk)
	}
	bad("8c000200", Bulk) // extend bit
}

func TestMemoryReadRequestGuard(t *testing.T) {
	for _, addr := range []uint32{addrInfo, addrFirmware, addrOTP} {
		req, err := MemoryReadRequest(4, addr, 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(req) != 36 || req[0] != 0x44 || req[2] != 1 || req[3] != 1 {
			t.Fatalf("bad frame % X", req)
		}
		if err := CheckRequest(req, Bulk); err != nil {
			t.Errorf("addr 0x%X refused: %v", addr, err)
		}
		if err := CheckRequest(req, HID); err == nil {
			t.Errorf("MemoryRead allowed over HID")
		}
	}
	for _, addr := range []uint32{0x40010450, 0x0, 0x20000000} {
		req, _ := MemoryReadRequest(4, addr, 64)
		if err := CheckRequest(req, Bulk); err == nil {
			t.Errorf("addr 0x%X allowed, want refusal", addr)
		}
	}
	req, _ := MemoryReadRequest(4, addrInfo, 256)
	if err := CheckRequest(req, Bulk); err == nil {
		t.Errorf("oversized read allowed")
	}
}

func TestMemoryReadPlainRoundTrip(t *testing.T) {
	p := memoryReadPlain(0x4420, 64)
	enc, err := encryptECB(key0, p)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decryptECB(key0, enc)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(dec) != hex.EncodeToString(p) {
		t.Fatal("ECB round trip mismatch")
	}
}

func TestParsePDPacket(t *testing.T) {
	// status block + a 6-byte attach event (0x45) + a Source_Capabilities
	// with one fixed 5V/3A PDO: header 0x11A1 (type 1, rev 3.x, SRC, 1 obj).
	status := "10 27 00 00  88 13  00 00  2C 01  00 00"
	event := "45  20 27 00 00  11"
	// record: b0 = 0x80 | (len-1); len = 1+4+1+2+4 = 12 → b0 = 0x8B
	msg := "8B  30 27 00 00  00  A1 11  2C 91 01 00"
	p, err := ParsePDPacket(mustHex(t, status+event+msg))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.TimestampMS != 10000 || p.Status.VBusMV != 5000 || p.Status.CC1MV != 300 {
		t.Errorf("status %+v", p.Status)
	}
	if len(p.Records) != 2 || len(p.Trailing) != 0 {
		t.Fatalf("records %d trailing % X", len(p.Records), p.Trailing)
	}
	ev := p.Records[0]
	if ev.IsMessage || ev.Event != 1 || ev.EventParam != 1 {
		t.Errorf("event %+v", ev)
	}
	m := p.Records[1]
	if !m.IsMessage || m.Message.Header.Name() != "Source_Capabilities" || len(m.Message.Objects) != 1 {
		t.Fatalf("msg %+v", m)
	}
	if m.Message.Objects[0] != 0x0001912C {
		t.Errorf("pdo %08X", m.Message.Objects[0])
	}
}
