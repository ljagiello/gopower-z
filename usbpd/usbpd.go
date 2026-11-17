// Package usbpd decodes USB Power Delivery message headers, power data objects
// (PDOs) and request data objects (RDOs) as defined by the USB PD 3.1 spec.
package usbpd

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Header is the 16-bit USB PD message header.
type Header uint16

// Type is the 5-bit message type (meaning depends on NumObjects and Extended).
func (h Header) Type() int { return int(h & 0x1F) }

// PortDataRole is 1 for DFP, 0 for UFP (SOP only).
func (h Header) PortDataRole() int { return int(h>>5) & 1 }

// SpecRevision is 0=1.0, 1=2.0, 2=3.x.
func (h Header) SpecRevision() int { return int(h>>6) & 3 }

// PortPowerRole is 1 for Source, 0 for Sink (SOP only; cable plug for SOP'/SOP”).
func (h Header) PortPowerRole() int { return int(h>>8) & 1 }

// MessageID is the 3-bit rolling message counter.
func (h Header) MessageID() int { return int(h>>9) & 7 }

// NumObjects is the number of 32-bit data objects that follow.
func (h Header) NumObjects() int { return int(h>>12) & 7 }

// Extended reports whether this is an extended message.
func (h Header) Extended() bool { return h&0x8000 != 0 }

// IsControl reports a control message (no data objects, not extended).
func (h Header) IsControl() bool { return !h.Extended() && h.NumObjects() == 0 }

var controlNames = map[int]string{
	1: "GoodCRC", 2: "GotoMin", 3: "Accept", 4: "Reject", 5: "Ping", 6: "PS_RDY",
	7: "Get_Source_Cap", 8: "Get_Sink_Cap", 9: "DR_Swap", 10: "PR_Swap",
	11: "VCONN_Swap", 12: "Wait", 13: "Soft_Reset", 14: "Data_Reset",
	15: "Data_Reset_Complete", 16: "Not_Supported", 17: "Get_Source_Cap_Extended",
	18: "Get_Status", 19: "FR_Swap", 20: "Get_PPS_Status", 21: "Get_Country_Codes",
	22: "Get_Sink_Cap_Extended", 23: "Get_Source_Info", 24: "Get_Revision",
}

var dataNames = map[int]string{
	1: "Source_Capabilities", 2: "Request", 3: "BIST", 4: "Sink_Capabilities",
	5: "Battery_Status", 6: "Alert", 7: "Get_Country_Info", 8: "Enter_USB",
	9: "EPR_Request", 10: "EPR_Mode", 11: "Source_Info", 12: "Revision",
	15: "Vendor_Defined",
}

var extendedNames = map[int]string{
	1: "Source_Capabilities_Extended", 2: "Status", 3: "Get_Battery_Cap",
	4: "Get_Battery_Status", 5: "Battery_Capabilities", 6: "Get_Manufacturer_Info",
	7: "Manufacturer_Info", 8: "Security_Request", 9: "Security_Response",
	10: "Firmware_Update_Request", 11: "Firmware_Update_Response", 12: "PPS_Status",
	13: "Country_Info", 14: "Country_Codes", 15: "Sink_Capabilities_Extended",
	16: "Extended_Control", 17: "EPR_Source_Capabilities", 18: "EPR_Sink_Capabilities",
	30: "Vendor_Defined_Extended",
}

// Name returns the spec name of the message type.
func (h Header) Name() string {
	var m map[int]string
	kind := "Data"
	switch {
	case h.Extended():
		m, kind = extendedNames, "Extended"
	case h.NumObjects() == 0:
		m, kind = controlNames, "Control"
	default:
		m = dataNames
	}
	if n, ok := m[h.Type()]; ok {
		return n
	}
	return fmt.Sprintf("%s_Reserved_%d", kind, h.Type())
}

// Role returns "SRC" or "SNK" (SOP messages) based on the power-role bit.
func (h Header) Role() string {
	if h.PortPowerRole() == 1 {
		return "SRC"
	}
	return "SNK"
}

// Rev returns the spec revision as text.
func (h Header) Rev() string {
	switch h.SpecRevision() {
	case 0:
		return "1.0"
	case 1:
		return "2.0"
	case 2:
		return "3.x"
	}
	return "?"
}

// Message is a decoded PD message (without CRC).
type Message struct {
	Header  Header
	Objects []uint32 // data objects (non-extended messages)
	Raw     []byte   // header + payload bytes as captured
}

// ParseMessage decodes a header followed by data objects. Extra bytes beyond
// the advertised object count are kept in Raw only.
func ParseMessage(b []byte) (Message, error) {
	if len(b) < 2 {
		return Message{}, fmt.Errorf("usbpd: message too short")
	}
	h := Header(binary.LittleEndian.Uint16(b))
	m := Message{Header: h, Raw: append([]byte(nil), b...)}
	if !h.Extended() {
		n := h.NumObjects()
		for i := 0; i < n && 2+4*i+4 <= len(b); i++ {
			m.Objects = append(m.Objects, binary.LittleEndian.Uint32(b[2+4*i:]))
		}
	}
	return m, nil
}

// PDOKind classifies a power data object.
type PDOKind int

const (
	PDOFixed PDOKind = iota
	PDOBattery
	PDOVariable
	PDOPPS    // SPR programmable power supply (APDO)
	PDOEPRAVS // EPR adjustable voltage supply (APDO)
	PDOSPRAVS // SPR adjustable voltage supply (APDO)
	PDOUnknown
)

// PDO is a decoded source power data object.
type PDO struct {
	Kind     PDOKind
	Raw      uint32
	MinVolts float64 // equals MaxVolts for fixed supplies
	MaxVolts float64
	MaxAmps  float64 // 0 if specified as power
	MaxWatts float64 // battery / EPR AVS PDP
	// Fixed-supply flags (first PDO carries the capability bits).
	DualRole, USBSuspend, Unconstrained, USBComms, DualRoleData, UnchunkedExt, EPRCapable bool
}

// DecodeSourcePDO decodes one PDO from a Source_Capabilities message.
func DecodeSourcePDO(v uint32) PDO {
	p := PDO{Raw: v}
	switch v >> 30 {
	case 0:
		p.Kind = PDOFixed
		p.MaxVolts = float64((v>>10)&0x3FF) * 0.05
		p.MinVolts = p.MaxVolts
		p.MaxAmps = float64(v&0x3FF) * 0.01
		p.DualRole = v&(1<<29) != 0
		p.USBSuspend = v&(1<<28) != 0
		p.Unconstrained = v&(1<<27) != 0
		p.USBComms = v&(1<<26) != 0
		p.DualRoleData = v&(1<<25) != 0
		p.UnchunkedExt = v&(1<<24) != 0
		p.EPRCapable = v&(1<<23) != 0
	case 1:
		p.Kind = PDOBattery
		p.MaxVolts = float64((v>>20)&0x3FF) * 0.05
		p.MinVolts = float64((v>>10)&0x3FF) * 0.05
		p.MaxWatts = float64(v&0x3FF) * 0.25
	case 2:
		p.Kind = PDOVariable
		p.MaxVolts = float64((v>>20)&0x3FF) * 0.05
		p.MinVolts = float64((v>>10)&0x3FF) * 0.05
		p.MaxAmps = float64(v&0x3FF) * 0.01
	case 3:
		switch (v >> 28) & 3 {
		case 0:
			p.Kind = PDOPPS
			p.MaxVolts = float64((v>>17)&0xFF) * 0.1
			p.MinVolts = float64((v>>8)&0xFF) * 0.1
			p.MaxAmps = float64(v&0x7F) * 0.05
		case 1:
			p.Kind = PDOEPRAVS
			p.MaxVolts = float64((v>>17)&0x1FF) * 0.1
			p.MinVolts = float64((v>>8)&0xFF) * 0.1
			p.MaxWatts = float64(v & 0xFF)
		case 2:
			p.Kind = PDOSPRAVS
			p.MinVolts = 9
			p.MaxVolts = 20
			// Max current at 15 V (bits 19..10) and 20 V (bits 9..0), 10 mA units.
			p.MaxAmps = float64((v>>10)&0x3FF) * 0.01
		default:
			p.Kind = PDOUnknown
		}
	}
	return p
}

func (p PDO) String() string {
	switch p.Kind {
	case PDOFixed:
		return fmt.Sprintf("Fixed %.2fV %.2fA (%.1fW)", p.MaxVolts, p.MaxAmps, p.MaxVolts*p.MaxAmps)
	case PDOBattery:
		return fmt.Sprintf("Battery %.2f-%.2fV %.1fW", p.MinVolts, p.MaxVolts, p.MaxWatts)
	case PDOVariable:
		return fmt.Sprintf("Variable %.2f-%.2fV %.2fA", p.MinVolts, p.MaxVolts, p.MaxAmps)
	case PDOPPS:
		return fmt.Sprintf("PPS %.1f-%.1fV %.2fA", p.MinVolts, p.MaxVolts, p.MaxAmps)
	case PDOEPRAVS:
		return fmt.Sprintf("EPR AVS %.1f-%.1fV %.0fW", p.MinVolts, p.MaxVolts, p.MaxWatts)
	case PDOSPRAVS:
		v := p.Raw
		return fmt.Sprintf("SPR AVS 9-20V %.2fA@15V %.2fA@20V", float64((v>>10)&0x3FF)*0.01, float64(v&0x3FF)*0.01)
	}
	return fmt.Sprintf("PDO 0x%08X", p.Raw)
}

// RDO is a decoded request data object. Its interpretation depends on the PDO
// it references, so DecodeRDO takes the referenced PDO when known.
type RDO struct {
	Raw         uint32
	Position    int // 1-based index into the source capabilities
	CapMismatch bool
	EPRMode     bool
	Volts       float64 // PPS/AVS requested output voltage (0 for fixed)
	OpAmps      float64
	MaxAmps     float64 // fixed/variable max operating current
	OpWatts     float64 // battery
}

// DecodeRDO decodes a Request data object. ref is the PDO at the requested
// position, or nil if unknown (decoded as fixed/variable).
func DecodeRDO(v uint32, ref *PDO) RDO {
	r := RDO{
		Raw:         v,
		Position:    int(v>>28) & 0xF,
		CapMismatch: v&(1<<26) != 0,
		EPRMode:     v&(1<<22) != 0,
	}
	kind := PDOFixed
	if ref != nil {
		kind = ref.Kind
	}
	switch kind {
	case PDOPPS:
		r.Volts = float64((v>>9)&0xFFF) * 0.02
		r.OpAmps = float64(v&0x7F) * 0.05
	case PDOEPRAVS, PDOSPRAVS:
		r.Volts = float64((v>>9)&0xFFF) * 0.025
		r.OpAmps = float64(v&0x7F) * 0.05
	case PDOBattery:
		r.OpWatts = float64((v>>10)&0x3FF) * 0.25
	default:
		r.OpAmps = float64((v>>10)&0x3FF) * 0.01
		r.MaxAmps = float64(v&0x3FF) * 0.01
	}
	return r
}

func (r RDO) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PDO#%d", r.Position)
	switch {
	case r.Volts > 0:
		fmt.Fprintf(&b, " %.2fV %.2fA", r.Volts, r.OpAmps)
	case r.OpWatts > 0:
		fmt.Fprintf(&b, " %.1fW", r.OpWatts)
	default:
		fmt.Fprintf(&b, " op %.2fA max %.2fA", r.OpAmps, r.MaxAmps)
	}
	if r.CapMismatch {
		b.WriteString(" (cap mismatch)")
	}
	if r.EPRMode {
		b.WriteString(" EPR")
	}
	return b.String()
}
