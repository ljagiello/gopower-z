// Package fakemeter is a test double for the POWER-Z KM003C bulk interface.
// It answers requests with frames captured from a real meter (application
// firmware 1.9.5, hardware/km003c/testdata/fw1.9.5) and models the behaviour
// that matters for a safe client:
//
//   - Like the real meter, it never answers a request written while an earlier
//     reply (including a zero-length packet) is still queued on the IN
//     endpoint; the real firmware then stalls and its watchdog reboots it.
//     Such a write sets Hung.
//   - Read honours its context: with nothing queued it blocks until the
//     deadline (ErrTimeout) or cancellation (context.Canceled). A Read whose
//     context has no deadline fails the test, since against a silent meter it
//     would block forever.
//   - Replies echo the request's transaction id; the session (CONNECT) gates
//     MemoryRead, and DISCONNECT is answered with the meter's own counter.
//
// The exported knobs inject the failure modes a client must survive.
package fakemeter

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
)

// MemoryRead block addresses the meter serves (the client's whitelist).
const (
	AddrInfo     uint32 = 0x00000420
	AddrFirmware uint32 = 0x00004420
	AddrOTP      uint32 = 0x03000C00
)

// Meter is a fake bulk transport. The zero value is not usable; call New.
type Meter struct {
	tb       testing.TB
	in       [][]byte // queued IN transfers
	deferred [][]byte // replies that land after the next write
	late     bool

	// Written records every frame written, in order, and WrittenAt when.
	Written   [][]byte
	WrittenAt []time.Time
	// Hung is set when a request was written while a reply was still
	// queued; the real meter stops answering (and reboots).
	Hung bool
	// Connected reports whether a CONNECT session is open.
	Connected bool
	// Closed is set by Close.
	Closed bool

	// LateReplies delays the reply to each of the next n requests until
	// after the client's read has timed out.
	LateReplies int
	// VeryLate delays the next reply until the client has drained and
	// written its next request.
	VeryLate bool
	// TruncateNext delivers only the first 64-byte packet of the next reply
	// and reports the transfer as cut off (km003c.ErrIncomplete).
	TruncateNext bool
	// DropReplies leaves the next n requests unanswered (only requests of
	// type DropOnly, if set).
	DropReplies int
	DropOnly    km003c.MsgType
	// RejectNext answers the next request with REJECT.
	RejectNext bool
	// EmptyNext answers the next GET_DATA with a PUT_DATA without sections.
	EmptyNext bool
	// StaleBefore delivers an ADC reply carrying another transaction id
	// ahead of the reply to the next request of this type (0 = off).
	StaleBefore km003c.MsgType
	// Confirm, if set, may edit every MemoryRead confirmation (20 bytes:
	// header, address u32 at 4, length u32 at 8) before it is queued; addr
	// is the block that was requested.
	Confirm func(addr uint32, conf []byte)
	// WriteErr is returned by the next Write. Like a write cut off by a
	// deadline, the frame still reaches the meter, which queues its reply.
	WriteErr error
	// GoneAfter, if positive, makes every Write after that many written
	// frames fail with ErrGone (the meter was unplugged).
	GoneAfter int
	ErrGone   error
	// SilentAfter, if positive, leaves every request after that many
	// written frames unanswered (the meter hangs).
	SilentAfter int
	// ReadErr is returned by the next Read.
	ReadErr error
	// Babble makes the meter send unsolicited frames without end, so the
	// endpoint can never be drained.
	Babble bool
	// ADC, if set, replaces the 44-byte ADC payload of every ADC reply.
	ADC []byte
	// ADCSeq, if set, serves successive ADC replies in order; once it is
	// down to its last payload, that one repeats. It takes precedence over
	// ADC, so a test can drive a changing reading through a command.
	ADCSeq [][]byte
	// PD holds PD_PACKET payloads (12-byte status block plus records)
	// served by successive PD reads; when empty, the captured idle payload
	// is used.
	PD [][]byte
}

// New returns a fake meter that reports failures to tb.
func New(tb testing.TB) *Meter { return &Meter{tb: tb} }

// Frame returns a captured frame from hardware/km003c/testdata/fw1.9.5.
func Frame(tb testing.TB, name string) []byte {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("fakemeter: cannot locate testdata")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "hardware", "km003c", "testdata", "fw1.9.5")
	if filepath.Base(name) != name {
		tb.Fatalf("fakemeter: bad fixture name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(root, name+".hex")) //nolint:gosec // fixed testdata directory, plain file name
	if err != nil {
		tb.Fatal(err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		tb.Fatalf("%s: %v", name, err)
	}
	return b
}

// Queue appends a raw IN transfer (for example an unsolicited DISCONNECT).
func (m *Meter) Queue(b []byte) { m.in = append(m.in, b) }

// Pending reports how many IN transfers are queued.
func (m *Meter) Pending() int { return len(m.in) }

// ExpireSession ends the CONNECT session the way the meter's session timeout
// does: the session is dropped and an unsolicited DISCONNECT is queued.
func (m *Meter) ExpireSession() {
	m.Connected = false
	m.in = append(m.in, []byte{0x03, 0x5A, 0x00, 0x00})
}

// Close implements km003c.Transport.
func (m *Meter) Close() error {
	m.Closed = true
	return nil
}

// Write implements km003c.Transport.
func (m *Meter) Write(_ context.Context, p []byte) (err error) {
	if m.WriteErr != nil {
		werr := m.WriteErr
		m.WriteErr = nil
		defer func() { err = werr }()
	}
	if m.GoneAfter > 0 && len(m.Written) >= m.GoneAfter {
		return m.ErrGone
	}
	m.Written = append(m.Written, append([]byte(nil), p...))
	m.WrittenAt = append(m.WrittenAt, time.Now())
	if err := km003c.CheckRequest(p); err != nil {
		m.tb.Errorf("fakemeter: non-whitelisted frame written: % X (%v)", p, err)
	}
	if len(m.in) > 0 {
		m.Hung = true
	}
	if m.Hung {
		return nil
	}
	// A very late reply to the previous request lands right after this
	// write, ahead of this request's own reply.
	m.in = append(m.in, m.deferred...)
	m.deferred = nil
	h, err := km003c.ParseHeader(p)
	if err != nil {
		return nil
	}
	if m.StaleBefore != 0 && h.Type == m.StaleBefore {
		m.StaleBefore = 0
		m.in = append(m.in, m.withID(Frame(m.tb, "bulk_W1_1_resp"), h.ID-1))
	}
	if m.SilentAfter > 0 && len(m.Written) > m.SilentAfter {
		return nil
	}
	if m.DropReplies > 0 && (m.DropOnly == 0 || h.Type == m.DropOnly) {
		m.DropReplies--
		return nil
	}
	if m.VeryLate {
		m.VeryLate = false
		before := len(m.in)
		defer func() {
			m.deferred = append(m.deferred, m.in[before:]...)
			m.in = m.in[:before]
		}()
	}
	if m.LateReplies > 0 {
		m.LateReplies--
		m.late = true
	}
	if m.RejectNext {
		m.RejectNext = false
		m.in = append(m.in, []byte{byte(km003c.TypeReject), h.ID, 0x00, 0x00})
		return nil
	}
	switch h.Type {
	case km003c.TypeConnect:
		m.in = append(m.in, m.withID(Frame(m.tb, "bulk_W5_1_resp"), h.ID))
		m.Connected = true
	case km003c.TypeDisconnect:
		if m.Connected {
			m.in = append(m.in, []byte{0x03, 0x77, 0x00, 0x00})
		} else {
			m.in = append(m.in, []byte{byte(km003c.TypeReject), h.ID, 0x00, 0x00})
		}
		m.Connected = false
	case km003c.TypeGetData:
		m.getData(h)
	case km003c.TypeMemoryRead:
		m.memoryRead(p, h.ID)
	}
	return nil
}

func (m *Meter) withID(b []byte, id uint8) []byte {
	b[1] = id
	return b
}

func (m *Meter) getData(h km003c.Header) {
	if m.EmptyNext {
		m.EmptyNext = false
		m.in = append(m.in, []byte{byte(km003c.TypePutData), h.ID, 0x00, 0x00})
		return
	}
	switch km003c.Attribute(h.Word >> 1) {
	case km003c.AttADC:
		m.in = append(m.in, putData(h.ID, section{km003c.AttADC, m.adc()}))
	case km003c.AttSettings:
		m.in = append(m.in, m.withID(Frame(m.tb, "bulk_W2_1_resp"), h.ID))
	case km003c.AttPDPacket:
		m.in = append(m.in, putData(h.ID, section{km003c.AttPDPacket, m.pd()}))
	case km003c.AttADC | km003c.AttPDPacket:
		m.in = append(m.in, putData(h.ID, section{km003c.AttADC, m.adc()}, section{km003c.AttPDPacket, m.pd()}))
	}
}

func (m *Meter) adc() []byte {
	if len(m.ADCSeq) > 0 {
		p := m.ADCSeq[0]
		if len(m.ADCSeq) > 1 {
			m.ADCSeq = m.ADCSeq[1:]
		}
		return p
	}
	if m.ADC != nil {
		return m.ADC
	}
	return Frame(m.tb, "bulk_W1_1_resp")[8:]
}

func (m *Meter) pd() []byte {
	if len(m.PD) > 0 {
		p := m.PD[0]
		m.PD = m.PD[1:]
		return p
	}
	return Frame(m.tb, "bulk_W3_1_resp")[8:]
}

type section struct {
	att     km003c.Attribute
	payload []byte
}

// putData frames sections the way the meter does: header word bits 6-15
// carry len/4-3, each section header is att | next<<15 | size<<22.
func putData(id uint8, secs ...section) []byte {
	out := []byte{byte(km003c.TypePutData), id, 0, 0}
	for i, s := range secs {
		w := uint32(s.att) | uint32(len(s.payload))<<22
		if i < len(secs)-1 {
			w |= 1 << 15
		}
		out = binary.LittleEndian.AppendUint32(out, w)
		out = append(out, s.payload...)
	}
	binary.LittleEndian.PutUint16(out[2:], uint16((len(out)/4-3)<<6))
	return out
}

func (m *Meter) memoryRead(p []byte, id uint8) {
	if !m.Connected {
		// The firmware refuses MemoryRead outside a session.
		m.in = append(m.in, []byte{byte(km003c.TypeReject), id, 0x00, 0x00})
		return
	}
	n, block := "", uint32(0)
	for name, addr := range map[string]uint32{"1": AddrInfo, "2": AddrFirmware, "3": AddrOTP} {
		if want, err := km003c.MemoryReadRequest(id, addr, 64); err == nil && string(want) == string(p) {
			n, block = name, addr
		}
	}
	if n == "" {
		m.tb.Errorf("fakemeter: MemoryRead of an unknown block: % X", p)
		return
	}
	conf := m.withID(Frame(m.tb, "bulk_W6_"+n+"_resp"), id)
	if m.Confirm != nil {
		m.Confirm(block, conf)
	}
	m.in = append(m.in, conf, Frame(m.tb, "bulk_W6_"+n+"_respdata"), []byte{})
}

// Read implements km003c.Transport.
func (m *Meter) Read(ctx context.Context, p []byte) (int, error) {
	if m.ReadErr != nil {
		err := m.ReadErr
		m.ReadErr = nil
		return 0, err
	}
	if m.Babble {
		if ctx.Err() != nil {
			return 0, km003c.ErrTimeout
		}
		return copy(p, []byte{0x03, 0x01, 0x00, 0x00}), nil
	}
	if m.Hung || len(m.in) == 0 || m.late {
		m.late = false
		return 0, m.wait(ctx)
	}
	next := m.in[0]
	if m.TruncateNext && len(next) > 64 {
		m.TruncateNext = false
		n := copy(p, next[:64])
		m.in[0] = next[64:]
		return n, km003c.ErrIncomplete
	}
	if len(next) > len(p) {
		// A short buffer receives what fits; the rest stays queued.
		n := copy(p, next)
		m.in[0] = next[n:]
		return n, nil
	}
	m.in = m.in[1:]
	return copy(p, next), nil
}

// wait blocks like a read on a silent endpoint.
func (m *Meter) wait(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		m.tb.Errorf("fakemeter: Read without a deadline would block forever on a silent meter")
		return km003c.ErrTimeout
	}
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	return km003c.ErrTimeout
}
