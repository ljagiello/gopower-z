package km003c_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/ljagiello/gopower-z/internal/fakemeter"
)

// The fake meter (internal/fakemeter) replays frames captured on firmware
// 1.9.5 and, like the real meter, stops answering when a request is written
// while anything is still queued on the IN endpoint (Hung). Every write is
// also checked against the request whitelist.

func newDevice(t *testing.T) (*km003c.Device, *fakemeter.Meter) {
	t.Helper()
	fm := fakemeter.New(t)
	d := km003c.NewDevice(fm)
	km003c.SetMemSpacing(d, 0)
	return d, fm
}

func assertClean(t *testing.T, fm *fakemeter.Meter) {
	t.Helper()
	if fm.Hung {
		t.Fatal("a request was written while a reply was still queued (the real meter stalls and reboots)")
	}
}

func TestReadInfoConsumesZLP(t *testing.T) {
	d, fm := newDevice(t)
	info, err := d.ReadInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertClean(t, fm)
	want := km003c.DeviceInfo{
		Model: "KM003C", HardwareVersion: "2.2", HardwareDate: "2024.9.23",
		FirmwareModel: "KM003C", FirmwareVersion: "1.9.5", FirmwareDate: "2025.1.7",
		Serial: "050002", UUID: "0123456789ABCDEF0123456789ABCDEF",
	}
	if info != want {
		t.Errorf("info\n got %+v\nwant %+v", info, want)
	}
	if d.FirmwareVersion() != "1.9.5" || d.Layout() != km003c.TailLineAverages {
		t.Errorf("layout not selected: %q %v", d.FirmwareVersion(), d.Layout())
	}
	// A follow-up request must still be answered.
	if _, err := d.ReadSettings(context.Background()); err != nil {
		t.Fatalf("settings after info: %v", err)
	}
	assertClean(t, fm)
	if fm.Pending() != 0 {
		t.Fatalf("%d transfers left queued", fm.Pending())
	}
}

// A late reply after a timeout must be drained before the retry is written;
// otherwise the meter stalls (watchdog reboot on real hardware).
func TestTimeoutRetryDrainsLateReply(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 50 * time.Millisecond
	fm.LateReplies = 1
	a, err := d.ReadADC(context.Background())
	if err != nil {
		t.Fatalf("ReadADC after a late reply: %v", err)
	}
	assertClean(t, fm)
	if a.VBusUV == 0 {
		t.Fatal("no data")
	}
	// A late CONNECT reply is drained and CONNECT retried.
	fm.LateReplies = 1
	if _, err := d.ReadInfo(context.Background()); err != nil {
		t.Fatalf("ReadInfo after a late CONNECT reply: %v", err)
	}
	if _, err := d.ReadSettings(context.Background()); err != nil {
		t.Fatalf("settings after info: %v", err)
	}
	assertClean(t, fm)
}

func TestReadADCAndPD(t *testing.T) {
	d, fm := newDevice(t)
	a, pd, err := d.ReadADCAndPD(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pd == nil || a.VBusUV == 0 || pd.Status.TimestampMS == 0 {
		t.Fatalf("adc %+v pd %+v", a, pd)
	}
	assertClean(t, fm)
}

// A reply that arrives after the client already retried must not be taken as
// the retry's answer: that returns stale data and leaves the retry's own reply
// queued, which hangs the meter on the next write.
func TestVeryLateReplyIsMatchedByID(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 50 * time.Millisecond
	fm.VeryLate = true
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC: %v", err)
	}
	for i := range 3 {
		if _, err := d.ReadADC(context.Background()); err != nil {
			t.Fatalf("ReadADC #%d after a very late reply: %v", i+2, err)
		}
	}
	assertClean(t, fm)
}

// A reply with another transaction id is never returned as the answer, even
// when the real answer never comes.
func TestStaleReplyIsNotAnAnswer(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 50 * time.Millisecond
	fm.StaleBefore = km003c.TypeGetData
	fm.DropReplies = 2 // both attempts of getData
	if _, err := d.ReadADC(context.Background()); !errors.Is(err, km003c.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout (a stale reply was returned as data)", err)
	}
	// A stale frame ahead of a real reply is skipped.
	fm.StaleBefore = km003c.TypeGetData
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC with a stale frame ahead: %v", err)
	}
	assertClean(t, fm)
}

// MemoryRead skips frames of earlier requests before its confirmation.
func TestMemoryReadSkipsStaleFrame(t *testing.T) {
	d, fm := newDevice(t)
	fm.StaleBefore = km003c.TypeMemoryRead
	v, err := d.DetectLayout(context.Background())
	if err != nil || v != "1.9.5" {
		t.Fatalf("DetectLayout = %q, %v", v, err)
	}
	assertClean(t, fm)
	if fm.Pending() != 0 {
		t.Fatalf("%d transfers left queued", fm.Pending())
	}
}

// A reply cut off by the deadline leaves the rest queued; the endpoint must be
// drained before the next request.
func TestTruncatedReplyMarksDirty(t *testing.T) {
	d, fm := newDevice(t)
	fm.TruncateNext = true
	_, _ = d.ReadSettings(context.Background()) // 188-byte reply cut after 64 bytes
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC after truncated reply: %v", err)
	}
	assertClean(t, fm)
}

// A cancelled context must not put anything on the wire and must surface as
// context.Canceled (exit 130 in the CLI).
func TestCancelledContextDoesNotWrite(t *testing.T) {
	d, fm := newDevice(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.ReadADC(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(fm.Written) != 0 {
		t.Fatalf("%d frames written on a cancelled context", len(fm.Written))
	}
}

// Cancellation while waiting for a reply ends the read promptly with
// context.Canceled and does not retry.
func TestCancelWhileWaiting(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 5 * time.Second
	if _, err := d.ReadADC(context.Background()); err != nil { // initial drain done
		t.Fatal(err)
	}
	fm.Written = nil
	fm.DropReplies = 1
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, err := d.ReadADC(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancellation took %v", el)
	}
	if len(fm.Written) != 1 {
		t.Fatalf("%d requests written, want 1 (no retry after cancel)", len(fm.Written))
	}
}

// A meter that never answers yields ErrTimeout within the per-request
// timeout (retried once), not a hang.
func TestSilentMeterTimesOut(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 50 * time.Millisecond
	fm.DropReplies = 2
	start := time.Now()
	if _, err := d.ReadADC(context.Background()); !errors.Is(err, km003c.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatalf("ReadADC after timeouts: %v", err)
	}
	assertClean(t, fm)
}

// A MemoryRead the meter never answers times out within the per-request
// timeout, and the next read works.
func TestMemoryReadTimesOut(t *testing.T) {
	d, fm := newDevice(t)
	d.Timeout = 50 * time.Millisecond
	fm.DropOnly, fm.DropReplies = km003c.TypeMemoryRead, 1
	start := time.Now()
	if _, err := d.DetectLayout(context.Background()); !errors.Is(err, km003c.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
	if v, err := d.DetectLayout(context.Background()); err != nil || v != "1.9.5" {
		t.Fatalf("DetectLayout after a timeout = %q, %v", v, err)
	}
	assertClean(t, fm)
}

// MemoryRead confirmations that do not match the request are rejected. The
// blocks that were read are still returned (a failure of one block must not
// drop the others), and the meter stays usable.
func TestMemoryReadRejectsMismatchedConfirmation(t *testing.T) {
	cases := []struct {
		name   string
		block  uint32
		edit   func([]byte)
		want   km003c.DeviceInfo // fields expected to survive
		layout km003c.TailLayout
	}{
		// Firmware redirects OTP reads when the calibration records are
		// nearly full (docs/firmware.html §11).
		{"OTP redirect to another address", fakemeter.AddrOTP,
			func(c []byte) { binary.LittleEndian.PutUint32(c[4:], 0x03000D80) },
			km003c.DeviceInfo{Model: "KM003C", FirmwareVersion: "1.9.5"}, km003c.TailLineAverages},
		{"smaller length", fakemeter.AddrInfo,
			func(c []byte) { binary.LittleEndian.PutUint32(c[8:], 32) },
			km003c.DeviceInfo{FirmwareVersion: "1.9.5", Serial: "050002"}, km003c.TailLineAverages},
		{"oversized length", fakemeter.AddrFirmware,
			func(c []byte) { binary.LittleEndian.PutUint32(c[8:], 8192) },
			km003c.DeviceInfo{Model: "KM003C", Serial: "050002"}, km003c.TailUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, fm := newDevice(t)
			fm.Confirm = func(addr uint32, conf []byte) {
				if addr == c.block {
					c.edit(conf)
				}
			}
			info, err := d.ReadInfo(context.Background())
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("0x%08X", c.block)) {
				t.Fatalf("ReadInfo err = %v, want a failure of block 0x%08X", err, c.block)
			}
			if info.Model != c.want.Model || info.FirmwareVersion != c.want.FirmwareVersion || info.Serial != c.want.Serial {
				t.Errorf("partial info %+v, want model %q firmware %q serial %q",
					info, c.want.Model, c.want.FirmwareVersion, c.want.Serial)
			}
			if d.Layout() != c.layout {
				t.Errorf("layout %v, want %v", d.Layout(), c.layout)
			}
			fm.Confirm = nil
			if _, err := d.ReadADC(context.Background()); err != nil {
				t.Fatalf("meter not usable after a bad confirmation: %v", err)
			}
			assertClean(t, fm)
		})
	}
}

// Close drains a dirty endpoint so the next program does not stall the meter.
func TestCloseDrainsWhenDirty(t *testing.T) {
	d, fm := newDevice(t)
	fm.TruncateNext = true
	_, _ = d.ReadSettings(context.Background()) // leaves the rest of the reply queued
	fm.Queue([]byte{0x03, 0x09, 0x00, 0x00})    // and an unsolicited DISCONNECT
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if fm.Pending() != 0 {
		t.Fatalf("%d transfers left queued after Close", fm.Pending())
	}
	if !fm.Closed {
		t.Fatal("transport not closed")
	}
}

// After CONNECT, Close sends DISCONNECT and consumes the meter's answer, so
// no unsolicited session-timeout frame is left for the next program.
func TestCloseDisconnects(t *testing.T) {
	d, fm := newDevice(t)
	if _, err := d.DetectLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fm.Connected {
		t.Fatal("expected a connected session")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if fm.Connected || fm.Pending() != 0 {
		t.Fatalf("after Close: connected=%v queued=%d", fm.Connected, fm.Pending())
	}
	assertClean(t, fm)
	if last := fm.Written[len(fm.Written)-1]; last[0] != byte(km003c.TypeDisconnect) {
		t.Fatalf("last frame % X, want DISCONNECT", last)
	}
	// Without CONNECT, Close sends nothing.
	d2, fm2 := newDevice(t)
	if _, err := d2.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = d2.Close()
	if n := len(fm2.Written); n != 1 {
		t.Fatalf("%d frames written, want only the ADC request", n)
	}
}

// After an idle gap the meter may have ended the session and queued an
// unsolicited DISCONNECT: it is drained before the next request, and CONNECT
// is sent again before a MemoryRead.
func TestIdleGapDrainsAndReconnects(t *testing.T) {
	d, fm := newDevice(t)
	ctx := context.Background()
	if _, err := d.DetectLayout(ctx); err != nil {
		t.Fatal(err)
	}
	fm.ExpireSession()
	km003c.Backdate(d, 6*time.Second)
	if _, err := d.ReadADC(ctx); err != nil {
		t.Fatalf("ReadADC after idle gap: %v", err)
	}
	assertClean(t, fm)

	fm.ExpireSession()
	km003c.Backdate(d, 6*time.Second)
	n := len(fm.Written)
	info, err := d.ReadInfo(ctx)
	if err != nil || info.Serial != "050002" {
		t.Fatalf("ReadInfo after the session expired: %+v %v", info, err)
	}
	if first := fm.Written[n]; first[0] != byte(km003c.TypeConnect) {
		t.Fatalf("first frame after the idle gap % X, want CONNECT", first)
	}
	assertClean(t, fm)
}

// MemoryReads are spaced by the verified probe interval, also across
// DetectLayout and ReadInfo.
func TestMemoryReadSpacing(t *testing.T) {
	d, fm := newDevice(t)
	const gap = 80 * time.Millisecond
	km003c.SetMemSpacing(d, gap)
	ctx := context.Background()
	if _, err := d.DetectLayout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadInfo(ctx); err != nil {
		t.Fatal(err)
	}
	var prev time.Time
	reads := 0
	for i, w := range fm.Written {
		if w[0] != byte(km003c.TypeMemoryRead) {
			continue
		}
		reads++
		if !prev.IsZero() && fm.WrittenAt[i].Sub(prev) < gap {
			t.Errorf("MemoryRead %d only %v after the previous one", reads, fm.WrittenAt[i].Sub(prev))
		}
		prev = fm.WrittenAt[i]
	}
	if reads != 4 {
		t.Fatalf("%d MemoryReads, want 4", reads)
	}
}

// A meter that keeps sending data cannot be drained: no request is written.
func TestStuckMeterIsNotWritten(t *testing.T) {
	d, fm := newDevice(t)
	fm.Babble = true
	if _, err := d.ReadADC(context.Background()); !errors.Is(err, km003c.ErrStuck) {
		t.Fatalf("err = %v, want ErrStuck", err)
	}
	if len(fm.Written) != 0 {
		t.Fatalf("%d frames written to a babbling meter", len(fm.Written))
	}
}

// A failed write may still have reached the meter, so its reply is drained
// before the next request.
func TestWriteErrorMarksDirty(t *testing.T) {
	d, fm := newDevice(t)
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("pipe error")
	fm.WriteErr = boom // the request still reaches the meter and is answered
	if _, err := d.ReadADC(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertClean(t, fm)
}

// The same holds for the DISCONNECT that Close sends: if its write fails but
// the meter answers anyway, Close drains the answer so the next program does
// not stall the meter.
func TestCloseDrainsAfterFailedDisconnect(t *testing.T) {
	d, fm := newDevice(t)
	if _, err := d.DetectLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	fm.WriteErr = errors.New("pipe error")
	_ = d.Close()
	if fm.Pending() != 0 {
		t.Fatalf("%d transfers left queued after Close", fm.Pending())
	}
}

func TestRejectedRequest(t *testing.T) {
	d, fm := newDevice(t)
	fm.RejectNext = true
	if _, err := d.ReadSettings(context.Background()); !errors.Is(err, km003c.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if _, err := d.ReadSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertClean(t, fm)
}

// An empty PUT_DATA (nothing to report yet) is retried once.
func TestEmptyReplyRetried(t *testing.T) {
	d, fm := newDevice(t)
	fm.EmptyNext = true
	if _, err := d.ReadADC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fm.Written) != 2 {
		t.Fatalf("%d requests, want 2 (one retry)", len(fm.Written))
	}
	assertClean(t, fm)
}

// Every frame the Device writes is in the whitelist (the fake checks each
// write); exercise every public read once.
func TestEveryReadIsWhitelisted(t *testing.T) {
	d, fm := newDevice(t)
	ctx := context.Background()
	if _, err := d.DetectLayout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadADC(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReadADCAndPD(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadPD(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadInfo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, w := range fm.Written {
		if err := km003c.CheckRequest(w); err != nil {
			t.Errorf("sent % X: %v", w, err)
		}
	}
	assertClean(t, fm)
}
