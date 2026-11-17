package main

import (
	"math"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
)

// snapshot is one ADC reading. Field names carry their unit.
type snapshot struct {
	SchemaVersion   int       `json:"schema_version"`
	Time            time.Time `json:"time"`
	Serial          string    `json:"serial,omitempty"`
	FirmwareVersion string    `json:"firmware_version,omitempty"`
	VBusV           float64   `json:"vbus_v"`
	IBusA           float64   `json:"ibus_a"`
	PowerW          float64   `json:"power_w"`
	VBusAvgV        float64   `json:"vbus_avg_v"`
	IBusAvgA        float64   `json:"ibus_avg_a"`
	PowerAvgW       float64   `json:"power_avg_w"`
	TempC           *float64  `json:"temp_c"`
	Temp2C          *float64  `json:"temp2_c"`
	CC1V            float64   `json:"cc1_v"`
	CC2V            float64   `json:"cc2_v"`
	DPV             float64   `json:"dp_v"`
	DMV             float64   `json:"dm_v"`
	VDDV            float64   `json:"vdd_v"`
	CC2AvgV         *float64  `json:"cc2_avg_v"`
	DPAvgV          *float64  `json:"dp_avg_v"`
	DMAvgV          *float64  `json:"dm_avg_v"`
	SampleRateSPS   *int      `json:"sample_rate_sps"`
	SinkBatteryPct  *float64  `json:"sink_battery_percent"`
	Inferred        inferred  `json:"inferred"`
	Raw             rawADC    `json:"raw"`
}

// inferred holds values computed by powerz rather than reported by the meter.
type inferred struct {
	// DashboardLabel and DashboardLabel2 are the meter's two D+/D- protocol
	// labels, recomputed with the meter firmware's own algorithm.
	DashboardLabel  string `json:"dashboard_label"`
	DashboardLabel2 string `json:"dashboard_label_2"`
	// CC is powerz's reading of the CC voltages (Type-C advertisement).
	CC string `json:"cc"`
}

type rawADC struct {
	VBusUncalAvgUV int32  `json:"vbus_uncal_avg_uv"`
	IBusUncalAvgUA int32  `json:"ibus_uncal_avg_ua"`
	TailLayout     string `json:"tail_layout"`
	TailHex        string `json:"tail_hex,omitempty"`
	AccelStatus    *int   `json:"accel_status,omitempty"`
}

// round keeps 9 significant decimals and removes negative zero, so JSON stays
// short (no 1e-8 artefacts of float multiplication).
func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	r := math.Round(v*p) / p
	if r == 0 {
		return 0
	}
	return r
}

func toSnapshot(a km003c.ADC, t time.Time, serial, firmware string) snapshot {
	l1, l2 := km003c.DashboardLabels(a)
	s := snapshot{
		SchemaVersion:   schemaVersion,
		Time:            t,
		Serial:          serial,
		FirmwareVersion: firmware,
		VBusV:           round(a.VBus(), 6),
		IBusA:           round(a.IBus(), 6),
		PowerW:          round(a.Power(), 6),
		VBusAvgV:        round(a.VBusAvg(), 6),
		IBusAvgA:        round(a.IBusAvg(), 6),
		PowerAvgW:       round(a.PowerAvg(), 6),
		CC1V:            round(a.CC1V(), 4),
		CC2V:            round(a.CC2V(), 4),
		DPV:             round(a.DPV(), 4),
		DMV:             round(a.DMV(), 4),
		VDDV:            round(a.VDDV(), 4),
		Inferred:        inferred{DashboardLabel: l1, DashboardLabel2: l2, CC: ccHint(a.CC1V(), a.CC2V())},
		Raw: rawADC{VBusUncalAvgUV: a.VBusRawUV, IBusUncalAvgUA: a.IBusRawUA,
			TailLayout: a.Layout.String()},
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
	if len(a.Tail) > 0 {
		s.Raw.TailHex = hexdump(a.Tail)
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
