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
	IBusUA    int32  // IBUS, µA, signed (sign gives the direction of power flow)
	VBusAvgUV int32  // smoothed VBUS average, µV
	IBusAvgUA int32  // smoothed IBUS average, µA
	VBusRawUV int32  // uncalibrated VBUS average
	IBusRawUA int32  // uncalibrated IBUS average
	TempRaw   int16  // die temperature, 1/128 °C; -32768 = invalid
	CC1, CC2  uint16 // CC line voltages, 0.1 mV
	DP, DM    uint16 // D+ / D- voltages, 0.1 mV
	VDD       uint16 // internal supply, 0.1 mV
	// Averages in the 44-byte payload (offsets 0x26-0x2B), 1 mV units.
	// Verified on firmware 1.9.5 against the live CC2/D+/D- readings.
	HasLineAvg bool
	CC2AvgMV   uint16
	DPAvgMV    uint16
	DMAvgMV    uint16
	Tail       []byte // payload bytes 0x24.. verbatim (0x24/0x25 are undocumented)
}

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
	if len(p) >= 0x2C {
		a.HasLineAvg = true
		a.CC2AvgMV = le.Uint16(p[0x26:])
		a.DPAvgMV = le.Uint16(p[0x28:])
		a.DMAvgMV = le.Uint16(p[0x2A:])
	}
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
