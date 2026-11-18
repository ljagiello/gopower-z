package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// fieldDoc documents one JSON field. Nested object fields use dotted names
// ("inferred.cc.attached"); objects and arrays whose shape is defined
// elsewhere carry a ref to an entry of "definitions".
type fieldDoc struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // number, integer, string, boolean, object, array
	Nullable bool     `json:"nullable"`
	Enum     []string `json:"enum,omitempty"`  // allowed values of a string field
	Items    string   `json:"items,omitempty"` // element type of an array (a JSON type, or "object" with ref)
	// Ref names the definition of an object (or of an array's elements).
	Ref         string `json:"ref,omitempty"`
	Description string `json:"description"`
}

func fd(name, typ string, nullable bool, desc string) fieldDoc {
	return fieldDoc{Name: name, Type: typ, Nullable: nullable, Description: desc}
}

func fref(name, typ, ref string, nullable bool, desc string) fieldDoc {
	f := fieldDoc{Name: name, Type: typ, Nullable: nullable, Ref: ref, Description: desc}
	if typ == tArr {
		f.Items = tObj
	}
	return f
}

// fenum documents a string field with a closed set of values.
func fenum(name string, nullable bool, desc string, values ...string) fieldDoc {
	return fieldDoc{Name: name, Type: tStr, Nullable: nullable, Enum: values, Description: desc}
}

// farr documents an array of scalars.
func farr(name, items string, desc string) fieldDoc {
	return fieldDoc{Name: name, Type: tArr, Items: items, Description: desc}
}

var (
	directions   = []string{"receptacle_to_plug", "plug_to_receptacle", "none"}
	tailLayouts  = []string{"line-averages", "status", "unknown"}
	endReasons   = []string{"completed", "interrupted", "error"}
	errorCodes   = []string{"usage", "not_found", "busy", "timeout", "permission_denied", "partial", "interrupted", "error"}
	pdEventNames = []string{"none", "attach", "detach", "bad_message_id", "hard_reset", "cable_reset", "receive_error", "monitor_type", "big_message", "unknown"}
)

const (
	tNum  = "number"
	tInt  = "integer"
	tStr  = "string"
	tBool = "boolean"
	tObj  = "object"
	tArr  = "array"
)

// definitions documents every JSON shape powerz prints. A contract test checks
// real output against these lists in both directions.
var definitions = map[string][]fieldDoc{
	"snapshot": {
		fd("schema_version", tInt, false, "output schema version; bumped when a field is renamed or removed"),
		fd("time", tStr, false, "host time of the reading, UTC, ISO 8601 with milliseconds"),
		fd("serial", tStr, false, "USB serial number of the meter"),
		fd("firmware_version", tStr, true, "meter application firmware (selects firmware-dependent decoding); null if it could not be read (see firmware_error)"),
		fd("firmware_error", tStr, true, "why firmware_version is null (the read failed); the firmware-dependent fields are then null. null otherwise"),
		fd("vbus_v", tNum, false, "bus voltage, volts (resolution 1 µV)"),
		fd("ibus_a", tNum, false, "bus current, amperes (resolution 1 µA); positive = flowing from the meter's female receptacle to its male plug (normal set-up: source on the receptacle), negative = reverse; same sign as the meter's screen"),
		fd("power_w", tNum, false, "vbus_v * ibus_a, watts; positive = delivered receptacle -> plug"),
		fenum("direction", false, "flow direction of the current (|current| < 1 mA is none); the arrow on the meter's screen", directions...),
		fd("vbus_avg_v", tNum, false, "the meter's smoothed VBUS average, volts (updates about once per second)"),
		fd("ibus_avg_a", tNum, false, "the meter's smoothed current average, amperes, same sign as ibus_a"),
		fd("power_avg_w", tNum, false, "vbus_avg_v * ibus_avg_a, watts"),
		fd("temp_c", tNum, true, "meter temperature, °C; null when the meter flags it invalid (e.g. at 1000 SPS)"),
		fd("temp2_c", tNum, true, "second (external, reserved) temperature channel, °C; normally null"),
		fd("cc1_v", tNum, false, "CC1 line voltage, volts"),
		fd("cc2_v", tNum, false, "CC2 line voltage, volts"),
		fd("dp_v", tNum, false, "D+ voltage, volts"),
		fd("dm_v", tNum, false, "D- voltage, volts"),
		fd("vdd_v", tNum, false, "the meter's internal supply voltage, volts"),
		fd("cc2_avg_v", tNum, true, "the meter's CC2 average, volts (4 mV resolution); firmware <= 2.0.0 only"),
		fd("dp_avg_v", tNum, true, "the meter's D+ 50-sample average, volts; firmware <= 2.0.0 only"),
		fd("dm_avg_v", tNum, true, "the meter's D- 50-sample average, volts; firmware <= 2.0.0 only"),
		fd("sample_rate_sps", tInt, true, "sample rate configured on the meter: 2, 10, 50 or 1000; null if unknown"),
		fd("sink_battery_percent", tNum, true, "battery level of the sink learned by the meter from USB-PD traffic (firmware >= 2.0.1); null if unknown"),
		fd("inferred", tObj, false, "values computed by powerz, not reported by the meter"),
		fd("inferred.dashboard_label", tStr, true, "the meter's first D+/D- protocol label (DCP, DCP 1.5A, Apple 2.4A, QC2.0 9V, QC3.0 | QC3+, FCP | AFC, VOOC, ...), recomputed with the meter firmware's algorithm; a D+/D- voltage signature, not a confirmed protocol session; null where the screen shows [N/A], and null when dashboard_classifier is null"),
		fd("inferred.dashboard_label_2", tStr, true, "the meter's second D+/D- label (SFCP 9V, FCP | SCP | AFC, VOOC, ...); null where the screen shows [N/A], and null when dashboard_classifier is null"),
		fenum("inferred.dashboard_classifier", true, "which firmware's label algorithm produced the labels: 1.9.5 (firmware 1.7.9-2.0.0) or 2.0.7 (2.0.1-2.0.7); null when the firmware is unknown or outside that range, in which case the labels are unknown rather than [N/A]", "1.9.5", "2.0.7"),
		fd("inferred.cc", tObj, false, "Type-C connection inferred from the CC voltages"),
		fenum("inferred.cc.attached", false, "CC line carrying the Type-C connection (open: pulled up, no sink)", "cc1", "cc2", "both", "none", "open", "unknown"),
		fenum("inferred.cc.advertisement", false, "source current advertisement", "default_usb", "1.5A", "3.0A", "none", "unknown"),
		fd("inferred.cc.advertised_current_a", tNum, true, "1.5 or 3.0; null for default USB current or no advertisement"),
		fd("inferred.cc.text", tStr, false, "one-line human summary"),
		fd("raw", tObj, false, "raw meter values for debugging"),
		fd("raw.ibus_ua", tInt, false, "the meter's raw signed current, µA (negative = receptacle -> plug)"),
		fd("raw.vbus_uncal_avg_uv", tInt, false, "uncalibrated VBUS average, µV"),
		fd("raw.ibus_uncal_avg_ua", tInt, false, "uncalibrated current average, µA, raw sign"),
		fenum("raw.tail_layout", false, "how ADC bytes 0x24..0x2B were decoded: line-averages (firmware 1.7.9-2.0.0), status (2.0.1-2.0.7), unknown (firmware unknown or outside the analysed range: those fields are null)", tailLayouts...),
		fd("raw.tail_hex", tStr, false, "ADC payload bytes 0x24..0x2B, hex"),
		fd("raw.accel_status", tInt, true, "accelerometer status/orientation byte (firmware >= 2.0.1)"),
	},
	"stat": {
		fd("min", tNum, false, "smallest value (signed: for ibus_a/power_w the lowest flow toward the plug)"),
		fd("mean", tNum, false, "arithmetic mean of the samples"),
		fd("max", tNum, false, "largest value (for ibus_a/power_w in the normal direction: the peak draw)"),
		fd("last", tNum, false, "final sample"),
		fd("stdev", tNum, false, "sample standard deviation (0 with one sample)"),
		fd("p5", tNum, false, "5th percentile (linear interpolation between samples)"),
		fd("p50", tNum, false, "median"),
		fd("p95", tNum, false, "95th percentile"),
	},
	"bin": {
		fd("t_s", tNum, false, "start of the bin, seconds from the start of the window"),
		fd("samples", tInt, false, "successful readings in the bin (0: the value objects are null)"),
		fref("vbus_v", tObj, "range", true, "bus voltage in the bin, volts"),
		fref("ibus_a", tObj, "range", true, "current in the bin, amperes, signed like ibus_a"),
		fref("power_w", tObj, "range", true, "power in the bin, watts, signed like power_w"),
	},
	"level": {
		fd("start_s", tNum, false, "start of the level, seconds from the start of the window (0 for the first level)"),
		fd("end_s", tNum, false, "end of the level, seconds: the next level's start, or the end of the window"),
		fd("samples", tInt, false, "successful readings in the level"),
		fref("vbus_v", tObj, "range", false, "bus voltage in the level, volts"),
		fref("ibus_a", tObj, "stat", false, "current in the level, amperes, signed like ibus_a; p50 is the typical value that defines the level"),
		fref("power_w", tObj, "range", false, "power in the level, watts, signed like power_w"),
	},
	"range": {
		fd("min", tNum, false, "smallest value"),
		fd("mean", tNum, false, "mean"),
		fd("max", tNum, false, "largest value"),
	},
	"measure": {
		fd("schema_version", tInt, false, "output schema version"),
		fd("serial", tStr, false, "USB serial number of the meter"),
		fd("firmware_version", tStr, true, "meter application firmware; null if unknown"),
		fd("firmware_error", tStr, true, "why firmware_version is null, otherwise null"),
		fd("start", tStr, false, "start of the window, UTC ISO 8601"),
		fd("end", tStr, false, "end of the window, UTC ISO 8601"),
		fenum("end_reason", false, "completed: ran for --duration (exit 0, or 7 if samples failed); interrupted: stopped by SIGINT/SIGTERM (exit 130); error: the meter went away, was taken by another program or could not be drained (exit with that error's code). The object is printed in every case once a sample was collected", endReasons...),
		fd("duration_s", tNum, false, "wall-clock length of the window, seconds"),
		fd("integrated_s", tNum, false, "time span covered by energy_wh / charge_mah (first sample to end), seconds"),
		fd("interval_s", tNum, false, "requested sampling interval, seconds"),
		fd("samples", tInt, false, "successful readings"),
		fd("failed_samples", tInt, false, "failed readings (exit status 7 if > 0 and end_reason is completed)"),
		fd("last_error", tStr, true, "message of the last failed reading, or null"),
		fenum("direction", false, "dominant flow direction of the mean current (|mean| < 1 mA is none)", directions...),
		fd("stats", tObj, false, "statistics over the successful samples"),
		fref("stats.vbus_v", tObj, "stat", true, "bus voltage, volts"),
		fref("stats.ibus_a", tObj, "stat", true, "current, amperes, signed like ibus_a"),
		fref("stats.power_w", tObj, "stat", true, "power, watts, signed like power_w"),
		fref("stats.temp_c", tObj, "stat", true, "meter temperature, °C; null if never valid"),
		fref("stats.dp_v", tObj, "stat", true, "D+ voltage, volts; a USB device dropping off the bus usually shows as a D+ (or D-) change"),
		fref("stats.dm_v", tObj, "stat", true, "D- voltage, volts"),
		fd("bin_s", tNum, false, fmt.Sprintf("width of the time bins, seconds: --bin (at least --interval, at most %d bins over --duration; equal to --duration for one bin), or automatic, chosen when the window ends from the time actually measured (at least 5 polling intervals per bin, at most %d bins); a bin holds fewer samples, or none, when reads are slow or fail", maxBins, maxAutoBins)),
		fref("bins", tArr, "bin", false, "the window cut into bins of bin_s seconds, in time order (the last one may be shorter): the shape over time that min/mean/max hide. Empty bins have samples 0"),
		fref("levels", tArr, "level", false, fmt.Sprintf("the window split where the typical (median) current changes and stays changed, in time order: one level when nothing changed, [] with no samples. A level lasts at least %g s and %d readings, is steady (at least %g s of its readings within %g%% (and %g mA) of its median: pulses inside it do not count, so on a load that pulses a third of the time a phase needs about 8 s), and its median differs from its neighbours' by at least %g%% (and %g mA); with fewer than %d readings (slow polling) it must also pass a rank test (Mann-Whitney |z| >= %g). Shorter stretches (bursts, spikes, reboots) stay inside the levels around them; a load switching on and off in phases of a few seconds is one level (near 50%% duty its median can be either current, and stretches of it may show as levels). start_s is the first reading of the level: within a reading or two at 100 ms polling, a few readings at 0.5-1 s (a pulse at a change moves it). A ramp shows as a change inside it. Polling much slower than the load's own rhythm can alias it into levels. Candidate changes come from ED-PELT (Haynes, Fearnhead & Eckley 2017) on 1 s averages (window/%d on windows over %d s)", levelMinS, levelMinReadings, levelMinS, levelRel*100, levelAbsA*1000, levelRel*100, levelAbsA*1000, levelFewReadings, levelMinZ, levelPoints, levelPoints)),
		fd("energy_wh", tNum, false, "energy over integrated_s, watt-hours; positive = delivered receptacle -> plug"),
		fd("charge_mah", tNum, false, "charge over integrated_s, milliamp-hours; positive = delivered receptacle -> plug"),
		fref("last", tObj, "snapshot", true, "the final reading (same shape as `read`)"),
		fref("pd", tObj, "pd_summary", true, "USB-PD capture over the window; null with -pd=false"),
	},
	"pd_summary": {
		fd("messages", tInt, false, "captured USB-PD messages (GoodCRC excluded)"),
		fd("events", tInt, false, "captured connection events (attach, detach, hard reset, ...)"),
		fd("undecoded", tInt, false, "records of PD bytes that could not be parsed (kind undecoded); normally 0"),
		fref("source_capabilities", tArr, "pdo", false, "PDOs most recently advertised by the source since the last attach ([] if none seen; cleared on detach and hard reset)"),
		fref("last_request", tObj, "rdo", true, "last sink Request seen (whether or not it was accepted), or null"),
		fref("contract", tObj, "rdo", true, "the active contract: the last Request the source accepted (Accept) and confirmed (PS_RDY); cleared on detach, hard and soft reset; null if no complete negotiation was captured"),
		fd("note", tStr, true, "explanation when nothing was captured (no negotiation during the window is normal), otherwise null"),
		fref("records", tArr, "pd_record", true, "every captured record in order (measure); null in the summary line of `pd`, whose records were streamed"),
	},
	"pd_end": {
		fd("schema_version", tInt, false, "output schema version"),
		fenum("kind", false, "marks the last line of `pd` output", "summary"),
		fd("time", tStr, false, "host time, UTC ISO 8601"),
		fenum("end_reason", false, "completed: --duration or --count reached (exit 0); interrupted: SIGINT/SIGTERM (exit 130); error: reading failed (exit with that error's code). This line is printed in every case", endReasons...),
		fd("duration_s", tNum, false, "length of the capture, seconds"),
		fd("messages", tInt, false, "as in pd_summary"),
		fd("events", tInt, false, "as in pd_summary"),
		fd("undecoded", tInt, false, "as in pd_summary"),
		fref("source_capabilities", tArr, "pdo", false, "as in pd_summary"),
		fref("last_request", tObj, "rdo", true, "as in pd_summary"),
		fref("contract", tObj, "rdo", true, "as in pd_summary"),
		fd("note", tStr, true, "as in pd_summary"),
	},
	"pd_record": {
		fd("schema_version", tInt, false, "output schema version"),
		fd("time", tStr, false, "host time the record was read, UTC ISO 8601"),
		fd("meter_time_ms", tInt, false, "meter uptime when the record was captured, milliseconds"),
		fenum("kind", false, "record type; undecoded: bytes the meter delivered that could not be split into records (kept in raw_hex, since reading drained them from the meter)", "message", "event", "undecoded"),
		fenum("sop", true, "SOP packet type (messages); unknown for the reserved values 5-7", "SOP", "SOP'", "SOP''", "SOP'_Dbg", "SOP''_Dbg", "unknown"),
		fd("message", tStr, true, "USB-PD message name, e.g. Source_Capabilities, Request, Accept, PS_RDY; long_message_undecoded for unverified long records"),
		fenum("class", true, "message class (messages)", "control", "data", "extended"),
		fenum("power_role", true, "port power role (SOP messages)", "source", "sink"),
		fenum("data_role", true, "port data role (SOP messages)", "dfp", "ufp"),
		fenum("spec_revision", true, "USB-PD revision in the header", "1.0", "2.0", "3.x", "?"),
		fd("message_id", tInt, true, "3-bit message counter"),
		fd("from_meter", tBool, false, "true if the meter itself sent the message"),
		farr("objects_hex", tStr, "raw 32-bit data objects as hex strings ([] if none)"),
		fref("source_capabilities", tArr, "pdo", true, "decoded PDOs (Source_Capabilities / EPR_Source_Capabilities), otherwise null"),
		fref("request", tObj, "rdo", true, "decoded Request with the PDO it selects, otherwise null"),
		fenum("event", true, "event type (events)", pdEventNames...),
		fd("cc_line", tInt, true, "CC line (1 or 2) of an attach/detach event"),
		fd("param", tInt, true, "event parameter for other events"),
		fd("raw_hex", tStr, false, "record bytes as captured, hex"),
	},
	"pdo": {
		fd("index", tInt, false, "1-based object position (what a Request selects)"),
		fenum("type", false, "PDO type", "fixed", "battery", "variable", "pps", "epr_avs", "spr_avs", "unknown"),
		fd("min_voltage_v", tNum, false, "minimum voltage, volts (= max for fixed)"),
		fd("max_voltage_v", tNum, false, "maximum voltage, volts"),
		fd("max_current_a", tNum, true, "maximum current, amperes (SPR AVS: at 15 V); null when the PDO specifies power"),
		fd("max_current_20v_a", tNum, true, "SPR AVS maximum current at 20 V, amperes; otherwise null"),
		fd("max_power_w", tNum, false, "maximum power, watts"),
		fd("raw_hex", tStr, false, "raw PDO"),
		fd("text", tStr, false, "one-line human summary"),
	},
	"rdo": {
		fd("object_position", tInt, false, "1-based position of the selected PDO"),
		fd("voltage_v", tNum, true, "requested voltage for PPS/AVS, volts; null for fixed"),
		fd("operating_current_a", tNum, true, "requested operating current, amperes"),
		fd("max_current_a", tNum, true, "maximum operating current (fixed/variable), amperes"),
		fd("operating_power_w", tNum, true, "requested power (battery), watts"),
		fd("capability_mismatch", tBool, false, "sink says the offered power is not enough"),
		fd("epr", tBool, false, "EPR mode capable"),
		fref("pdo", tObj, "pdo", true, "the source PDO it selects, if the capabilities were captured"),
		fd("raw_hex", tStr, false, "raw RDO"),
		fd("text", tStr, false, "one-line human summary"),
	},
	"info": {
		fd("schema_version", tInt, false, "output schema version"),
		fref("usb", tObj, "usb", false, "USB descriptors"),
		fd("device", tObj, true, "meter identity read from three memory blocks; null if none could be read. A block that fails leaves its fields null and adds an entry to errors (exit status 7)"),
		fd("device.model", tStr, true, "e.g. KM003C"),
		fd("device.firmware_version", tStr, true, "application firmware, e.g. 1.9.5"),
		fd("device.firmware_date", tStr, true, "firmware build date, YYYY-MM-DD"),
		fd("device.hardware_version", tStr, true, "hardware/bootloader version, e.g. 2.2"),
		fd("device.hardware_date", tStr, true, "hardware/bootloader date, YYYY-MM-DD"),
		fd("device.serial", tStr, true, "factory serial number"),
		fd("device.uuid", tStr, true, "factory unique id (32 hex characters), null if absent"),
		fenum("device.adc_tail_layout", false, "firmware-dependent ADC decoding in use", tailLayouts...),
		fd("settings", tObj, true, "meter settings; null if unavailable (exit status 7)"),
		fd("settings.device_name", tStr, false, "name shown on the meter"),
		fd("settings.sample_rate_sps", tInt, false, "2, 10, 50 or 1000"),
		fd("settings.brightness_percent", tInt, false, "screen brightness, %"),
		fd("settings.rotation_code", tInt, false, "raw screen-rotation code (0-3); meaning not decoded"),
		fd("settings.auto_rotate", tBool, false, "screen auto-rotation enabled"),
		fd("settings.language_code", tInt, false, "raw UI language code; meaning not decoded"),
		fd("settings.theme_code", tInt, false, "raw UI theme code; meaning not decoded"),
		fd("settings.checksum_ok", tBool, false, "both settings blocks passed their CRC-32"),
		farr("errors", tStr, "parts that could not be read ([] if none; exit status 7 otherwise)"),
	},
	"usb": {
		fd("manufacturer", tStr, false, "USB manufacturer string"),
		fd("product", tStr, false, "USB product string"),
		fd("serial", tStr, false, "USB serial number (use with -serial)"),
		fd("bus", tInt, false, "USB bus number"),
		fd("address", tInt, false, "USB device address"),
	},
	"devices": {
		fd("schema_version", tInt, false, "output schema version"),
		fd("count", tInt, false, "number of meters found"),
		fref("devices", tArr, "usb", false, "one entry per meter"),
	},
	"version": {
		fd("schema_version", tInt, false, "output schema version"),
		fd("version", tStr, false, "powerz version"),
	},
	"error": {
		fd("schema_version", tInt, false, "output schema version"),
		fd("error", tObj, false, "the error (printed on stderr)"),
		fenum("error.code", false, "stable error code", errorCodes...),
		fd("error.exit_code", tInt, false, "the process exit status"),
		fd("error.message", tStr, false, "human-readable message"),
		fd("error.retryable", tBool, false, "true if retrying the same command can succeed"),
		fd("error.hint", tStr, false, "what to do next"),
	},
}

type output struct {
	Format string   `json:"format"` // json_object, json_lines, csv, text
	Refs   []string `json:"refs"`   // definitions of what stdout carries ([] for text)
	Stderr string   `json:"stderr"` // definition of the error object printed on failure
	Note   string   `json:"note,omitempty"`
}

func out(format, note string, refs ...string) output {
	if refs == nil {
		refs = []string{}
	}
	return output{Format: format, Refs: refs, Stderr: "error", Note: note}
}

type commandDoc struct {
	Name    string    `json:"name"`
	Summary string    `json:"summary"`
	Output  output    `json:"output"`
	Bounded bool      `json:"bounded"`
	Flags   []flagDoc `json:"flags"`
	Example string    `json:"example"`
	Notes   []string  `json:"notes"`
}

type flagDoc struct {
	Name        string `json:"name"`                // e.g. --duration
	Shorthand   string `json:"shorthand,omitempty"` // e.g. -n
	Type        string `json:"type"`                // bool, int, duration, string
	Default     string `json:"default"`
	Description string `json:"description"`
}

func flagsOf(fs *pflag.FlagSet) []flagDoc {
	out := []flagDoc{}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		d := flagDoc{Name: "--" + f.Name, Type: f.Value.Type(), Default: f.DefValue, Description: f.Usage}
		if f.Shorthand != "" {
			d.Shorthand = "-" + f.Shorthand
		}
		out = append(out, d)
	})
	return out
}

// buildSchema describes the command tree rooted at root.
func buildSchema(root *cobra.Command) map[string]any {
	type spec struct {
		out     output
		bounded bool
		example string
		notes   []string
	}
	specs := map[string]spec{
		"read": {out("json_object", "", "snapshot"), true, "powerz read --json", []string{
			"Reads the firmware version first (CONNECT plus one MemoryRead), which selects the decoding of firmware-dependent fields.",
		}},
		"measure": {out("json_object", "", "measure"), true, "powerz measure --duration 10s --json", []string{
			"Preferred command for agents: how much power/energy the attached device draws, and what the charger offered and negotiated.",
			"The meter samples at its configured rate (info: settings.sample_rate_sps); polling faster only repeats values.",
			"USB-PD is only exchanged on attach or renegotiation: start measure, then connect the device, to capture it.",
			fmt.Sprintf("measure keeps every reading until the window ends (for the percentiles), so a window holds at most %d readings (just under 28 h at 100 ms, up to about 250 MB of memory); longer runs are a usage error: use log.", maxSamples),
		}},
		"log": {out("json_lines", "csv columns with --format csv: "+strings.Join(csvColumns, ","), "snapshot"), false,
			"powerz log --duration 30s --interval 500ms --json", []string{"Runs until --duration/--count or interrupt; always pass --duration or --count from automation."}},
		"pd": {out("json_lines", "one pd_record per captured record, then one pd_end summary line (kind=summary)", "pd_record", "pd_end"), false,
			"powerz pd --duration 20s --json", []string{
				"Passive capture: the meter sniffs traffic between the charger and the device plugged through it; powerz never sends PD messages.",
				"Sends only GET_DATA for the PD buffer: no CONNECT session and no memory reads.",
				"Reading drains the meter's PD buffer, so only one consumer sees each record.",
				"An empty capture is normal when nothing negotiated during the window; the summary line's note says so.",
				"Always pass --duration from automation (--count alone waits for traffic).",
			}},
		"info":    {out("json_object", "", "info"), true, "powerz info --json", []string{}},
		"devices": {out("json_object", "", "devices"), true, "powerz devices --json", []string{"Does not claim the meter, so it works while another program uses it."}},
		"watch":   {out("text", "full-screen terminal UI"), false, "powerz watch", []string{"Interactive terminals only; agents should use measure/log/pd."}},
		"schema":  {out("json_object", "this document"), true, "powerz schema", []string{}},
		"version": {out("json_object", "", "version"), true, "powerz version --json", []string{}},
	}
	var cmds []commandDoc
	for _, sub := range root.Commands() {
		sp, ok := specs[sub.Name()]
		if !ok {
			continue // cobra's help and completion commands
		}
		cmds = append(cmds, commandDoc{
			Name: sub.Name(), Summary: sub.Short, Output: sp.out, Bounded: sp.bounded,
			Flags: flagsOf(sub.LocalNonPersistentFlags()), Example: sp.example, Notes: sp.notes,
		})
	}
	return map[string]any{
		"schema_version": schemaVersion,
		"tool":           "powerz",
		"version":        Version,
		"description":    "Read-only CLI for the ChargerLAB POWER-Z KM003C USB-C power meter (USB 5fc9:0063).",
		"device": map[string]any{
			"ranges":       "VBUS 0-50 V, current 0-6 A in either direction, resolution 1 µV / 1 µA",
			"connection":   "the meter's USB-C port marked HID goes to this computer; the source (charger) plugs into the meter's female receptacle and the device under test onto its male plug",
			"sample_rates": "2, 10, 50 or 1000 samples/s, configured on the meter",
		},
		"output_contract": []string{
			"stdout carries only data; diagnostics and errors go to stderr",
			"JSON is the default whenever stdout is not a terminal; --json / --text override",
			"flags take two dashes (--duration 10s); --count has the shorthand -n",
			"every documented key is always present; unavailable values are null, empty lists are []",
			"field names end in their unit: _v volts, _a amperes, _w watts, _wh watt-hours, _mah milliamp-hours, _c °C, _s seconds, _ms milliseconds, _uv microvolts, _ua microamperes, _sps samples per second, _percent percent",
			"timestamps are UTC ISO 8601 with milliseconds (2026-01-02T03:04:05.678Z)",
			"current and power are positive when flowing from the meter's female receptacle to its male plug (the normal set-up), matching the meter's screen; see direction",
			"errors are one JSON object on stderr: {\"schema_version\":1,\"error\":{\"code\",\"exit_code\",\"message\",\"retryable\",\"hint\"}}",
			"fields under inferred are computed by powerz, not reported by the meter",
			"only one powerz process may use a meter at a time; concurrent calls fail with code busy unless --wait is given",
			"a non-zero exit does not always mean empty stdout: measure prints its object and pd its summary line when they end early (end_reason interrupted or error), and log lines already printed stay valid; read stdout before discarding it",
		},
		"common_flags": flagsOf(root.PersistentFlags()),
		"completion":   "powerz completion bash|zsh|fish|powershell prints a shell completion script",
		"commands":     cmds,
		"definitions":  definitions,
		"exit_codes": []map[string]any{
			{"exit_code": exitOK, "code": "ok", "retryable": false, "meaning": "success"},
			{"exit_code": exitError, "code": "error", "retryable": false, "meaning": "I/O or protocol failure"},
			{"exit_code": exitUsage, "code": "usage", "retryable": false, "meaning": "bad command, flag or argument"},
			{"exit_code": exitNotFound, "code": "not_found", "retryable": false, "meaning": "no meter connected (or none with --serial); if it disappeared mid-run, measure/pd printed what they had (end_reason error)"},
			{"exit_code": exitBusy, "code": "busy", "retryable": true, "meaning": "another program holds the meter; retry or use --wait; if it was taken mid-run, measure/pd printed what they had (end_reason error)"},
			{"exit_code": exitTimeout, "code": "timeout", "retryable": true, "meaning": "the meter did not answer in time"},
			{"exit_code": exitPermission, "code": "permission_denied", "retryable": false, "meaning": "the OS denied USB access"},
			{"exit_code": exitPartial, "code": "partial", "retryable": true, "meaning": "output was printed but some requested data is missing"},
			{"exit_code": exitInterrupt, "code": "interrupted", "retryable": false, "meaning": "SIGINT/SIGTERM; measure/pd printed what they had collected (end_reason interrupted)"},
		},
		"verified": []string{
			"on firmware 1.9.5 with a 0.44 A load: current direction, the D+/D- dashboard labels ([N/A] / VOOC) and VBUS/IBUS/CC/D+/D- against the meter's screen",
		},
		"caveats": []string{
			"USB-PD decoding is tested against the USB-PD specification and synthetic messages; it has not yet been checked against a live negotiation.",
			"Protocols the meter detects by sniffing (AFC/FCP/SCP/UFCS/VOOC sessions) are not exposed over USB; inferred.dashboard_label* is the meter's D+/D- voltage signature.",
			"Linux support is built and unit-tested in CI but has not been exercised with a meter attached.",
			"pd opens no CONNECT session; firmware analysis shows 1.9.5 records PD traffic regardless, but attach/detach capture by pd itself has not yet been observed live (measure, which opens a session, has).",
			"Firmware outside 1.7.9-2.0.7 is not guessed: firmware-dependent fields and dashboard labels are null until that firmware is analysed.",
		},
		"safety": "Every frame sent to the meter is checked against a read-only whitelist (GET_DATA for ADC/settings/PD, CONNECT/DISCONNECT, and MemoryRead of three fixed info blocks). powerz never writes settings, never triggers fast-charge protocols (cannot change VBUS), never authenticates and never touches firmware.",
	}
}

func setupSchema(*pflag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		return g.emitIndented(buildSchema(g.root))
	}
}
