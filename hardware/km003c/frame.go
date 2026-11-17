// Package km003c implements the USB protocol of the ChargerLAB POWER-Z KM003C
// USB-C power meter (USB 5FC9:0063).
//
// The protocol description lives in docs/protocol.html. Only read-only requests
// are implemented; see guard.go for the whitelist that every outgoing frame is
// checked against.
package km003c

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// USB identity.
const (
	VendorID  = 0x5FC9
	ProductID = 0x0063
)

// MsgType is the 7-bit message type in the low bits of every frame header.
// Values below 0x40 are control messages; 0x40 and above are data messages.
type MsgType uint8

// Message types used by this package. Types that change device state are
// deliberately not given names here.
const (
	TypeConnect     MsgType = 0x02
	TypeDisconnect  MsgType = 0x03
	TypeAccept      MsgType = 0x05
	TypeReject      MsgType = 0x06
	TypeFinished    MsgType = 0x07
	TypeGetData     MsgType = 0x0C
	TypeNotReadable MsgType = 0x27
	TypePutData     MsgType = 0x41
	TypeMemoryRead  MsgType = 0x44
)

func (t MsgType) String() string {
	switch t {
	case TypeConnect:
		return "CONNECT"
	case TypeDisconnect:
		return "DISCONNECT"
	case TypeAccept:
		return "ACCEPT"
	case TypeReject:
		return "REJECT"
	case TypeFinished:
		return "FINISHED"
	case TypeGetData:
		return "GET_DATA"
	case TypeNotReadable:
		return "NOT_READABLE"
	case TypePutData:
		return "PUT_DATA"
	case TypeMemoryRead:
		return "MEMORY_READ"
	}
	return fmt.Sprintf("type(0x%02X)", uint8(t))
}

// Attribute selects a data class in GET_DATA requests and identifies the
// sections of a PUT_DATA reply. Attributes are single bits that can be OR-ed.
type Attribute uint16

// Attributes. Only ADC, Settings and PDPacket are requested by this package.
const (
	AttADC      Attribute = 0x001
	AttADCQueue Attribute = 0x002
	AttSettings Attribute = 0x008
	AttPDPacket Attribute = 0x010
	AttPDStatus Attribute = 0x020
	AttQCPacket Attribute = 0x040
	AttTick     Attribute = 0x080
	AttRecList  Attribute = 0x200
)

func (a Attribute) String() string {
	switch a {
	case AttADC:
		return "ADC"
	case AttADCQueue:
		return "ADC_QUEUE"
	case AttSettings:
		return "SETTINGS"
	case AttPDPacket:
		return "PD_PACKET"
	case AttPDStatus:
		return "PD_STATUS"
	case AttQCPacket:
		return "QC_PACKET"
	case AttTick:
		return "TICK"
	case AttRecList:
		return "RECORD_LIST"
	}
	return fmt.Sprintf("att(0x%03X)", uint16(a))
}

// ControlHeader encodes the 4-byte control header
// word = type | id<<8 | att<<17, little-endian.
func ControlHeader(t MsgType, id uint8, att Attribute) [4]byte {
	w := uint32(t)&0x7F | uint32(id)<<8 | (uint32(att)&0x7FFF)<<17
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], w)
	return b
}

// Header is the decoded first word of a frame.
type Header struct {
	Type   MsgType // bits 0-6
	Extend bool    // bit 7
	ID     uint8   // bits 8-15
	// Word holds bits 16-31 verbatim. For control frames bit 0 of Word is the
	// encrypted flag and bits 1-15 the attribute. For PUT_DATA bit 0 is the
	// encrypted flag, bits 1-2 the session auth level and bits 6-15 an object
	// count whose meaning differs between firmware versions.
	Word uint16
}

// Encrypted reports bit 16 of the header.
func (h Header) Encrypted() bool { return h.Word&1 != 0 }

// ParseHeader decodes the first 4 bytes of a frame.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < 4 {
		return Header{}, fmt.Errorf("km003c: frame too short (%d bytes)", len(b))
	}
	return Header{
		Type:   MsgType(b[0] & 0x7F),
		Extend: b[0]&0x80 != 0,
		ID:     b[1],
		Word:   binary.LittleEndian.Uint16(b[2:4]),
	}, nil
}

// Section is one logical block of a PUT_DATA reply.
type Section struct {
	Att     Attribute
	Next    bool // another section follows
	Chunk   int  // record count (ADC_QUEUE)
	Size    int  // payload or record size from the header
	Payload []byte
}

// ErrEmpty is returned for a PUT_DATA frame without sections (the device had
// nothing to report).
var ErrEmpty = errors.New("km003c: empty PUT_DATA")

// ParsePutData splits a PUT_DATA frame into its sections.
func ParsePutData(frame []byte) (Header, []Section, error) {
	h, err := ParseHeader(frame)
	if err != nil {
		return h, nil, err
	}
	if h.Type != TypePutData {
		return h, nil, fmt.Errorf("km003c: expected PUT_DATA, got %v", h.Type)
	}
	rest := frame[4:]
	if len(rest) == 0 {
		return h, nil, ErrEmpty
	}
	var out []Section
	for len(rest) >= 4 {
		w := binary.LittleEndian.Uint32(rest[:4])
		s := Section{
			Att:   Attribute(w & 0x7FFF),
			Next:  w&(1<<15) != 0,
			Chunk: int(w>>16) & 0x3F,
			Size:  int(w >> 22),
		}
		body := rest[4:]
		n := s.Size
		switch s.Att {
		case AttADCQueue:
			n = s.Chunk * s.Size
		case AttPDStatus, AttRecList:
			// The size field is unusable for these; the payload is the
			// rest of the frame.
			n = len(body)
		}
		if n > len(body) {
			return h, out, fmt.Errorf("km003c: section %v claims %d bytes, only %d left", s.Att, n, len(body))
		}
		s.Payload = body[:n]
		out = append(out, s)
		rest = body[n:]
		if !s.Next {
			break
		}
	}
	return h, out, nil
}

// FindSection returns the first section with the given attribute.
func FindSection(sections []Section, att Attribute) (Section, bool) {
	for _, s := range sections {
		if s.Att == att {
			return s, true
		}
	}
	return Section{}, false
}
