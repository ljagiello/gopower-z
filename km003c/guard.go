package km003c

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// TransportKind identifies which USB interface a frame travels over. The
// device accepts different subsets of the protocol on each.
type TransportKind int

const (
	// Bulk is interface 0 (vendor class, bulk EP 0x01/0x81). Full protocol.
	Bulk TransportKind = iota
	// HID is interface 3 (interrupt EP 0x05/0x85, 64-byte reports). Replies
	// longer than 64 bytes hang the firmware, so only the ADC read is allowed.
	HID
	// CDC is interface 2 (virtual serial port). The same byte stream also
	// carries text commands that renegotiate VBUS, so only the ADC read is
	// allowed.
	CDC
)

func (k TransportKind) String() string {
	switch k {
	case Bulk:
		return "bulk"
	case HID:
		return "hid"
	case CDC:
		return "cdc"
	}
	return fmt.Sprintf("transport(%d)", int(k))
}

// bulkReads are the exact GET_DATA attribute masks this package may request
// over bulk (protocol.html §12.1 W1-W4). ADC and SETTINGS are pure reads;
// PD_PACKET drains the device's PD event FIFO but changes nothing else.
var bulkReads = map[Attribute]bool{
	AttADC:               true,
	AttSettings:          true,
	AttPDPacket:          true,
	AttADC | AttPDPacket: true,
}

// memoryWhitelist lists the only MemoryRead regions that may be requested.
// Reads at other addresses are reported to crash the meter.
var memoryWhitelist = map[uint32]uint32{
	addrInfo:     64,
	addrFirmware: 64,
	addrOTP:      64,
}

// CheckRequest returns an error unless frame is one of the read-only requests
// this package is permitted to send on transport k. Every frame passes through
// this check immediately before it is written to the device.
func CheckRequest(frame []byte, k TransportKind) error {
	h, err := ParseHeader(frame)
	if err != nil {
		return err
	}
	if h.Extend {
		return fmt.Errorf("km003c: refusing frame with extend bit set")
	}
	switch h.Type {
	case TypeGetData:
		if len(frame) != 4 {
			return fmt.Errorf("km003c: GET_DATA must be 4 bytes, got %d", len(frame))
		}
		if h.Word&1 != 0 {
			return fmt.Errorf("km003c: refusing GET_DATA with encrypted flag")
		}
		att := Attribute(h.Word >> 1)
		if att == 0 {
			return fmt.Errorf("km003c: GET_DATA without attribute")
		}
		if k == HID || k == CDC {
			// The vendor sample limits HID and CDC sections to <64 bytes;
			// only the 52-byte ADC reply is known to be safe there.
			if att != AttADC {
				return fmt.Errorf("km003c: only the ADC read is safe over %v, refusing %v", k, att)
			}
			return nil
		}
		if !bulkReads[att] {
			return fmt.Errorf("km003c: attribute mask 0x%03X not in read whitelist", uint16(att))
		}
		return nil

	case TypeConnect, TypeDisconnect:
		if k != Bulk {
			// On CDC (firmware 2.0.x) these bytes start/stop a push
			// stream; over HID they are ignored.
			return fmt.Errorf("km003c: %v is only allowed on bulk", h.Type)
		}
		if len(frame) != 4 || h.Word != 0 {
			return fmt.Errorf("km003c: malformed %v", h.Type)
		}
		return nil

	case TypeMemoryRead:
		if k != Bulk {
			return fmt.Errorf("km003c: MemoryRead is only allowed on bulk")
		}
		if len(frame) != 36 || frame[2] != 0x01 || frame[3] != 0x01 {
			return fmt.Errorf("km003c: malformed MemoryRead")
		}
		p, err := decryptECB(key0, frame[4:])
		if err != nil {
			return err
		}
		addr := binary.LittleEndian.Uint32(p[0:4])
		n := binary.LittleEndian.Uint32(p[4:8])
		if want, ok := memoryWhitelist[addr]; !ok || n != want {
			return fmt.Errorf("km003c: MemoryRead of 0x%08X/%d not in whitelist", addr, n)
		}
		// The whole frame (magic, CRC, padding) must be exactly what this
		// package encodes for that block.
		canonical, err := MemoryReadRequest(frame[1], addr, n)
		if err != nil {
			return err
		}
		if !bytes.Equal(frame, canonical) {
			return fmt.Errorf("km003c: MemoryRead of 0x%08X/%d is not canonically encoded", addr, n)
		}
		return nil
	}
	return fmt.Errorf("km003c: message type 0x%02X is not in the read-only whitelist", uint8(h.Type))
}
