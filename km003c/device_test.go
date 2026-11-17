package km003c

import (
	"context"
	"encoding/binary"
	"testing"
)

// fakeMeter replays captured firmware-1.9.5 replies. Like the real meter, it
// stops answering if a request arrives while an earlier reply (including a
// zero-length packet) is still queued on the IN endpoint.
type fakeMeter struct {
	t       *testing.T
	kind    TransportKind
	in      [][]byte
	hung    bool
	written [][]byte
	// lateReplies makes the next n replies arrive only after the client's
	// read has already timed out.
	lateReplies int
	late        bool
	// veryLate makes the next reply arrive only after the client has
	// already drained and written its next request (later than timeout plus
	// the drain window).
	veryLate bool
	deferred [][]byte
	// truncateNext delivers only the first packet of the next reply and
	// reports the transfer as cut off by the deadline.
	truncateNext bool
	// corruptConfirm makes the next MemoryRead confirmation claim 8 KiB.
	corruptConfirm bool
	connected      bool
}

func (f *fakeMeter) Kind() TransportKind { return f.kind }
func (f *fakeMeter) Close() error        { return nil }

func (f *fakeMeter) Write(_ context.Context, p []byte) error {
	f.written = append(f.written, append([]byte(nil), p...))
	if len(f.in) > 0 {
		f.hung = true // real firmware stalls here and the watchdog reboots it
	}
	if f.hung {
		return nil
	}
	// A very late reply to the previous request lands right after this
	// write, ahead of this request's own reply.
	f.in = append(f.in, f.deferred...)
	f.deferred = nil
	if f.veryLate {
		f.veryLate = false
		before := len(f.in)
		defer func() {
			f.deferred = append(f.deferred, f.in[before:]...)
			f.in = f.in[:before]
		}()
	}
	if f.lateReplies > 0 {
		f.lateReplies--
		f.late = true
	}
	h, _ := ParseHeader(p)
	// Framed replies echo the request's transaction id, as the real meter
	// does (verified 20/20 on firmware 1.9.5).
	reply := func(name string) []byte {
		b := fixture(f.t, name)
		b[1] = h.ID
		return b
	}
	switch h.Type {
	case TypeConnect:
		f.in = append(f.in, reply("bulk_W5_1_resp"))
		f.connected = true
	case TypeDisconnect:
		// The meter answers with its own DISCONNECT under its own counter,
		// or REJECT when not connected (docs/firmware.html).
		if f.connected {
			f.in = append(f.in, []byte{0x03, 0x77, 0x00, 0x00})
		} else {
			f.in = append(f.in, []byte{0x06, h.ID, 0x00, 0x00})
		}
		f.connected = false
	case TypeGetData:
		switch Attribute(h.Word >> 1) {
		case AttADC:
			f.in = append(f.in, reply("bulk_W1_1_resp"))
		case AttSettings:
			f.in = append(f.in, reply("bulk_W2_1_resp"))
		case AttPDPacket:
			f.in = append(f.in, reply("bulk_W3_1_resp"))
		case AttADC | AttPDPacket:
			f.in = append(f.in, reply("bulk_W4_1_resp"))
		}
	case TypeMemoryRead:
		plain, _ := decryptECB(key0, p[4:])
		n := map[uint32]string{addrInfo: "1", addrFirmware: "2", addrOTP: "3"}[binary.LittleEndian.Uint32(plain)]
		conf := reply("bulk_W6_" + n + "_resp")
		if f.corruptConfirm {
			f.corruptConfirm = false
			binary.LittleEndian.PutUint32(conf[8:], 8192)
		}
		f.in = append(f.in,
			conf,
			fixture(f.t, "bulk_W6_"+n+"_respdata"),
			[]byte{}, // zero-length packet
		)
	}
	return nil
}

// queue appends a raw frame to the IN endpoint (e.g. a stale reply).
func (f *fakeMeter) queue(b []byte) { f.in = append(f.in, b) }

func (f *fakeMeter) Read(_ context.Context, p []byte) (int, error) {
	if f.hung || len(f.in) == 0 {
		return 0, ErrTimeout
	}
	if f.late { // reply is queued but arrives after this read gave up
		f.late = false
		return 0, ErrTimeout
	}
	next := f.in[0]
	if f.truncateNext && len(next) > 64 {
		f.truncateNext = false
		n := copy(p, next[:64])
		f.in[0] = next[64:]
		return n, ErrIncomplete
	}
	if len(next) > len(p) {
		// A short buffer receives one full packet; the rest stays queued.
		n := copy(p, next)
		f.in[0] = next[n:]
		return n, nil
	}
	f.in = f.in[1:]
	return copy(p, next), nil
}

func TestReadInfoConsumesZLP(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	info, err := d.ReadInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fm.hung {
		t.Fatal("a request was sent while a reply/ZLP was still pending")
	}
	if info.FirmwareVersion != "1.9.5" || info.Serial != "050002" || info.Model != "KM003C" {
		t.Errorf("info %+v", info)
	}
	// A follow-up request must still be answered.
	if _, err := d.ReadSettings(context.Background()); err != nil {
		t.Fatalf("settings after info: %v", err)
	}
	if fm.hung || len(fm.in) != 0 {
		t.Fatalf("hung=%v pending=%d", fm.hung, len(fm.in))
	}
	for _, w := range fm.written {
		if err := CheckRequest(w, Bulk); err != nil {
			t.Errorf("sent non-whitelisted frame % X: %v", w, err)
		}
	}
}

// A late reply after a timeout must be drained before the retry is written;
// otherwise the meter stalls (watchdog reboot on real hardware).
func TestTimeoutRetryDrainsLateReply(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	fm.lateReplies = 1
	a, err := d.ReadADC(context.Background())
	if err != nil {
		t.Fatalf("ReadADC after a late reply: %v", err)
	}
	if fm.hung {
		t.Fatal("retry was written while the late reply was still queued")
	}
	if a.VBusUV == 0 {
		t.Fatal("no data")
	}
	// A failed info read must not poison the next exchange either.
	fm.lateReplies = 1
	if _, err := d.ReadInfo(context.Background()); err == nil {
		t.Log("info succeeded despite late reply (retry path)")
	}
	if _, err := d.ReadSettings(context.Background()); err != nil {
		t.Fatalf("settings after failed info: %v", err)
	}
	if fm.hung {
		t.Fatal("meter hung after a failed info read")
	}
}

func TestReadADCAndPD(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	a, pd, err := d.ReadADCAndPD(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pd == nil || a.VBusUV == 0 {
		t.Fatalf("adc %+v pd %+v", a, pd)
	}
}

func TestHIDRefusesNonADC(t *testing.T) {
	fm := &fakeMeter{t: t, kind: HID}
	d := NewDevice(fm)
	if _, err := d.ReadSettings(context.Background()); err == nil {
		t.Fatal("settings over HID allowed")
	}
	if _, err := d.ReadInfo(context.Background()); err == nil {
		t.Fatal("info over HID allowed")
	}
	if len(fm.written) != 0 {
		t.Fatalf("frames written over HID: %d", len(fm.written))
	}
}
