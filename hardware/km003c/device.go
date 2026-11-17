package km003c

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Device is a high-level, read-only client for one meter.
//
// The meter stalls - and its watchdog reboots it - if it receives a request
// while a previous reply or zero-length packet is still queued on its IN
// endpoint. Device therefore keeps a "dirty" flag: any exchange that did not
// end cleanly marks the endpoint dirty, and the next request is written only
// after the endpoint has been drained.
type Device struct {
	t         Transport
	mu        sync.Mutex
	id        uint8
	connected bool
	dirty     bool
	// lastIO is when the last request was written. After an idle gap the
	// meter may have queued an unsolicited DISCONNECT (firmware 2.0.7 drops
	// the session after ~10 s), so the endpoint is drained first.
	lastIO time.Time
	// layout selects how ADC tail bytes are decoded (see DetectLayout).
	layout   TailLayout
	firmware string
	// memSpacing is the minimum gap between two MemoryReads (the spacing of
	// the verified probe sequence); lastMem is when the last one was sent.
	memSpacing time.Duration
	lastMem    time.Time

	// Timeout bounds each request/reply exchange (draining happens before
	// the timeout starts).
	Timeout time.Duration
	// Trace, if set, receives every frame sent ("tx") and received ("rx").
	Trace func(dir string, frame []byte)
}

// NewDevice wraps an open transport. Stale replies left on the IN endpoint by
// a previous session are drained before the first request.
func NewDevice(t Transport) *Device {
	return &Device{t: t, Timeout: time.Second, dirty: true, memSpacing: 300 * time.Millisecond}
}

// Close ends the session and closes the transport. If CONNECT was sent, it
// sends DISCONNECT: otherwise the meter's session timeout (5 s on 1.9.5)
// later queues an unsolicited DISCONNECT that the next program to open the
// meter would find waiting. It then drains anything still queued.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected {
		d.disconnect()
	}
	if d.dirty {
		d.drain()
	}
	return d.t.Close()
}

// disconnect sends DISCONNECT and consumes the meter's answer. The meter
// replies with its own 03 frame (carrying its own counter, not the request
// id), or with REJECT if the session had already timed out. Its only effect
// is clearing the session flags (connected, streaming, PD capture) - see
// docs/firmware.html.
func (d *Device) disconnect() {
	ctx, cancel := context.WithTimeout(context.Background(), d.Timeout)
	defer cancel()
	if err := d.prepare(ctx); err != nil {
		return
	}
	h := ControlHeader(TypeDisconnect, d.nextID(), 0)
	if err := d.send(ctx, h[:]); err != nil {
		return
	}
	d.connected = false
	buf := make([]byte, 4096)
	for {
		frame, err := d.read(ctx, buf)
		if err != nil {
			d.dirty = true
			return
		}
		if rh, err := ParseHeader(frame); err == nil && (rh.Type == TypeDisconnect || rh.Type == TypeReject) {
			return
		}
	}
}

// idleDrainAfter is how long the link may be idle before the IN endpoint is
// drained ahead of the next request.
const idleDrainAfter = 4 * time.Second

// drain discards everything queued on the IN endpoint (stale replies and
// zero-length packets). It reports whether the endpoint was left empty.
func (d *Device) drain() bool {
	buf := make([]byte, 4096)
	for range 16 {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		n, err := d.t.Read(ctx, buf)
		cancel()
		if err != nil && !errors.Is(err, ErrIncomplete) {
			return true // nothing more pending
		}
		if d.Trace != nil {
			d.Trace("rx(stale)", buf[:n])
		}
	}
	return false
}

// idle reports whether the link has been quiet long enough that the meter may
// have dropped the session and queued an unsolicited DISCONNECT.
func (d *Device) idle() bool {
	return !d.lastIO.IsZero() && time.Since(d.lastIO) > idleDrainAfter
}

// prepare makes sure nothing is queued before a request is written. It runs
// before the exchange deadline starts, so draining never eats into it.
func (d *Device) prepare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// After an idle gap the meter may also have ended the session; connect
	// checks for that itself before a MemoryRead.
	if d.dirty || d.idle() {
		if !d.drain() {
			d.dirty = true
			return ErrStuck
		}
		d.dirty = false
	}
	return nil
}

func (d *Device) nextID() uint8 {
	d.id++
	return d.id
}

// send checks a frame against the whitelist and writes it.
func (d *Device) send(ctx context.Context, frame []byte) error {
	if err := CheckRequest(frame); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Trace != nil {
		d.Trace("tx", frame)
	}
	d.lastIO = time.Now()
	if err := d.t.Write(ctx, frame); err != nil {
		d.dirty = true // a cancelled write may still have reached the meter
		return err
	}
	return nil
}

// read returns one IN transfer. A transfer cut off by the deadline is
// returned as data, but the endpoint is marked dirty because the rest of the
// reply may still be queued.
func (d *Device) read(ctx context.Context, buf []byte) ([]byte, error) {
	n, err := d.t.Read(ctx, buf)
	if errors.Is(err, ErrIncomplete) {
		d.dirty = true
		err = nil
	}
	if err != nil {
		return nil, err
	}
	frame := buf[:n]
	if d.Trace != nil {
		d.Trace("rx", frame)
	}
	return frame, nil
}

// exchange sends a request and waits for the reply of one of the wanted
// types that echoes the request's transaction id (the meter echoes it on
// every reply; verified 20/20 on firmware 1.9.5). Frames with another id are
// stale replies to earlier requests; they are consumed and skipped, never
// returned.
func (d *Device) exchange(ctx context.Context, req []byte, want ...MsgType) ([]byte, error) {
	if err := d.prepare(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	if err := d.send(ctx, req); err != nil {
		return nil, err
	}
	id := req[1]
	buf := make([]byte, 4096)
	for {
		frame, err := d.read(ctx, buf)
		if err != nil {
			d.dirty = true // the reply may still arrive
			return nil, err
		}
		h, err := ParseHeader(frame)
		if err != nil || h.ID != id {
			continue // zero-length packet or stale reply
		}
		if slices.Contains(want, h.Type) {
			return append([]byte(nil), frame...), nil
		}
		if h.Type == TypeReject {
			return nil, fmt.Errorf("%w: request % X", ErrRejected, req[:4])
		}
		// DISCONNECT ("no data") or an unexpected type: keep waiting.
	}
}

// getData performs a GET_DATA exchange and returns the reply sections.
func (d *Device) getData(ctx context.Context, att Attribute) ([]Section, error) {
	var lastErr error
	for range 2 {
		h := ControlHeader(TypeGetData, d.nextID(), att)
		frame, err := d.exchange(ctx, h[:], TypePutData)
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrTimeout) && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		_, secs, err := ParsePutData(frame)
		if errors.Is(err, ErrEmpty) {
			lastErr = err
			continue
		}
		return secs, err
	}
	return nil, lastErr
}

// markDirtyOn sets the dirty flag when a public read fails for any reason
// (truncated reply, missing section, parse error), since the rest of the
// reply may still be queued.
func (d *Device) markDirtyOn(err *error) {
	if *err != nil {
		d.dirty = true
	}
}

// ReadADC reads one ADC snapshot.
func (d *Device) ReadADC(ctx context.Context) (a ADC, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	secs, err := d.getData(ctx, AttADC)
	if err != nil {
		return ADC{}, err
	}
	s, ok := FindSection(secs, AttADC)
	if !ok {
		return ADC{}, fmt.Errorf("km003c: reply has no ADC section")
	}
	a, err = ParseADC(s.Payload)
	a.ApplyTail(d.layout)
	return a, err
}

// ReadADCAndPD reads an ADC snapshot together with the pending PD events in a
// single exchange. The PD event FIFO on the device is drained.
func (d *Device) ReadADCAndPD(ctx context.Context) (a ADC, pd *PDPacket, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	secs, err := d.getData(ctx, AttADC|AttPDPacket)
	if err != nil {
		return ADC{}, nil, err
	}
	s, ok := FindSection(secs, AttADC)
	if !ok {
		return ADC{}, nil, fmt.Errorf("km003c: reply has no ADC section")
	}
	if a, err = ParseADC(s.Payload); err != nil {
		return a, nil, err
	}
	a.ApplyTail(d.layout)
	if ps, ok := FindSection(secs, AttPDPacket); ok {
		p, err := ParsePDPacket(ps.Payload)
		if err != nil {
			return a, nil, err
		}
		return a, &p, nil
	}
	return a, nil, nil
}

// ReadPD reads the pending PD events. The PD event FIFO on the device is
// drained.
func (d *Device) ReadPD(ctx context.Context) (p PDPacket, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	secs, err := d.getData(ctx, AttPDPacket)
	if err != nil {
		return PDPacket{}, err
	}
	s, ok := FindSection(secs, AttPDPacket)
	if !ok {
		return PDPacket{}, fmt.Errorf("km003c: reply has no PD section")
	}
	return ParsePDPacket(s.Payload)
}

// ReadSettings reads the device settings block.
func (d *Device) ReadSettings(ctx context.Context) (st Settings, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	secs, err := d.getData(ctx, AttSettings)
	if err != nil {
		return Settings{}, err
	}
	s, ok := FindSection(secs, AttSettings)
	if !ok {
		return Settings{}, fmt.Errorf("km003c: reply has no SETTINGS section")
	}
	return ParseSettings(s.Payload)
}

// connect opens a CONNECT session unless one is known to be open. After an
// idle gap the meter may have ended the session (5 s after the last command on
// firmware 1.9.5), so it is opened again. A CONNECT sent while the session is
// in fact still open only lengthens the meter's session timeout, and Close
// ends the session with DISCONNECT either way.
func (d *Device) connect(ctx context.Context) error {
	if d.connected && !d.idle() {
		return nil
	}
	d.connected = false
	var err error
	for range 3 {
		h := ControlHeader(TypeConnect, d.nextID(), 0)
		if _, err = d.exchange(ctx, h[:], TypeAccept); err == nil {
			d.connected = true
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("km003c: CONNECT: %w", err)
}

// maxMemoryRead bounds a single MemoryRead; the information blocks are 64
// bytes and other sizes are refused by the whitelist anyway.
const maxMemoryRead = 256

func (d *Device) memoryRead(ctx context.Context, addr, length uint32) (out []byte, err error) {
	if length == 0 || length > maxMemoryRead {
		return nil, fmt.Errorf("km003c: MemoryRead length %d out of range", length)
	}
	req, err := MemoryReadRequest(d.nextID(), addr, length)
	if err != nil {
		return nil, err
	}
	// Keep the verified spacing between MemoryReads, also across calls
	// (DetectLayout followed by ReadInfo).
	if wait := d.memSpacing - time.Since(d.lastMem); !d.lastMem.IsZero() && wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	if err := d.prepare(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	defer func() {
		if err != nil {
			d.dirty = true // confirmation, data or ZLP may still be queued
		}
	}()
	d.lastMem = time.Now()
	if err := d.send(ctx, req); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	var conf MemoryReadConfirm
	for {
		frame, err := d.read(ctx, buf)
		if err != nil {
			return nil, err
		}
		h, err := ParseHeader(frame)
		if err != nil || h.ID != req[1] || h.Type == TypeDisconnect {
			continue // ZLP or stale frame
		}
		if conf, err = ParseMemoryReadConfirm(frame); err != nil {
			return nil, err
		}
		break
	}
	if conf.Addr != addr || conf.Length != length {
		return nil, fmt.Errorf("km003c: MemoryRead confirmation for 0x%08X/%d does not match request 0x%08X/%d",
			conf.Addr, conf.Length, addr, length)
	}
	want := (int(length) + 15) &^ 15
	data := make([]byte, 0, want)
	for len(data) < want {
		// Read exactly what is still missing so a full 64-byte packet
		// completes the transfer without waiting for a short packet.
		n, err := d.t.Read(ctx, buf[:want-len(data)])
		if err != nil && !errors.Is(err, ErrIncomplete) {
			return nil, fmt.Errorf("km003c: MemoryRead data: %w", err)
		}
		if d.Trace != nil {
			d.Trace("rx(data)", buf[:n])
		}
		data = append(data, buf[:n]...)
	}
	// The data ends on a 64-byte boundary, so the device terminates it with
	// a zero-length packet. It must be consumed before the next request:
	// with the ZLP still queued, the meter never answers the following
	// request, its USB stack stalls and the watchdog reboots it (observed on
	// firmware 1.9.5).
	if !d.consumeZLP() {
		d.dirty = true
	}
	return DecodeMemoryData(conf, data)
}

// consumeZLP reads the zero-length packet that follows a reply whose length
// is a multiple of the 64-byte packet size. It reports whether the endpoint
// is known to be empty afterwards.
func (d *Device) consumeZLP() bool {
	buf := make([]byte, 4096)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		n, err := d.t.Read(ctx, buf)
		cancel()
		if err != nil && !errors.Is(err, ErrIncomplete) {
			return true // nothing pending
		}
		if d.Trace != nil {
			d.Trace("rx(zlp)", buf[:n])
		}
		if n == 0 && err == nil {
			return true
		}
	}
	return false
}

// ReadInfo reads model, firmware version and serial number. It sends CONNECT
// once per session, then three MemoryReads of fixed 64-byte information
// blocks. A block that cannot be read does not discard the others: the
// fields of every block that was read are returned together with an error
// that joins one error per failed block.
func (d *Device) ReadInfo(ctx context.Context) (info DeviceInfo, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	if err := d.connect(ctx); err != nil {
		return info, err
	}
	blocks := []struct {
		addr  uint32
		parse func([]byte, *DeviceInfo)
	}{
		{addrInfo, ParseInfoBlock},
		{addrFirmware, ParseFirmwareBlock},
		{addrOTP, ParseOTPBlock},
	}
	var errs []error
	for _, b := range blocks {
		data, err := d.memoryRead(ctx, b.addr, 64)
		if ctx.Err() != nil {
			return info, ctx.Err()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("read block 0x%08X: %w", b.addr, err))
			continue
		}
		b.parse(data, &info)
	}
	if info.FirmwareVersion != "" {
		d.setFirmware(info.FirmwareVersion)
	}
	return info, errors.Join(errs...)
}

func (d *Device) setFirmware(v string) {
	d.firmware = v
	d.layout = TailLayoutFor(v)
}

// DetectLayout reads the application firmware version (CONNECT plus one
// MemoryRead) and selects the matching ADC tail layout for subsequent reads.
// It returns the firmware version. Without it, the firmware-dependent ADC
// tail bytes are left undecoded.
func (d *Device) DetectLayout(ctx context.Context) (v string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.markDirtyOn(&err)
	if d.firmware != "" {
		return d.firmware, nil
	}
	if err := d.connect(ctx); err != nil {
		return "", err
	}
	data, err := d.memoryRead(ctx, addrFirmware, 64)
	if err != nil {
		return "", fmt.Errorf("read firmware block: %w", err)
	}
	var info DeviceInfo
	ParseFirmwareBlock(data, &info)
	if info.FirmwareVersion == "" {
		return "", fmt.Errorf("km003c: firmware block carries no version")
	}
	d.setFirmware(info.FirmwareVersion)
	return d.firmware, nil
}

// FirmwareVersion returns the version learned by ReadInfo or DetectLayout,
// or "" if neither has run.
func (d *Device) FirmwareVersion() string { return d.firmware }

// Layout returns the ADC tail layout in use.
func (d *Device) Layout() TailLayout { return d.layout }
