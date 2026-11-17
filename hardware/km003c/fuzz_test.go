package km003c

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything the meter sends back is untrusted input. These fuzz targets
// check that the parsers never panic, that their outputs are internally
// consistent, that the request whitelist accepts exactly the canonical
// requests, and that the Device stays within the whitelist and terminates on
// any reply stream.
//
// Run one target with:
//
//	go test -run='^$' -fuzz='^FuzzParsePutData$' ./hardware/km003c
//
// FuzzTailLayoutFor, FuzzParsePutData, FuzzParseADC and FuzzMemoryRead mainly
// look for panics and out-of-bounds reads: their remaining checks hold by
// construction. FuzzCheckRequest, FuzzParsePDPacket and FuzzDeviceReplies
// check real properties (exact whitelist, byte accounting, no stall).

// seedFrames returns every captured frame from testdata as fuzz seeds.
func seedFrames(f *testing.F) [][]byte {
	f.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "fw1.9.5", "*.hex"))
	if err != nil || len(files) == 0 {
		f.Fatalf("no seed frames: %v", err)
	}
	var out [][]byte
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		if err != nil {
			f.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func addSeeds(f *testing.F) {
	for _, b := range seedFrames(f) {
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte{0x41, 0x01, 0x00, 0x00})
	f.Add([]byte{0x41, 0x01, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF})
}

func FuzzParsePutData(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, frame []byte) {
		_, secs, err := ParsePutData(frame)
		if err != nil {
			return
		}
		total := 4
		for _, s := range secs {
			total += 4 + len(s.Payload)
		}
		if total > len(frame) {
			t.Fatalf("sections cover %d bytes of a %d-byte frame", total, len(frame))
		}
	})
}

func FuzzParseADC(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, frame []byte) {
		a, err := ParseADCFrame(frame)
		if err != nil {
			return
		}
		for _, l := range []TailLayout{TailUnknown, TailLineAverages, TailStatus} {
			b := a
			b.ApplyTail(l)
			l1, l2, ok := DashboardLabels(b)
			if ok != (b.Layout != TailUnknown) || ok == (l1 == "") || ok == (l2 == "") {
				t.Fatalf("labels %q %q ok=%v for layout %v", l1, l2, ok, b.Layout)
			}
			if b.RateIndex < -1 || b.RateIndex > 3 {
				t.Fatalf("rate index %d", b.RateIndex)
			}
			_ = b.Temp2Valid()
			_, _ = b.TempC(), b.Power()
		}
	})
}

func FuzzParsePDPacket(f *testing.F) {
	addSeeds(f)
	// status + attach event + one-PDO Source_Capabilities, + a long record
	f.Add(mustHexF(f, "10270000 8813 0000 2C01 0000 4520270000 11 8B302700 0000A111 2C910100"))
	f.Add(mustHexF(f, "10270000 8813 0000 2C01 0000 25 00000000 02 0300 AABBCC"))
	f.Fuzz(func(t *testing.T, p []byte) {
		pd, err := ParsePDPacket(p)
		if err != nil {
			return
		}
		n := 12 + len(pd.Trailing)
		for _, r := range pd.Records {
			n += len(r.Raw)
			if r.IsMessage && r.Event != 0 {
				t.Fatalf("message record carries an event code: %+v", r)
			}
		}
		if n != len(p) {
			t.Fatalf("records + trailing = %d bytes, input %d", n, len(p))
		}
	})
}

func FuzzParseSettings(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, p []byte) {
		s, err := ParseSettings(p)
		if err != nil {
			return
		}
		if s.SampleRate > 3 || s.Brightness > 127 {
			t.Fatalf("out-of-range settings %+v", s)
		}
		assertPrintable(t, s.DeviceName)
	})
}

func FuzzMemoryRead(f *testing.F) {
	frames := seedFrames(f)
	for _, a := range frames {
		for _, b := range frames {
			if len(a) == 20 && len(b) == 64 {
				f.Add(a, b)
			}
		}
	}
	f.Add([]byte{0xC4, 1, 1, 1, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}, []byte{})
	f.Fuzz(func(t *testing.T, confirm, data []byte) {
		c, err := ParseMemoryReadConfirm(confirm)
		if err != nil {
			return
		}
		plain, err := DecodeMemoryData(c, data)
		if err != nil {
			return
		}
		if uint32(len(plain)) != c.Length {
			t.Fatalf("decoded %d bytes, confirmation says %d", len(plain), c.Length)
		}
		var info DeviceInfo
		ParseInfoBlock(plain, &info)
		ParseFirmwareBlock(plain, &info)
		ParseOTPBlock(plain, &info)
		for _, s := range []string{info.Model, info.HardwareVersion, info.HardwareDate, info.FirmwareModel,
			info.FirmwareVersion, info.FirmwareDate, info.Serial, info.UUID} {
			assertPrintable(t, s)
		}
	})
}

func FuzzTailLayoutFor(f *testing.F) {
	for _, s := range []string{"1.9.5", "2.0.0", "2.0.1", "2.0.7", "", "9.9.9", "1.2", "a.b.c", "2.0.0.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		switch TailLayoutFor(v) {
		case TailUnknown, TailLineAverages, TailStatus:
		default:
			t.Fatal("invalid layout")
		}
	})
}

// canonicalRequests lists every frame the whitelist must accept for a given
// transaction id, built independently of CheckRequest.
func canonicalRequests(t *testing.T, id uint8) [][]byte {
	var out [][]byte
	add := func(h [4]byte) { out = append(out, append([]byte(nil), h[:]...)) }
	for _, att := range []Attribute{AttADC, AttSettings, AttPDPacket, AttADC | AttPDPacket} {
		add(ControlHeader(TypeGetData, id, att))
	}
	add(ControlHeader(TypeConnect, id, 0))
	add(ControlHeader(TypeDisconnect, id, 0))
	for _, a := range []uint32{addrInfo, addrFirmware, addrOTP} {
		r, err := MemoryReadRequest(id, a, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// FuzzCheckRequest proves the whitelist is exact: a frame passes if and only
// if it is byte-identical to one of the canonical read requests.
func FuzzCheckRequest(f *testing.F) {
	for _, b := range seedFrames(f) {
		if len(b) == 65 { // hidapi buffer
			b = b[1:]
		}
		f.Add(b)
	}
	for _, typ := range []byte{0x01, 0x03, 0x04, 0x08, 0x09, 0x0A, 0x0E, 0x10, 0x42, 0x44, 0x48, 0x4A, 0x4C, 0x4D, 0x8C} {
		f.Add([]byte{typ, 1, 0, 0})
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		err := CheckRequest(frame)
		allowed := false
		if len(frame) >= 2 {
			for _, c := range canonicalRequests(t, frame[1]) {
				if bytes.Equal(c, frame) {
					allowed = true
				}
			}
		}
		if (err == nil) != allowed {
			t.Fatalf("CheckRequest(% X) = %v, canonical = %v", frame, err, allowed)
		}
	})
}

// fuzzTransport replays a fuzz-generated IN stream instantly and checks
// every write against the whitelist.
type fuzzTransport struct {
	t       *testing.T
	in      [][]byte
	writes  int
	partial bool
}

func (f *fuzzTransport) Close() error { return nil }

func (f *fuzzTransport) Write(_ context.Context, p []byte) error {
	f.writes++
	if err := CheckRequest(p); err != nil {
		f.t.Fatalf("non-whitelisted frame reached the transport: % X (%v)", p, err)
	}
	return nil
}

func (f *fuzzTransport) Read(_ context.Context, p []byte) (int, error) {
	if len(f.in) == 0 {
		return 0, ErrTimeout
	}
	next := f.in[0]
	if len(next) > len(p) {
		n := copy(p, next)
		f.in[0] = next[n:]
		return n, nil
	}
	f.in = f.in[1:]
	n := copy(p, next)
	if f.partial && n > 0 {
		f.partial = false
		return n, ErrIncomplete
	}
	return n, nil
}

// splitStream turns fuzz bytes into IN transfers: [len][bytes]...
func splitStream(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 && len(out) < 64 {
		n := int(data[0])
		data = data[1:]
		n = min(n, len(data))
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// FuzzDeviceReplies drives every Device read against an arbitrary reply
// stream: it must never panic, never hang, and never write a frame outside
// the whitelist.
func FuzzDeviceReplies(f *testing.F) {
	var stream []byte
	for _, b := range seedFrames(f) {
		if len(b) <= 255 {
			stream = append(stream, byte(len(b)))
			stream = append(stream, b...)
		}
	}
	f.Add(stream, uint8(0))
	f.Add([]byte{4, 0x05, 0x01, 0, 0, 20, 0xC4, 0x02, 0x01, 0x01, 0x20, 0x04, 0, 0, 0x40, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0}, uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		ft := &fuzzTransport{t: t, in: splitStream(data), partial: mode&1 != 0}
		d := NewDevice(ft)
		d.Timeout = 5 * time.Millisecond
		d.memSpacing = 0
		ctx := context.Background()
		_, _ = d.DetectLayout(ctx)
		_, _ = d.ReadADC(ctx)
		_, _, _ = d.ReadADCAndPD(ctx)
		_, _ = d.ReadPD(ctx)
		_, _ = d.ReadSettings(ctx)
		_, _ = d.ReadInfo(ctx)
		_ = d.Close()
	})
}

func assertPrintable(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			t.Fatalf("non-printable rune %U in %q", r, s)
		}
	}
}

func mustHexF(f *testing.F, s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		f.Fatal(err)
	}
	return b
}
