package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/ljagiello/gopower-z/usbpd"
)

// pdo is the JSON form of a source power data object.
type pdo struct {
	Index       int      `json:"index"`
	Type        string   `json:"type"` // fixed, battery, variable, pps, epr_avs, spr_avs, unknown
	MinVoltageV float64  `json:"min_voltage_v"`
	MaxVoltageV float64  `json:"max_voltage_v"`
	MaxCurrentA *float64 `json:"max_current_a"` // SPR AVS: at 15 V; null when specified as power
	// MaxCurrent20VA is the SPR AVS maximum current at 20 V.
	MaxCurrent20VA *float64 `json:"max_current_20v_a"`
	MaxPowerW      float64  `json:"max_power_w"`
	RawHex         string   `json:"raw_hex"`
	Text           string   `json:"text"`
}

func nzPtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return new(v)
}

func toPDO(i int, p usbpd.PDO) pdo {
	t := map[usbpd.PDOKind]string{
		usbpd.PDOFixed: "fixed", usbpd.PDOBattery: "battery", usbpd.PDOVariable: "variable",
		usbpd.PDOPPS: "pps", usbpd.PDOEPRAVS: "epr_avs", usbpd.PDOSPRAVS: "spr_avs",
	}[p.Kind]
	if t == "" {
		t = "unknown"
	}
	out := pdo{
		Index: i, Type: t, MinVoltageV: round(p.MinVolts, 3), MaxVoltageV: round(p.MaxVolts, 3),
		MaxCurrentA: nzPtr(round(p.MaxAmps, 3)), MaxCurrent20VA: nzPtr(round(p.MaxAmps2, 3)), MaxPowerW: round(p.MaxWatts, 3),
		RawHex: fmt.Sprintf("%08X", p.Raw), Text: p.String(),
	}
	if out.MaxPowerW == 0 && p.MaxAmps > 0 {
		out.MaxPowerW = round(p.MaxVolts*p.MaxAmps, 3)
	}
	return out
}

// rdo is the JSON form of a sink request.
type rdo struct {
	ObjectPosition     int      `json:"object_position"`
	VoltageV           *float64 `json:"voltage_v"` // PPS / AVS requests
	OperatingCurrentA  *float64 `json:"operating_current_a"`
	MaxCurrentA        *float64 `json:"max_current_a"`
	OperatingPowerW    *float64 `json:"operating_power_w"`
	CapabilityMismatch bool     `json:"capability_mismatch"`
	EPR                bool     `json:"epr"`
	PDO                *pdo     `json:"pdo"` // the source PDO it selects, if known
	RawHex             string   `json:"raw_hex"`
	Text               string   `json:"text"`
}

// pdRecord is the JSON form of one captured PD message or event, or of PD
// bytes the meter delivered that could not be parsed.
type pdRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Time          string `json:"time"` // host time the record was read
	MeterTimeMS   uint32 `json:"meter_time_ms"`
	Kind          string `json:"kind"` // "message", "event" or "undecoded"
	// message fields
	SOP          string   `json:"sop,omitempty"`
	Message      string   `json:"message,omitempty"`
	Class        string   `json:"class,omitempty"`      // control, data, extended
	PowerRole    string   `json:"power_role,omitempty"` // source or sink (SOP only)
	DataRole     string   `json:"data_role,omitempty"`  // dfp or ufp (SOP only)
	SpecRevision string   `json:"spec_revision,omitempty"`
	MessageID    *int     `json:"message_id,omitempty"`
	FromMeter    bool     `json:"from_meter,omitempty"`
	ObjectsHex   []string `json:"objects_hex,omitempty"`
	SourceCaps   []pdo    `json:"source_capabilities,omitempty"`
	Request      *rdo     `json:"request,omitempty"`
	// event fields
	Event  string `json:"event,omitempty"`
	CC     int    `json:"cc,omitempty"`
	Param  int    `json:"param,omitempty"`
	RawHex string `json:"raw_hex"`
}

// MarshalJSON emits every key for every record (null where a field does not
// apply to the record's kind) so consumers see one fixed shape.
func (r pdRecord) MarshalJSON() ([]byte, error) {
	type record struct {
		SchemaVersion int      `json:"schema_version"`
		Time          string   `json:"time"`
		MeterTimeMS   uint32   `json:"meter_time_ms"`
		Kind          string   `json:"kind"`
		SOP           *string  `json:"sop"`
		Message       *string  `json:"message"`
		Class         *string  `json:"class"`
		PowerRole     *string  `json:"power_role"`
		DataRole      *string  `json:"data_role"`
		SpecRevision  *string  `json:"spec_revision"`
		MessageID     *int     `json:"message_id"`
		FromMeter     bool     `json:"from_meter"`
		ObjectsHex    []string `json:"objects_hex"`
		SourceCaps    []pdo    `json:"source_capabilities"`
		Request       *rdo     `json:"request"`
		Event         *string  `json:"event"`
		CCLine        *int     `json:"cc_line"`
		Param         *int     `json:"param"`
		RawHex        string   `json:"raw_hex"`
	}
	o := record{
		SchemaVersion: r.SchemaVersion, Time: r.Time, MeterTimeMS: r.MeterTimeMS, Kind: r.Kind,
		SOP: strOrNil(r.SOP), Message: strOrNil(r.Message), Class: strOrNil(r.Class),
		PowerRole: strOrNil(r.PowerRole), DataRole: strOrNil(r.DataRole), SpecRevision: strOrNil(r.SpecRevision),
		MessageID: r.MessageID, FromMeter: r.FromMeter, ObjectsHex: r.ObjectsHex, SourceCaps: r.SourceCaps,
		Request: r.Request, RawHex: r.RawHex,
	}
	if o.ObjectsHex == nil {
		o.ObjectsHex = []string{}
	}
	if r.Kind == "event" {
		o.Event = strOrNil(r.Event)
		if r.CC != 0 {
			o.CCLine = new(r.CC)
		} else {
			o.Param = new(r.Param)
		}
	}
	return json.Marshal(o)
}

// sopName names a record's SOP packet type; the reserved values 5-7 are
// "unknown".
func sopName(s km003c.SOP) string {
	if s > 4 {
		return "unknown"
	}
	return s.String()
}

// pdTracker decodes PD records and follows the negotiation: the source's
// capabilities, the sink's Request, the source's Accept and finally PS_RDY,
// which makes the accepted Request the active contract.
type pdTracker struct {
	showGoodCRC bool
	srcCaps     []usbpd.PDO
	lastReq     *rdo // last Request seen (whatever its outcome)
	pending     *rdo // Request awaiting Accept
	accepted    *rdo // Request accepted, awaiting PS_RDY
	contract    *rdo // accepted Request confirmed by PS_RDY
	messages    int
	events      int
	attaches    int // attach/detach events
	undecoded   int
	ext         usbpd.Reassembler // chunked extended messages
}

// reset forgets the negotiation (detach or hard reset: the next source starts
// from scratch).
func (t *pdTracker) reset() {
	t.srcCaps, t.lastReq, t.pending, t.accepted, t.contract = nil, nil, nil, nil, nil
	t.ext = usbpd.Reassembler{}
}

// packet decodes every record of a PD_PACKET section. Bytes the parser could
// not split into records are kept as one "undecoded" record: reading the
// packet drained them from the meter, so this is their only copy.
func (t *pdTracker) packet(p *km003c.PDPacket) []pdRecord {
	if p == nil {
		return nil
	}
	var out []pdRecord
	for _, r := range p.Records {
		if rec, ok := t.process(r); ok {
			out = append(out, rec)
		}
	}
	if len(p.Trailing) > 0 {
		t.undecoded++
		out = append(out, pdRecord{SchemaVersion: schemaVersion, Time: utcTime(time.Now()),
			MeterTimeMS: p.Status.TimestampMS, Kind: "undecoded", RawHex: hexdump(p.Trailing)})
	}
	return out
}

func (t *pdTracker) process(r km003c.PDRecord) (pdRecord, bool) {
	out := pdRecord{SchemaVersion: schemaVersion, Time: utcTime(time.Now()), MeterTimeMS: r.TimestampMS, RawHex: hexdump(r.Raw)}
	if !r.IsMessage {
		t.events++
		if r.Event == 1 || r.Event == 2 {
			t.attaches++
		}
		out.Kind = "event"
		out.Event = r.Event.String()
		if (r.Event == 1 || r.Event == 2) && (r.EventParam == 1 || r.EventParam == 2) {
			out.CC = int(r.EventParam)
		} else {
			out.Param = int(r.EventParam)
		}
		if r.Event == 2 || r.Event == 4 { // detach or hard reset
			t.reset()
		}
		return out, true
	}
	if r.Long {
		// Long message record: layout unverified, report it raw without
		// touching the negotiation state.
		t.messages++
		out.Kind = "message"
		out.SOP = sopName(r.SOP)
		out.Message = "long_message_undecoded"
		out.FromMeter = r.FromMeter
		return out, true
	}
	m := r.Message
	h := m.Header
	if h.IsControl() && h.Type() == 1 && !t.showGoodCRC {
		return out, false
	}
	t.messages++
	id := h.MessageID()
	out.Kind = "message"
	out.SOP = sopName(r.SOP)
	out.Message = h.Name()
	out.SpecRevision = h.Rev()
	out.MessageID = &id
	out.FromMeter = r.FromMeter
	switch {
	case h.Extended():
		out.Class = "extended"
	case h.IsControl():
		out.Class = "control"
	default:
		out.Class = "data"
	}
	if r.SOP == 0 {
		out.PowerRole = map[int]string{0: "sink", 1: "source"}[h.PortPowerRole()]
		out.DataRole = map[int]string{0: "ufp", 1: "dfp"}[h.PortDataRole()]
	}
	for _, o := range m.Objects {
		out.ObjectsHex = append(out.ObjectsHex, fmt.Sprintf("%08X", o))
	}
	if r.SOP != 0 {
		return out, true // cable (SOP'/SOP'') traffic does not negotiate power
	}
	switch out.Message {
	case "Source_Capabilities", "EPR_Source_Capabilities":
		objs := m.Objects
		if h.Extended() {
			// EPR_Source_Capabilities is an extended (usually chunked)
			// message; decode it once all chunks have arrived.
			block, ok := t.ext.Add(m)
			if !ok {
				break
			}
			objs = usbpd.Objects(block)
		}
		if len(objs) == 0 {
			break // keep the previous capabilities
		}
		t.srcCaps = t.srcCaps[:0]
		for i, o := range objs {
			if o == 0 {
				// Unused EPR slot (an SPR source fills positions 1-7
				// and pads with zero before the EPR PDOs).
				t.srcCaps = append(t.srcCaps, usbpd.PDO{Kind: usbpd.PDOUnknown})
				continue
			}
			p := usbpd.DecodeSourcePDO(o)
			t.srcCaps = append(t.srcCaps, p)
			out.SourceCaps = append(out.SourceCaps, toPDO(i+1, p))
		}
	case "Request", "EPR_Request":
		if len(m.Objects) > 0 {
			out.Request = t.decodeRequest(m)
			t.lastReq, t.pending, t.accepted = out.Request, out.Request, nil
		}
	case "Accept":
		if t.pending != nil {
			t.accepted, t.pending = t.pending, nil
		}
	case "Reject", "Wait":
		t.pending = nil
	case "PS_RDY":
		if t.accepted != nil {
			t.contract, t.accepted = t.accepted, nil
		}
	case "Soft_Reset":
		t.pending, t.accepted, t.contract = nil, nil, nil
	}
	return out, true
}

// decodeRequest decodes a Request or EPR_Request. An EPR_Request carries a
// copy of the PDO it selects as its second object, so it is decoded against
// that copy even when the EPR capabilities were not captured.
func (t *pdTracker) decodeRequest(m usbpd.Message) *rdo {
	v := m.Objects[0]
	pos := int(v>>28) & 0xF
	var ref *usbpd.PDO
	switch {
	case m.Header.Name() == "EPR_Request" && len(m.Objects) >= 2:
		p := usbpd.DecodeSourcePDO(m.Objects[1])
		ref = &p
	case pos >= 1 && pos <= len(t.srcCaps) && t.srcCaps[pos-1].Raw != 0:
		ref = &t.srcCaps[pos-1]
	}
	r := usbpd.DecodeRDO(v, ref)
	out := &rdo{
		ObjectPosition: r.Position, VoltageV: nzPtr(round(r.Volts, 3)),
		OperatingCurrentA: nzPtr(round(r.OpAmps, 3)), MaxCurrentA: nzPtr(round(r.MaxAmps, 3)),
		OperatingPowerW: nzPtr(round(r.OpWatts, 3)), CapabilityMismatch: r.CapMismatch, EPR: r.EPRMode,
		RawHex: fmt.Sprintf("%08X", v), Text: r.String(),
	}
	if ref != nil {
		p := toPDO(pos, *ref)
		out.PDO = &p
		out.Text += " -> " + ref.String()
	}
	return out
}

// pdSummary describes a PD capture window (measure's "pd" object and the
// final line of `pd`).
type pdSummary struct {
	Messages           int        `json:"messages"`
	Events             int        `json:"events"`
	Undecoded          int        `json:"undecoded"`
	SourceCapabilities []pdo      `json:"source_capabilities"`
	LastRequest        *rdo       `json:"last_request"`
	Contract           *rdo       `json:"contract"` // Request accepted and confirmed with PS_RDY
	Note               *string    `json:"note"`
	Records            []pdRecord `json:"records"`
}

// Notes explaining a capture without PD messages, so it is not mistaken for
// a failure.
const (
	noPDNote = "no USB-PD activity was captured in this window; this is normal when nothing was connected or " +
		"renegotiated (PD is only exchanged on attach or renegotiation); a contract negotiated before the capture " +
		"started is not visible - reconnect the device during the capture to observe it"
	noPDMessagesNote = "the device was attached/detached during the capture (see events) but no USB-PD messages " +
		"were exchanged: the charger or the device does not use USB-PD (the meter records every PD message it sees)"
)

func (t *pdTracker) summary(records []pdRecord) *pdSummary {
	if records == nil {
		records = []pdRecord{}
	}
	s := &pdSummary{
		Messages: t.messages, Events: t.events, Undecoded: t.undecoded, SourceCapabilities: []pdo{},
		LastRequest: t.lastReq, Contract: t.contract, Records: records,
	}
	for i, p := range t.srcCaps {
		if p.Kind == usbpd.PDOUnknown && p.Raw == 0 {
			continue // padding slot in EPR capabilities
		}
		s.SourceCapabilities = append(s.SourceCapabilities, toPDO(i+1, p))
	}
	switch {
	case t.messages > 0:
	case t.attaches > 0:
		s.Note = new(noPDMessagesNote)
	default:
		s.Note = new(noPDNote)
	}
	return s
}

func (t *pdTracker) capsText() string {
	parts := make([]string, 0, len(t.srcCaps))
	for _, p := range t.srcCaps {
		if p.Kind == usbpd.PDOUnknown && p.Raw == 0 {
			continue
		}
		parts = append(parts, p.String())
	}
	return strings.Join(parts, " | ")
}

// text renders a record as human-readable line(s).
func (r pdRecord) text() string {
	ts := fmt.Sprintf("%10.3fs", float64(r.MeterTimeMS)/1000)
	switch r.Kind {
	case "event":
		s := ts + "  EVENT " + r.Event
		if r.CC != 0 {
			s += fmt.Sprintf(" CC%d", r.CC)
		} else if r.Param != 0 {
			s += fmt.Sprintf(" (param %d)", r.Param)
		}
		return s
	case "undecoded":
		return ts + "  UNDECODED " + r.RawHex
	}
	role := "   "
	switch r.PowerRole {
	case "source":
		role = "SRC"
	case "sink":
		role = "SNK"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-5s %s %-24s", ts, r.SOP, role, r.Message)
	if r.MessageID != nil {
		fmt.Fprintf(&b, " id=%d rev=%s", *r.MessageID, r.SpecRevision)
	}
	if r.FromMeter {
		b.WriteString(" [meter]")
	}
	for _, p := range r.SourceCaps {
		fmt.Fprintf(&b, "\n%14s#%d %s", "", p.Index, p.Text)
	}
	if r.Request != nil {
		fmt.Fprintf(&b, "\n%14s%s", "", r.Request.Text)
	}
	if r.SourceCaps == nil && r.Request == nil {
		for i, o := range r.ObjectsHex {
			fmt.Fprintf(&b, "\n%14sobj%d 0x%s", "", i, o)
		}
	}
	return b.String()
}
