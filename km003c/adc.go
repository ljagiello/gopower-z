package km003c

import (
	"encoding/binary"
	"fmt"
)

// adcCoreSize is the part of the ADC payload whose layout every firmware
// version agrees on. Older firmware sends 40 bytes, newer 44.
const adcCoreSize = 36

// ADC is one snapshot of the meter's analog measurements (attribute 0x001).
// Raw integer fields keep the device units; methods convert to SI.
type ADC struct {
	VBusUV    int32  // VBUS, µV
	IBusUA    int32  // IBUS, µA, raw meter sign: negative = receptacle → plug (see FlowA)
	VBusAvgUV int32  // smoothed VBUS average, µV
	IBusAvgUA int32  // smoothed IBUS average, µA
	VBusRawUV int32  // uncalibrated VBUS average
	IBusRawUA int32  // uncalibrated IBUS average
	TempRaw   int16  // die temperature, 1/128 °C; -32768 = invalid
	CC1, CC2  uint16 // CC line voltages, 0.1 mV
	DP, DM    uint16 // D+ / D- voltages, 0.1 mV
	VDD       uint16 // internal supply, 0.1 mV
	Tail      []byte // payload bytes 0x24.. verbatim

	// Fields below are decoded from Tail by ApplyTail; their layout depends
	// on the firmware version (see TailLayoutFor).
	Layout     TailLayout
	Temp2Raw   int16 // second temperature channel, 1/128 °C; -32768 = absent
	RateIndex  int   // configured sample rate (0=2, 1=10, 2=50, 3=1000 SPS); -1 unknown
	HasLineAvg bool  // CC2/D+/D- averages present (TailLineAverages)
	CC2AvgMV   uint16
	DPAvgMV    uint16
	DMAvgMV    uint16
	HasStatus  bool  // TailStatus fields present
	Accel      uint8 // accelerometer status (low nibble) and orientation (high nibble, 0xF = flat)
	// BatteryPermille is the sink's battery level learned from sniffed PD
	// traffic, in 0.1 % units; 0 when unknown.
	BatteryPermille uint16
}

// TailLayout identifies how ADC payload bytes 0x24..0x2B are laid out. The
// meaning changed between firmware versions (docs/firmware.html).
type TailLayout int

const (
	// TailUnknown: firmware version unknown; only Tail is filled.
	TailUnknown TailLayout = iota
	// TailLineAverages (firmware up to 2.0.0): 0x24 i16 second temperature,
	// 0x26 u16 CC2 average in mV whose low 2 bits hold the sample-rate index,
	// 0x28 u16 D+ average mV, 0x2A u16 D- average mV.
	TailLineAverages
	// TailStatus (newer firmware): 0x24 i16 second temperature, 0x26 u8
	// sample-rate index, 0x27 u8 accelerometer status, 0x28 u16 battery ‰.
	TailStatus
)

func (l TailLayout) String() string {
	switch l {
	case TailLineAverages:
		return "line-averages"
	case TailStatus:
		return "status"
	}
	return "unknown"
}

// TailLayoutFor returns the tail layout used by an application firmware
// version such as "1.9.5" or "2.0.7".
func TailLayoutFor(version string) TailLayout {
	var a, b, c int
	if n, _ := fmt.Sscanf(version, "%d.%d.%d", &a, &b, &c); n != 3 {
		return TailUnknown
	}
	v := a*10000 + b*100 + c
	switch {
	case v <= tailLastLineAverages:
		return TailLineAverages
	case v >= tailFirstStatus:
		return TailStatus
	}
	return TailUnknown
}

// Version boundaries (major*10000 + minor*100 + patch), from analysis of every
// official image (docs/firmware.html §8): 1.9.5 (also captured), 1.9.7, 1.9.9
// and 2.0.0 use line averages; every build from 2.0.1 to 2.0.7 uses the status
// layout.
const (
	tailLastLineAverages = 20000
	tailFirstStatus      = 20001
)

// ApplyTail decodes the firmware-dependent tail bytes.
func (a *ADC) ApplyTail(l TailLayout) {
	a.Layout = l
	a.RateIndex = -1
	t := a.Tail
	if l == TailUnknown || len(t) < 8 {
		return
	}
	le := binary.LittleEndian
	a.Temp2Raw = int16(le.Uint16(t[0:]))
	switch l {
	case TailLineAverages:
		cc2 := le.Uint16(t[2:])
		a.RateIndex = int(cc2 & 3)
		a.CC2AvgMV = cc2 &^ 3
		a.DPAvgMV = le.Uint16(t[4:])
		a.DMAvgMV = le.Uint16(t[6:])
		a.HasLineAvg = true
	case TailStatus:
		a.RateIndex = int(t[2] & 3)
		a.Accel = t[3]
		a.BatteryPermille = le.Uint16(t[4:])
		a.HasStatus = true
	}
}

// Temp2Valid reports whether the second temperature channel is present.
func (a ADC) Temp2Valid() bool { return a.Layout != TailUnknown && a.Temp2Raw != -32768 }

// Temp2C returns the second temperature channel in °C.
func (a ADC) Temp2C() float64 { return float64(a.Temp2Raw) / 128 }

// Request for a single ADC snapshot.
func ADCRequest(id uint8) []byte {
	h := ControlHeader(TypeGetData, id, AttADC)
	return h[:]
}

// ParseADC decodes an ADC section payload.
func ParseADC(p []byte) (ADC, error) {
	if len(p) < adcCoreSize {
		return ADC{}, fmt.Errorf("km003c: ADC payload too short (%d bytes)", len(p))
	}
	le := binary.LittleEndian
	a := ADC{
		VBusUV:    int32(le.Uint32(p[0x00:])),
		IBusUA:    int32(le.Uint32(p[0x04:])),
		VBusAvgUV: int32(le.Uint32(p[0x08:])),
		IBusAvgUA: int32(le.Uint32(p[0x0C:])),
		VBusRawUV: int32(le.Uint32(p[0x10:])),
		IBusRawUA: int32(le.Uint32(p[0x14:])),
		TempRaw:   int16(le.Uint16(p[0x18:])),
		CC1:       le.Uint16(p[0x1A:]),
		CC2:       le.Uint16(p[0x1C:]),
		DP:        le.Uint16(p[0x1E:]),
		DM:        le.Uint16(p[0x20:]),
		VDD:       le.Uint16(p[0x22:]),
	}
	if len(p) > adcCoreSize {
		a.Tail = append([]byte(nil), p[adcCoreSize:]...)
	}
	a.RateIndex = -1
	return a, nil
}

// ParseADCFrame decodes a complete PUT_DATA frame that carries an ADC section.
func ParseADCFrame(frame []byte) (ADC, error) {
	_, secs, err := ParsePutData(frame)
	if err != nil {
		return ADC{}, err
	}
	s, ok := FindSection(secs, AttADC)
	if !ok {
		return ADC{}, fmt.Errorf("km003c: reply has no ADC section")
	}
	return ParseADC(s.Payload)
}

func tenthMV(v uint16) float64 { return float64(v) / 10000 }

// VBus returns the bus voltage in volts.
func (a ADC) VBus() float64 { return float64(a.VBusUV) / 1e6 }

// IBus returns the bus current in amperes (signed).
func (a ADC) IBus() float64 { return float64(a.IBusUA) / 1e6 }

// Power returns VBUS × IBUS in watts (signed like IBUS).
func (a ADC) Power() float64 { return a.VBus() * a.IBus() }

// VBusAvg returns the smoothed bus voltage in volts.
func (a ADC) VBusAvg() float64 { return float64(a.VBusAvgUV) / 1e6 }

// IBusAvg returns the smoothed bus current in amperes.
func (a ADC) IBusAvg() float64 { return float64(a.IBusAvgUA) / 1e6 }

// PowerAvg returns VBusAvg × IBusAvg in watts.
func (a ADC) PowerAvg() float64 { return a.VBusAvg() * a.IBusAvg() }

// Direction is the direction of power flow through the meter.
type Direction string

const (
	// FlowToPlug: current flows from the meter's female receptacle to its
	// male plug - the normal set-up with the source on the receptacle. The
	// meter's screen shows this as a positive current with an arrow pointing
	// to the plug.
	FlowToPlug Direction = "receptacle_to_plug"
	// FlowToReceptacle: current flows from the male plug to the receptacle.
	FlowToReceptacle Direction = "plug_to_receptacle"
	// FlowNone: |IBUS| is below FlowThresholdUA.
	FlowNone Direction = "none"
)

// FlowThresholdUA is the current below which no direction is reported; the
// idle noise floor is about ±0.1 mA.
const FlowThresholdUA = 1000

// The meter's raw IBUS is negative when current flows from the receptacle to
// the plug. Verified on firmware 1.9.5 with a 0.44 A load (raw -0.442 A,
// screen +0.433 A with the arrow toward the plug); the official app and
// open-powerz document the same convention.

// FlowA returns the bus current in amperes, positive when it flows from the
// receptacle to the plug (the sign the meter's screen uses).
func (a ADC) FlowA() float64 { return -a.IBus() }

// FlowAvgA is FlowA for the meter's smoothed average.
func (a ADC) FlowAvgA() float64 { return -a.IBusAvg() }

// FlowPower returns VBUS × FlowA in watts.
func (a ADC) FlowPower() float64 { return a.VBus() * a.FlowA() }

// FlowPowerAvg returns VBusAvg × FlowAvgA in watts.
func (a ADC) FlowPowerAvg() float64 { return a.VBusAvg() * a.FlowAvgA() }

// Direction returns the direction of the instantaneous current.
func (a ADC) Direction() Direction {
	switch {
	case a.IBusUA <= -FlowThresholdUA:
		return FlowToPlug
	case a.IBusUA >= FlowThresholdUA:
		return FlowToReceptacle
	}
	return FlowNone
}

// TempValid reports whether the temperature reading is valid.
func (a ADC) TempValid() bool { return a.TempRaw != -32768 }

// TempC returns the temperature in degrees Celsius.
func (a ADC) TempC() float64 { return float64(a.TempRaw) / 128 }

// CC1V returns the CC1 voltage in volts.
func (a ADC) CC1V() float64 { return tenthMV(a.CC1) }

// CC2V returns the CC2 voltage in volts.
func (a ADC) CC2V() float64 { return tenthMV(a.CC2) }

// DPV returns the D+ voltage in volts.
func (a ADC) DPV() float64 { return tenthMV(a.DP) }

// DMV returns the D- voltage in volts.
func (a ADC) DMV() float64 { return tenthMV(a.DM) }

// VDDV returns the meter's internal supply voltage in volts.
func (a ADC) VDDV() float64 { return tenthMV(a.VDD) }
