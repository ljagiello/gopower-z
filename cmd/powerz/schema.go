package main

import (
	"context"
	"flag"
	"sort"
)

type flagDoc struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	Description string `json:"description"`
}

type commandDoc struct {
	Name    string            `json:"name"`
	Summary string            `json:"summary"`
	Output  string            `json:"output"`
	Bounded bool              `json:"bounded"` // terminates on its own
	Flags   []flagDoc         `json:"flags"`
	Fields  map[string]string `json:"fields,omitempty"`
	Example string            `json:"example"`
	Notes   []string          `json:"notes,omitempty"`
}

var snapshotFields = map[string]string{
	"schema_version":             "integer; bumped when a field is renamed or removed",
	"time":                       "RFC 3339 host timestamp of the reading",
	"serial":                     "USB serial number of the meter",
	"firmware_version":           "meter application firmware version (selects how firmware-dependent fields are decoded); omitted if it could not be read",
	"vbus_v":                     "bus voltage, volts (resolution 1 µV)",
	"ibus_a":                     "bus current, amperes, signed (resolution 1 µA); negative = current flowing from the meter's female receptacle to its male plug (per the official app; not yet bench-verified)",
	"power_w":                    "vbus_v * ibus_a, watts, signed like ibus_a",
	"vbus_avg_v":                 "meter's smoothed VBUS average, volts (updates about once per second)",
	"ibus_avg_a":                 "meter's smoothed IBUS average, amperes",
	"power_avg_w":                "vbus_avg_v * ibus_avg_a, watts",
	"temp_c":                     "meter temperature, °C; null when the meter reports it invalid",
	"temp2_c":                    "second (external, reserved) temperature channel, °C; null when absent (normal)",
	"cc1_v":                      "CC1 line voltage, volts",
	"cc2_v":                      "CC2 line voltage, volts",
	"dp_v":                       "D+ voltage, volts",
	"dm_v":                       "D- voltage, volts",
	"vdd_v":                      "meter's internal supply voltage, volts",
	"cc2_avg_v":                  "meter's CC2 average, volts (4 mV resolution); firmware <= 2.0.0 only, otherwise null",
	"dp_avg_v":                   "meter's D+ average, volts (1 mV resolution); firmware <= 2.0.0 only, otherwise null",
	"dm_avg_v":                   "meter's D- average, volts (1 mV resolution); firmware <= 2.0.0 only, otherwise null",
	"sample_rate_sps":            "the meter's configured sample rate (2, 10, 50 or 1000); null if unknown",
	"sink_battery_percent":       "battery level of the sink, learned by the meter from USB-PD traffic (firmware 2.0.1+); null if unknown",
	"inferred.dashboard_label":   "the meter's first D+/D- protocol label (e.g. DCP, Apple 2.4A, QC2.0 9V, FCP | AFC, [ N/A ]), recomputed with the algorithm of the meter's firmware generation (1.9.x/2.0.0 or 2.0.1+)",
	"inferred.dashboard_label_2": "the meter's second D+/D- label (e.g. SFCP 9V, FCP | SCP | AFC, [ N/A ])",
	"inferred.cc":                "powerz's interpretation of CC voltages (Type-C current advertisement); inference, not meter data",
	"raw.vbus_uncal_avg_uv":      "uncalibrated VBUS average, µV",
	"raw.ibus_uncal_avg_ua":      "uncalibrated IBUS average, µA",
	"raw.tail_layout":            "how ADC bytes 0x24..0x2B were decoded: line-averages (firmware <= 2.0.0), status (2.0.1+) or unknown",
	"raw.tail_hex":               "ADC payload bytes 0x24..0x2B verbatim",
	"raw.accel_status":           "accelerometer status/orientation byte (status layout only)",
}

var measureFields = map[string]string{
	"start, end":                      "RFC 3339 host timestamps",
	"duration_s":                      "actual measurement duration, seconds",
	"interval_s":                      "requested sampling interval, seconds",
	"samples":                         "number of successful readings",
	"errors":                          "number of failed readings (exit status 7 if > 0)",
	"vbus_v, ibus_a, power_w, temp_c": "objects {min, mean, max, last} in the field's unit",
	"energy_wh":                       "integral of power over time, watt-hours, signed",
	"charge_mah":                      "integral of current over time, milliamp-hours, signed",
	"last":                            "the final reading, same fields as `read`",
	"pd.messages, pd.events":          "counts of captured USB-PD messages (GoodCRC excluded) and connection events",
	"pd.source_capabilities":          "PDOs most recently advertised by the source: {index, type, min_voltage_v, max_voltage_v, max_current_a, max_power_w, raw_hex, text}",
	"pd.last_request":                 "last sink Request: {object_position, voltage_v, operating_current_a, max_current_a, operating_power_w, capability_mismatch, epr, pdo, raw_hex, text}",
	"pd.contract":                     "last request acknowledged by PS_RDY (the active contract), or null",
	"pd.records":                      "every captured record (only with -pd-records), same shape as `pd` output",
}

var pdFields = map[string]string{
	"meter_time_ms":       "meter uptime when the record was captured, milliseconds",
	"kind":                "\"message\" or \"event\"",
	"sop":                 "SOP, SOP', SOP'', SOP'_Dbg, SOP''_Dbg",
	"message":             "USB-PD message name, e.g. Source_Capabilities, Request, Accept, PS_RDY",
	"class":               "control, data or extended",
	"power_role":          "source or sink (SOP messages)",
	"data_role":           "dfp or ufp (SOP messages)",
	"spec_revision":       "1.0, 2.0 or 3.x",
	"message_id":          "3-bit message counter",
	"from_meter":          "true if the meter itself sent the message",
	"objects_hex":         "raw 32-bit data objects",
	"source_capabilities": "decoded PDOs (Source_Capabilities only)",
	"request":             "decoded RDO with the PDO it selects (Request only)",
	"event":               "attach, detach, hard reset, cable reset, receive error, ... (events only)",
	"cc":                  "CC line of an attach/detach event",
	"raw_hex":             "record bytes as captured",
}

func flagsOf(fs *flag.FlagSet) []flagDoc {
	common := map[string]bool{"serial": true, "trace": true, "timeout": true, "wait": true, "json": true, "text": true}
	out := []flagDoc{}
	fs.VisitAll(func(f *flag.Flag) {
		if common[f.Name] {
			return
		}
		typ := "string"
		if _, ok := f.Value.(flag.Getter); ok {
			switch f.Value.(flag.Getter).Get().(type) {
			case bool:
				typ = "bool"
			case int:
				typ = "int"
			case interface{ Seconds() float64 }:
				typ = "duration (e.g. 500ms, 10s, 2m)"
			}
		}
		out = append(out, flagDoc{Name: "-" + f.Name, Type: typ, Default: f.DefValue, Description: f.Usage})
	})
	return out
}

func buildSchema() map[string]any {
	type spec struct {
		output  string
		bounded bool
		fields  map[string]string
		example string
		notes   []string
	}
	specs := map[string]spec{
		"read": {"one JSON object", true, snapshotFields, "powerz read -json", nil},
		"measure": {"one JSON object", true, measureFields, "powerz measure -duration 10s -json", []string{
			"Preferred command for agents: answers 'how much power/energy does the attached device draw' and 'what did the charger offer and negotiate'.",
			"The meter samples at its own configured rate (see `info` settings.sample_rate_sps); polling faster only repeats values.",
		}},
		"log": {"JSON lines (one snapshot per line, same fields as `read`) or CSV", false, nil, "powerz log -duration 30s -interval 500ms -json", []string{
			"Runs until -duration/-n or interrupt; always pass -duration or -n from automation.",
		}},
		"pd": {"JSON lines, one per captured PD message or event", false, pdFields, "powerz pd -duration 20s -json", []string{
			"Passive capture: the meter only sniffs traffic between the charger and the device plugged through it; powerz never sends PD messages.",
			"Reading PD drains the meter's PD buffer, so only one consumer sees each record.",
			"Always pass -duration or -n from automation.",
		}},
		"info": {"one JSON object {usb, device, settings, errors}", true, map[string]string{
			"usb":      "{manufacturer, product, serial, bus, address} from USB descriptors",
			"device":   "{model, firmware_version, firmware_date, hardware_version, hardware_date, serial, uuid} or null",
			"settings": "{device_name, sample_rate_sps, brightness_percent, rotation, auto_rotate, language, theme, checksum_ok} or null",
			"errors":   "parts that could not be read (exit status 7)",
		}, "powerz info -json", nil},
		"devices": {"one JSON object {count, devices:[{manufacturer, product, serial, bus, address}]}", true, nil, "powerz devices -json", []string{
			"Does not claim the meter, so it works while another program uses it.",
		}},
		"watch":   {"full-screen terminal UI", false, nil, "powerz watch", []string{"Interactive terminals only; agents should use measure/log/pd."}},
		"schema":  {"this document", true, nil, "powerz schema", nil},
		"version": {"one JSON object {version}", true, nil, "powerz version -json", nil},
		"help":    {"usage text", true, nil, "powerz help", nil},
	}
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	var cmds []commandDoc
	for _, n := range names {
		c := commands[n]
		fs := flag.NewFlagSet(n, flag.ContinueOnError)
		var g globals
		addCommon(fs, &g)
		c.setup(fs)
		sp := specs[n]
		cmds = append(cmds, commandDoc{
			Name: n, Summary: c.summary, Output: sp.output, Bounded: sp.bounded,
			Flags: flagsOf(fs), Fields: sp.fields, Example: sp.example, Notes: sp.notes,
		})
	}
	return map[string]any{
		"schema_version": schemaVersion,
		"tool":           "powerz",
		"version":        version,
		"description":    "Read-only CLI for the ChargerLAB POWER-Z KM003C USB-C power meter (USB 5fc9:0063).",
		"device": map[string]any{
			"ranges":       "VBUS 0-50 V, IBUS 0-6 A (bidirectional), resolution 1 µV / 1 µA",
			"connection":   "the meter's USB-C port marked HID connects to this computer; the device under test is plugged between the meter's male and female USB-C ports",
			"sample_rates": "2, 10, 50 or 1000 samples/s, configured on the meter",
		},
		"output_contract": []string{
			"stdout carries only data; diagnostics and errors go to stderr",
			"JSON is the default whenever stdout is not a terminal; -json / -text override",
			"field names end in their unit: _v volts, _a amperes, _w watts, _wh watt-hours, _mah milliamp-hours, _c °C, _s seconds, _ms milliseconds",
			"errors are printed to stderr as {\"schema_version\":1,\"error\":{\"code\",\"message\",\"retryable\",\"hint\"}}",
			"fields under `inferred` are powerz's interpretation, not values reported by the meter",
			"only one powerz process may use a meter at a time (exclusive USB interface); concurrent calls fail with code `busy` unless -wait is given",
		},
		"common_flags": []flagDoc{
			{"-json", "bool", "auto", "force JSON output"},
			{"-text", "bool", "auto", "force human-readable output"},
			{"-serial", "string", "", "select a meter by USB serial (see `devices`)"},
			{"-timeout", "duration", "1s", "per-request timeout (minimum 100ms)"},
			{"-wait", "duration", "0s", "if another process holds the meter, retry for up to this long instead of failing with busy"},
			{"-trace", "bool", "false", "dump raw USB frames to stderr"},
		},
		"commands": cmds,
		"exit_codes": []map[string]any{
			{"code": exitOK, "name": "ok"},
			{"code": exitError, "name": "error", "retryable": false, "meaning": "I/O or protocol failure"},
			{"code": exitUsage, "name": "usage", "retryable": false, "meaning": "bad command, flag or argument"},
			{"code": exitNotFound, "name": "not_found", "retryable": false, "meaning": "no meter connected (or none with -serial)"},
			{"code": exitBusy, "name": "busy", "retryable": true, "meaning": "another program holds the meter"},
			{"code": exitTimeout, "name": "timeout", "retryable": true, "meaning": "the meter did not answer in time"},
			{"code": exitPermission, "name": "permission_denied", "retryable": false, "meaning": "OS denied USB access"},
			{"code": exitPartial, "name": "partial", "retryable": true, "meaning": "output was printed but some requested data is missing"},
			{"code": exitInterrupt, "name": "interrupted", "retryable": false, "meaning": "SIGINT/SIGTERM"},
		},
		"caveats": []string{
			"ibus_a sign: the official app and open-powerz state negative = current from the female receptacle to the male plug; not yet bench-verified with a known load.",
			"The meter's D+/D- dashboard labels are recomputed exactly (inferred.dashboard_label*). Protocols the meter detects by sniffing (AFC/FCP/SCP/UFCS/VOOC) are not exposed over USB; USB-PD details come from `pd` and measure's pd section.",
		},
		"safety": "Every frame sent to the meter is checked against a read-only whitelist (GET_DATA for ADC/settings/PD, CONNECT, and MemoryRead of three fixed info blocks). powerz never writes settings, never triggers fast-charge protocols (cannot change VBUS), never authenticates and never touches firmware.",
	}
}

func setupSchema(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(context.Context, globals) error {
		return emitIndented(buildSchema())
	}
}
