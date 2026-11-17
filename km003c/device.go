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
type Device struct {
	t         Transport
	mu        sync.Mutex
	id        uint8
	connected bool
	// dirty is set when an exchange failed after its request was written,
	// so a late reply (or ZLP) may still be queued on the IN endpoint. The
	// meter stalls - and its watchdog reboots it - if it receives a new
	// request in that state, so the endpoint is drained before the next
	// write.
	dirty bool

	// Timeout bounds each request/reply exchange.
	Timeout time.Duration
	// Trace, if set, receives every frame sent ("tx") and received ("rx").
	Trace func(dir string, frame []byte)
}

// NewDevice wraps an open transport. It drains stale replies left in the IN
// endpoint by a previous session.
func NewDevice(t Transport) *Device {
	d := &Device{t: t, Timeout: time.Second}
	d.drain()
	return d
}

// Transport returns the underlying transport.
func (d *Device) Transport() Transport { return d.t }

// Close closes the transport.
func (d *Device) Close() error { return d.t.Close() }

// drain discards everything queued on the IN endpoint (stale replies and
// zero-length packets) until a read times out.
func (d *Device) drain() {
	buf := make([]byte, 4096)
	for range 16 {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		n, err := d.t.Read(ctx, buf)
		cancel()
		if err != nil {
			return
		}
		if d.Trace != nil {
			d.Trace("rx(stale)", buf[:n])
		}
	}
}

func (d *Device) nextID() uint8 {
	d.id++
	return d.id
}

func (d *Device) write(ctx context.Context, frame []byte) error {
	if err := CheckRequest(frame, d.t.Kind()); err != nil {
		return err
	}
	if d.dirty {
		d.drain()
		d.dirty = false
	}
	if d.Trace != nil {
		d.Trace("tx", frame)
	}
	out := frame
	if d.t.Kind() == HID {
		// HID output reports are always 64 bytes, zero padded.
		out = make([]byte, 64)
		copy(out, frame)
	}
	return d.t.Write(ctx, out)
}

func (d *Device) read(ctx context.Context, buf []byte) ([]byte, error) {
	n, err := d.t.Read(ctx, buf)
	if err != nil {
		return nil, err
	}
	frame := buf[:n]
	if d.Trace != nil {
		d.Trace("rx", frame)
	}
	return frame, nil
}

// exchange sends a request and waits for a reply of one of the wanted types.
// Stale DISCONNECT frames are skipped.
func (d *Device) exchange(ctx context.Context, req []byte, want ...MsgType) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	if err := d.write(ctx, req); err != nil {
		// A cancelled write may still have reached the device.
		d.dirty = true
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		frame, err := d.read(ctx, buf)
		if err != nil {
			d.dirty = true
			return nil, err
		}
		h, err := ParseHeader(frame)
		if err != nil {
			continue // zero-length packet
		}
		if slices.Contains(want, h.Type) {
			return append([]byte(nil), frame...), nil
		}
		switch h.Type {
		case TypeDisconnect:
			continue // stale or "no data"; keep waiting
		case TypeReject:
			return nil, fmt.Errorf("%w: request % X", ErrRejected, req[:4])
		}
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
			if errors.Is(err, ErrTimeout) {
				continue
			}
			return nil, err
		}
		if d.t.Kind() == HID && len(frame) > 64 {
			frame = frame[:64]
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

// ReadADC reads one ADC snapshot.
func (d *Device) ReadADC(ctx context.Context) (ADC, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	secs, err := d.getData(ctx, AttADC)
	if err != nil {
		return ADC{}, err
	}
	s, ok := FindSection(secs, AttADC)
	if !ok {
		return ADC{}, fmt.Errorf("km003c: reply has no ADC section")
	}
	return ParseADC(s.Payload)
}

// ReadADCAndPD reads an ADC snapshot together with the pending PD events in a
// single exchange (bulk only). The PD event FIFO on the device is drained.
func (d *Device) ReadADCAndPD(ctx context.Context) (ADC, *PDPacket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	secs, err := d.getData(ctx, AttADC|AttPDPacket)
	if err != nil {
		return ADC{}, nil, err
	}
	s, ok := FindSection(secs, AttADC)
	if !ok {
		return ADC{}, nil, fmt.Errorf("km003c: reply has no ADC section")
	}
	a, err := ParseADC(s.Payload)
	if err != nil {
		return a, nil, err
	}
	if ps, ok := FindSection(secs, AttPDPacket); ok {
		pd, err := ParsePDPacket(ps.Payload)
		if err != nil {
			return a, nil, err
		}
		return a, &pd, nil
	}
	return a, nil, nil
}

// ReadPD reads the pending PD events (bulk only).
func (d *Device) ReadPD(ctx context.Context) (PDPacket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
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

// ReadSettings reads the device settings block (bulk only).
func (d *Device) ReadSettings(ctx context.Context) (Settings, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
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

func (d *Device) connect(ctx context.Context) error {
	if d.connected {
		return nil
	}
	var err error
	for range 3 {
		h := ControlHeader(TypeConnect, d.nextID(), 0)
		_, err = d.exchange(ctx, h[:], TypeAccept)
		if err == nil {
			d.connected = true
			return nil
		}
	}
	return fmt.Errorf("km003c: CONNECT: %w", err)
}

func (d *Device) memoryRead(ctx context.Context, addr, length uint32) (out []byte, err error) {
	req, err := MemoryReadRequest(d.nextID(), addr, length)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	defer func() {
		if err != nil {
			d.dirty = true // confirmation, data or ZLP may still be queued
		}
	}()
	if err := d.write(ctx, req); err != nil {
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
		if err != nil || h.Type == TypeDisconnect {
			continue
		}
		if conf, err = ParseMemoryReadConfirm(frame); err != nil {
			return nil, err
		}
		break
	}
	want := (int(conf.Length) + 15) &^ 15
	data := make([]byte, 0, want)
	for len(data) < want {
		// Read exactly what is still missing so a full 64-byte packet
		// completes the transfer without waiting for a short packet.
		n, err := d.t.Read(ctx, buf[:want-len(data)])
		if err != nil {
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
	// MemoryRead, its USB stack stalls and the watchdog reboots it
	// (observed on firmware 1.9.5).
	d.consumeZLP()
	return DecodeMemoryData(conf, data)
}

// consumeZLP reads the zero-length packet that follows a reply whose length
// is a multiple of the 64-byte packet size.
func (d *Device) consumeZLP() {
	buf := make([]byte, 4096)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		n, err := d.t.Read(ctx, buf)
		cancel()
		if err != nil {
			return // nothing pending
		}
		if d.Trace != nil {
			d.Trace("rx(zlp)", buf[:n])
		}
		if n == 0 {
			return
		}
	}
}

// ReadInfo reads model, firmware version and serial number (bulk only). It
// sends CONNECT once per session, then three MemoryReads of fixed 64-byte
// information blocks.
func (d *Device) ReadInfo(ctx context.Context) (DeviceInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var info DeviceInfo
	if d.t.Kind() != Bulk {
		return info, fmt.Errorf("km003c: device info is only available over the bulk interface")
	}
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
	for i, b := range blocks {
		if i > 0 {
			// Same spacing as the verified probe sequence.
			time.Sleep(300 * time.Millisecond)
		}
		data, err := d.memoryRead(ctx, b.addr, 64)
		if err != nil {
			return info, fmt.Errorf("read block 0x%08X: %w", b.addr, err)
		}
		b.parse(data, &info)
	}
	return info, nil
}
