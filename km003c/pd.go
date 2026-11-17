package km003c

import (
	"encoding/binary"
	"fmt"

	"github.com/ljagiello/gopower-z/usbpd"
)

// PDStatus is the 12-byte block that starts every PD_PACKET section.
type PDStatus struct {
	TimestampMS uint32
	VBusMV      uint16
	IBusMA      int16
	CC1MV       uint16
	CC2MV       uint16
}

// SOP packet type of a captured PD message.
type SOP uint8

func (s SOP) String() string {
	switch s {
	case 0:
		return "SOP"
	case 1:
		return "SOP'"
	case 2:
		return "SOP''"
	case 3:
		return "SOP'_Dbg"
	case 4:
		return "SOP''_Dbg"
	}
	return fmt.Sprintf("SOP?%d", uint8(s))
}

// PDEventCode is the low nibble of an event record's code byte.
type PDEventCode uint8

func (c PDEventCode) String() string {
	switch c {
	case 0:
		return "none"
	case 1:
		return "attach"
	case 2:
		return "detach"
	case 3:
		return "bad message id"
	case 4:
		return "hard reset"
	case 5:
		return "cable reset"
	case 6:
		return "receive error"
	case 7:
		return "monitor type"
	case 8:
		return "big message"
	}
	return fmt.Sprintf("event %d", uint8(c))
}

// PDRecord is one entry of the PD event stream: either a captured PD message
// or a connection event.
type PDRecord struct {
	TimestampMS uint32
	IsMessage   bool
	FromMeter   bool // bit 6 of the flag byte: the meter itself sent this message
	SOP         SOP
	Message     usbpd.Message // valid when IsMessage
	Event       PDEventCode   // valid when !IsMessage
	EventParam  uint8         // high nibble (e.g. CC line for attach)
	Raw         []byte
}

// PDPacket is a decoded PD_PACKET section.
type PDPacket struct {
	Status  PDStatus
	Records []PDRecord
	// Trailing holds bytes that could not be parsed as records.
	Trailing []byte
}

// ParsePDPacket decodes a PD_PACKET (attribute 0x010) section payload.
func ParsePDPacket(p []byte) (PDPacket, error) {
	if len(p) < 12 {
		return PDPacket{}, fmt.Errorf("km003c: PD payload too short (%d bytes)", len(p))
	}
	le := binary.LittleEndian
	out := PDPacket{Status: PDStatus{
		TimestampMS: le.Uint32(p[0:]),
		VBusMV:      le.Uint16(p[4:]),
		IBusMA:      int16(le.Uint16(p[6:])),
		CC1MV:       le.Uint16(p[8:]),
		CC2MV:       le.Uint16(p[10:]),
	}}
	rest := p[12:]
	for len(rest) > 0 {
		b0 := rest[0]
		var n int
		switch {
		case b0&0x80 != 0:
			n = int(b0&0x3F) + 1
		case b0&0x20 != 0:
			if len(rest) < 8 {
				out.Trailing = rest
				return out, nil
			}
			n = int(b0&0x1F) + 1 + int(le.Uint16(rest[6:]))
		default:
			n = int(b0&0x1F) + 1
		}
		if n < 6 || n > len(rest) {
			out.Trailing = rest
			return out, nil
		}
		rec := rest[:n]
		r := PDRecord{
			TimestampMS: le.Uint32(rec[1:]),
			FromMeter:   b0&0x40 != 0,
			Raw:         append([]byte(nil), rec...),
		}
		if b0&0x80 != 0 {
			r.IsMessage = true
			r.SOP = SOP(rec[5] & 0x07)
			if len(rec) >= 8 {
				m, err := usbpd.ParseMessage(rec[6:])
				if err == nil {
					r.Message = m
				}
			}
		} else {
			r.Event = PDEventCode(rec[5] & 0x0F)
			r.EventParam = rec[5] >> 4
		}
		out.Records = append(out.Records, r)
		rest = rest[n:]
	}
	return out, nil
}

// PDRequest builds a GET_DATA request for the PD event stream.
func PDRequest(id uint8) []byte {
	h := ControlHeader(TypeGetData, id, AttPDPacket)
	return h[:]
}

// ADCAndPDRequest requests an ADC snapshot and the PD stream in one frame.
func ADCAndPDRequest(id uint8) []byte {
	h := ControlHeader(TypeGetData, id, AttADC|AttPDPacket)
	return h[:]
}
