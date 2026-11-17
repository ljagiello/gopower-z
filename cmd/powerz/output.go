package main

import (
	"fmt"
	"math"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
)

// utcTime formats host timestamps for JSON: UTC, fixed millisecond precision,
// so every RFC 3339 / ISO 8601 parser accepts them.
func utcTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// snapshot is one ADC reading. Field names carry their unit; keys are always
// present (null when a value is unavailable).
type snapshot struct {
	SchemaVersion   int      `json:"schema_version"`
	Time            string   `json:"time"`
	Serial          string   `json:"serial"`
	FirmwareVersion *string  `json:"firmware_version"`
	FirmwareError   *string  `json:"firmware_error"`
	VBusV           float64  `json:"vbus_v"`
	IBusA           float64  `json:"ibus_a"`
	PowerW          float64  `json:"power_w"`
	Direction       string   `json:"direction"`
	VBusAvgV        float64  `json:"vbus_avg_v"`
	IBusAvgA        float64  `json:"ibus_avg_a"`
	PowerAvgW       float64  `json:"power_avg_w"`
	TempC           *float64 `json:"temp_c"`
	Temp2C          *float64 `json:"temp2_c"`
	CC1V            float64  `json:"cc1_v"`
	CC2V            float64  `json:"cc2_v"`
	DPV             float64  `json:"dp_v"`
	DMV             float64  `json:"dm_v"`
	VDDV            float64  `json:"vdd_v"`
	CC2AvgV         *float64 `json:"cc2_avg_v"`
	DPAvgV          *float64 `json:"dp_avg_v"`
	DMAvgV          *float64 `json:"dm_avg_v"`
	SampleRateSPS   *int     `json:"sample_rate_sps"`
	SinkBatteryPct  *float64 `json:"sink_battery_percent"`
	Inferred        inferred `json:"inferred"`
	Raw             rawADC   `json:"raw"`
}

// inferred holds values computed by powerz rather than reported by the meter.
type inferred struct {
	// DashboardLabel and DashboardLabel2 are the meter's two D+/D- protocol
	// labels (a D+/D- voltage signature, not a confirmed protocol session),
	// recomputed with the meter firmware's algorithm; null where the screen
	// shows "[N/A]", or when DashboardClassifier is null.
	DashboardLabel  *string `json:"dashboard_label"`
	DashboardLabel2 *string `json:"dashboard_label_2"`
	// DashboardClassifier names the firmware whose label algorithm was used
	// ("1.9.5" for firmware 1.7.9-2.0.0, "2.0.7" for 2.0.1-2.0.7); null when
	// the firmware is unknown or outside that range, so no labels exist.
	DashboardClassifier *string `json:"dashboard_classifier"`
	CC                  ccInfo  `json:"cc"`
}

type rawADC struct {
	IBusUA         int32  `json:"ibus_ua"` // meter's signed value: negative = receptacle → plug
	VBusUncalAvgUV int32  `json:"vbus_uncal_avg_uv"`
	IBusUncalAvgUA int32  `json:"ibus_uncal_avg_ua"`
	TailLayout     string `json:"tail_layout"`
	TailHex        string `json:"tail_hex"`
	AccelStatus    *int   `json:"accel_status"`
}

// round keeps the given decimals and removes negative zero, so JSON stays
// short (no 1e-8 artefacts of float multiplication).
func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	r := math.Round(v*p) / p
	if r == 0 {
		return 0
	}
	return r
}

// label converts a dashboard label to JSON: null for the "[ N/A ]" sentinel.
func label(s string) *string {
	if s == km003c.LabelNA || s == "" {
		return nil
	}
	return &s
}

// classifierName is the firmware whose dashboard-label algorithm applies to
// a tail layout, or nil.
func classifierName(l km003c.TailLayout) *string {
	switch l {
	case km003c.TailLineAverages:
		return new("1.9.5")
	case km003c.TailStatus:
		return new("2.0.7")
	}
	return nil
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toSnapshot converts one reading. firmwareErr explains a missing firmware
// version (nil when it is known or was not needed).
func toSnapshot(a km003c.ADC, t time.Time, serial, firmware string, firmwareErr *string) snapshot {
	l1, l2, ok := km003c.DashboardLabels(a)
	inf := inferred{CC: ccInfer(a.CC1V(), a.CC2V())}
	if ok {
		inf.DashboardLabel, inf.DashboardLabel2 = label(l1), label(l2)
		inf.DashboardClassifier = classifierName(a.Layout)
	}
	s := snapshot{
		SchemaVersion:   schemaVersion,
		Time:            utcTime(t),
		Serial:          serial,
		FirmwareVersion: strOrNil(firmware),
		FirmwareError:   firmwareErr,
		VBusV:           round(a.VBus(), 6),
		IBusA:           round(a.FlowA(), 6),
		PowerW:          round(a.FlowPower(), 6),
		Direction:       string(a.Direction()),
		VBusAvgV:        round(a.VBusAvg(), 6),
		IBusAvgA:        round(a.FlowAvgA(), 6),
		PowerAvgW:       round(a.FlowPowerAvg(), 6),
		CC1V:            round(a.CC1V(), 4),
		CC2V:            round(a.CC2V(), 4),
		DPV:             round(a.DPV(), 4),
		DMV:             round(a.DMV(), 4),
		VDDV:            round(a.VDDV(), 4),
		Inferred:        inf,
		Raw: rawADC{IBusUA: a.IBusUA, VBusUncalAvgUV: a.VBusRawUV, IBusUncalAvgUA: a.IBusRawUA,
			TailLayout: a.Layout.String(), TailHex: hexdump(a.Tail)},
	}
	if a.TempValid() {
		s.TempC = new(round(a.TempC(), 3))
	}
	if a.Temp2Valid() {
		s.Temp2C = new(round(a.Temp2C(), 3))
	}
	if a.HasLineAvg {
		s.CC2AvgV = new(float64(a.CC2AvgMV) / 1000)
		s.DPAvgV = new(float64(a.DPAvgMV) / 1000)
		s.DMAvgV = new(float64(a.DMAvgMV) / 1000)
	}
	if sps := km003c.SampleRate(a.RateIndex).SamplesPerSecond(); a.RateIndex >= 0 && sps > 0 {
		s.SampleRateSPS = &sps
	}
	if a.HasStatus {
		s.Raw.AccelStatus = new(int(a.Accel))
		if a.BatteryPermille > 0 {
			s.SinkBatteryPct = new(float64(a.BatteryPermille) / 10)
		}
	}
	return s
}

// stat accumulates min/mean/max/last.
type stat struct {
	min, max, sum, last float64
	n                   int
}

func (s *stat) add(v float64) {
	if s.n == 0 || v < s.min {
		s.min = v
	}
	if s.n == 0 || v > s.max {
		s.max = v
	}
	s.sum += v
	s.last = v
	s.n++
}

func (s stat) avg() float64 {
	if s.n == 0 {
		return 0
	}
	return s.sum / float64(s.n)
}

type statJSON struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
	Last float64 `json:"last"`
}

func (s stat) json(decimals int) *statJSON {
	if s.n == 0 {
		return nil
	}
	return &statJSON{
		Min: round(s.min, decimals), Mean: round(s.avg(), decimals),
		Max: round(s.max, decimals), Last: round(s.last, decimals),
	}
}

// meanDirection classifies the mean current like ADC.Direction.
func meanDirection(meanA float64) string {
	switch {
	case meanA*1e6 >= km003c.FlowThresholdUA:
		return string(km003c.FlowToPlug)
	case meanA*1e6 <= -km003c.FlowThresholdUA:
		return string(km003c.FlowToReceptacle)
	}
	return string(km003c.FlowNone)
}

// isoDate turns the meter's "2025.1.7" dates into "2025-01-07".
func isoDate(s string) string {
	var y, m, d int
	if n, _ := fmt.Sscanf(s, "%d.%d.%d", &y, &m, &d); n == 3 && m >= 1 && m <= 12 && d >= 1 && d <= 31 {
		return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	}
	return s
}
