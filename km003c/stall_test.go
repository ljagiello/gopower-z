package km003c

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests model failure modes found in code review. The fake meter hangs
// (like the real one) if a request is written while anything is still queued.

// A reply that arrives after the client already retried must not be taken as
// the retry's answer: that returns stale data and leaves the retry's own reply
// queued, which hangs the meter on the next write.
func TestVeryLateReplyIsMatchedByID(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	d.Timeout = 50 * time.Millisecond
	fm.veryLate = true
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC: %v", err)
	}
	for i := range 3 {
		if _, err := d.ReadADC(context.Background()); err != nil {
			t.Fatalf("ReadADC #%d after a very late reply: %v", i+2, err)
		}
	}
	if fm.hung {
		t.Fatal("a request was written while a reply was still queued")
	}
}

// A reply cut off by the deadline leaves the rest queued; the endpoint must be
// drained before the next request.
func TestTruncatedReplyMarksDirty(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	fm.truncateNext = true
	_, _ = d.ReadSettings(context.Background()) // 188-byte reply cut after 64 bytes
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC after truncated reply: %v", err)
	}
	if fm.hung {
		t.Fatal("wrote while the rest of a truncated reply was queued")
	}
}

// A cancelled context must not put anything on the wire and must surface as
// context.Canceled (exit 130 in the CLI).
func TestCancelledContextDoesNotWrite(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.ReadADC(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(fm.written) != 0 {
		t.Fatalf("%d frames written on a cancelled context", len(fm.written))
	}
}

// A MemoryRead confirmation that does not match the request is rejected
// without panicking or allocating what it claims, and the meter stays usable.
func TestMemoryReadRejectsMismatchedConfirmation(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	fm.corruptConfirm = true
	if _, err := d.ReadInfo(context.Background()); err == nil {
		t.Fatal("ReadInfo accepted a confirmation claiming 8192 bytes")
	}
	if _, err := d.ReadADC(context.Background()); err != nil || fm.hung {
		t.Fatalf("meter not usable after a bad confirmation: err=%v hung=%v", err, fm.hung)
	}
}

// Close drains a dirty endpoint so the next program does not stall the meter.
func TestCloseDrainsWhenDirty(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	fm.queue([]byte{0x03, 0x09, 0x00, 0x00}) // unsolicited DISCONNECT
	d.dirty = true
	_ = d.Close()
	if len(fm.in) != 0 {
		t.Fatalf("%d frames left queued after Close", len(fm.in))
	}
}

// After CONNECT, Close sends DISCONNECT and consumes the meter's answer, so
// no unsolicited session-timeout frame is left for the next program.
func TestCloseDisconnects(t *testing.T) {
	fm := &fakeMeter{t: t, kind: Bulk}
	d := NewDevice(fm)
	d.infoSpacing = 0
	if _, err := d.DetectLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fm.connected {
		t.Fatal("expected a connected session")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if fm.connected || len(fm.in) != 0 || fm.hung {
		t.Fatalf("after Close: connected=%v queued=%d hung=%v", fm.connected, len(fm.in), fm.hung)
	}
	last := fm.written[len(fm.written)-1]
	if last[0] != byte(TypeDisconnect) {
		t.Fatalf("last frame % X, want DISCONNECT", last)
	}
	// Without CONNECT, Close sends nothing.
	fm2 := &fakeMeter{t: t, kind: Bulk}
	d2 := NewDevice(fm2)
	if _, err := d2.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = d2.Close()
	if n := len(fm2.written); n != 1 {
		t.Fatalf("%d frames written, want only the ADC request", n)
	}
}

// The guard only allows the exact documented requests.
func TestGuardExactWhitelist(t *testing.T) {
	for _, att := range []Attribute{0x009, 0x018, 0x019, 0x003} {
		h := ControlHeader(TypeGetData, 1, att)
		if err := CheckRequest(h[:], Bulk); err == nil {
			t.Errorf("attribute mask 0x%03X allowed", uint16(att))
		}
	}
	// A MemoryRead with the right address/length but altered padding.
	plain := memoryReadPlain(addrInfo, 64)
	plain[20] = 0x00
	enc, _ := encryptECB(key0, plain)
	frame := append([]byte{byte(TypeMemoryRead), 1, 1, 1}, enc...)
	if err := CheckRequest(frame, Bulk); err == nil {
		t.Error("non-canonical MemoryRead allowed")
	}
}
