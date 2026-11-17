// Package cli is the powerz command line: the cobra command tree, the JSON
// output contract, error classification and every command. It is driven by
// humans and by AI agents alike: when stdout is not a terminal every command
// prints JSON, errors are JSON objects with stable codes and exit statuses,
// and `powerz schema` describes the whole interface. cmd/powerz is a thin
// entrypoint that calls Main.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ljagiello/gopower-z/hardware/km003c"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// Version is set by cmd/powerz (from -ldflags or the module build info).
var Version = "dev"

// schemaVersion is bumped whenever a JSON field is renamed or removed.
const schemaVersion = 1

// minTimeout is the smallest accepted --timeout; shorter values cannot cover
// the meter's reply latency plus USB scheduling.
const minTimeout = 100 * time.Millisecond

// Exit statuses. Stable: agents branch on them.
const (
	exitOK         = 0
	exitError      = 1 // I/O or protocol failure
	exitUsage      = 2
	exitNotFound   = 3
	exitBusy       = 4
	exitTimeout    = 5
	exitPermission = 6
	exitPartial    = 7 // output printed, but some requested data is missing
	exitInterrupt  = 130
)

// env is everything a command touches outside the process: the output
// streams and the meter. Tests replace it.
type env struct {
	stdout, stderr io.Writer
	// tty reports whether stdout is an interactive terminal.
	tty bool
	// openUSB opens the meter with the given USB serial ("" = first one).
	openUSB func(serial string) (km003c.Transport, km003c.USBDescription, error)
	// listUSB lists connected meters without opening them.
	listUSB func() ([]km003c.USBDescription, error)
}

func osEnv() *env {
	return &env{
		stdout: os.Stdout, stderr: os.Stderr, tty: isTTY(os.Stdout),
		openUSB: func(serial string) (km003c.Transport, km003c.USBDescription, error) {
			t, err := km003c.OpenUSB(serial)
			if err != nil {
				return nil, km003c.USBDescription{}, err
			}
			return t, t.Description(), nil
		},
		listUSB: km003c.ListDevices,
	}
}

// Main runs powerz with the given arguments (without the program name) and
// returns the process exit status.
func Main(ctx context.Context, args []string) int {
	return run(ctx, osEnv(), args)
}

// globals holds the persistent flags and the environment.
type globals struct {
	serial  string
	trace   bool
	timeout time.Duration
	wait    time.Duration
	json    bool
	text    bool
	env     *env
	root    *cobra.Command
}

// addCommon registers the flags every command accepts.
func addCommon(fs *pflag.FlagSet, g *globals) {
	fs.StringVar(&g.serial, "serial", g.serial, "USB serial number of the meter to use (list them with: powerz devices)")
	fs.BoolVar(&g.trace, "trace", g.trace, "dump every raw USB frame (hex) to stderr")
	fs.DurationVar(&g.timeout, "timeout", g.timeout, "per-request timeout (minimum 100ms)")
	fs.DurationVar(&g.wait, "wait", g.wait, "if the meter is busy (used by another process), keep retrying for up to this long")
	fs.BoolVar(&g.json, "json", g.json, "force JSON output (default when stdout is not a terminal)")
	fs.BoolVar(&g.text, "text", g.text, "force human-readable output")
}

// jsonMode reports whether output should be JSON.
func (g globals) jsonMode() bool {
	switch {
	case g.json:
		return true
	case g.text:
		return false
	}
	return !g.env.tty
}

func (g globals) out() io.Writer { return g.env.stdout }

// style returns the ANSI styling for stdout.
func (g globals) style() style { return style{on: g.env.tty && os.Getenv("NO_COLOR") == ""} }

func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// cliError carries a stable error code for machine consumers.
type cliError struct {
	Code      string `json:"code"`
	ExitCode  int    `json:"exit_code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint"`
	exit      int
}

func (e *cliError) Error() string { return e.Message }

func usageErr(format string, a ...any) error {
	return &cliError{Code: "usage", Message: fmt.Sprintf(format, a...), exit: exitUsage,
		Hint: "run `powerz --help` or `powerz schema` for the command reference"}
}

// withHint replaces the hint of a usage error.
func withHint(err error, hint string) error {
	if ce, ok := errors.AsType[*cliError](err); ok {
		ce.Hint = hint
	}
	return err
}

// flagHint lists the flags a command accepts, plus format help for values.
func flagHint(c *cobra.Command, perr error) string {
	var names []string
	c.Flags().VisitAll(func(f *pflag.Flag) {
		n := "--" + f.Name
		if f.Shorthand != "" {
			n += "/-" + f.Shorthand
		}
		names = append(names, n)
	})
	sort.Strings(names)
	h := "valid flags for " + c.CommandPath() + ": " + strings.Join(names, " ")
	msg := perr.Error()
	switch {
	case strings.Contains(msg, "unknown shorthand flag"):
		h = "long flags take two dashes, e.g. --duration 10s. " + h
	case strings.Contains(msg, "invalid argument"):
		h = "durations look like 500ms, 10s or 2m; integers like 5. " + h
	}
	return h
}

// partialErr marks a command that printed its output but could not obtain
// everything that was requested.
type partialErr struct{ msg string }

func (e partialErr) Error() string { return e.msg }

func isPartial(err error) bool {
	_, ok := errors.AsType[partialErr](err)
	return ok
}

// classify maps any error to a cliError.
func classify(err error) *cliError {
	if ce, ok := errors.AsType[*cliError](err); ok {
		return ce
	}
	switch {
	case isPartial(err):
		return &cliError{Code: "partial", Message: err.Error(), exit: exitPartial, Retryable: true,
			Hint: "the JSON on stdout is valid but incomplete; see its errors / failed_samples / last_error fields"}
	case errors.Is(err, context.Canceled):
		return &cliError{Code: "interrupted", Message: "interrupted", exit: exitInterrupt,
			Hint: "stopped by SIGINT/SIGTERM; measure and pd still printed what they had collected (end_reason \"interrupted\"), log lines already printed are valid"}
	case errors.Is(err, km003c.ErrNotFound):
		return &cliError{Code: "not_found", Message: err.Error(), exit: exitNotFound,
			Hint: "connect the KM003C's data port (the USB-C side marked HID) to this computer; `powerz devices` lists detected meters"}
	case errors.Is(err, km003c.ErrBusy):
		return &cliError{Code: "busy", Message: err.Error(), exit: exitBusy, Retryable: true,
			Hint: "another program holds the meter (another powerz process, the ChargerLab app, or a browser WebUSB tab); retry, or pass --wait 10s to queue behind it"}
	case errors.Is(err, km003c.ErrPermission):
		return &cliError{Code: "permission_denied", Message: err.Error(), exit: exitPermission,
			Hint: "on Linux install a udev rule granting access to USB 5fc9:0063, or run with sufficient privileges"}
	case errors.Is(err, km003c.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return &cliError{Code: "timeout", Message: err.Error(), exit: exitTimeout, Retryable: true,
			Hint: "retry; if it persists, unplug and reconnect the meter"}
	case errors.Is(err, km003c.ErrStuck):
		return &cliError{Code: "error", Message: err.Error(), exit: exitError,
			Hint: "the meter keeps sending data powerz cannot drain, so nothing was written to it; unplug and reconnect the meter"}
	}
	return &cliError{Code: "error", Message: err.Error(), exit: exitError,
		Hint: "rerun with --trace to see the USB frames; if the meter misbehaves, unplug and reconnect it"}
}

func run(ctx context.Context, e *env, args []string) int {
	g := &globals{timeout: time.Second, env: e}
	root := newRootCmd(g)
	root.SetArgs(args)
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		return report(*g, err)
	}
	return exitOK
}

// command describes one subcommand. setup registers its flags and returns
// the function that runs it.
type command struct {
	summary string
	setup   func(fs *pflag.FlagSet) func(ctx context.Context, g globals) error
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"read":    {"print one measurement snapshot", setupRead},
		"measure": {"sample for a fixed duration and print one summary (min/mean/max, stdev, percentiles, time bins, energy, charge, PD)", setupMeasure},
		"log":     {"stream measurements as JSON lines or CSV", setupLog},
		"pd":      {"stream decoded USB Power Delivery messages captured passively by the meter", setupPD},
		"info":    {"device model, firmware, serial and settings", setupInfo},
		"devices": {"list connected meters without opening them", setupDevices},
		"watch":   {"live full-screen dashboard (interactive terminals only)", setupWatch},
		"schema":  {"print the machine-readable description of every command, flag, field and exit code", setupSchema},
		"version": {"print the version", setupVersion},
	}
}

// commandNames lists the commands in a stable order.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for c := range commands {
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}

func newRootCmd(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:     "powerz",
		Short:   "Read-only CLI for the ChargerLAB POWER-Z KM003C USB-C power meter",
		Long:    rootLong,
		Version: Version,
		// Errors are rendered by report (JSON on stderr in JSON mode),
		// never by cobra.
		SilenceUsage:  true,
		SilenceErrors: true,
		// "did you mean" for typos up to two edits away.
		SuggestionsMinimumDistance: 2,
	}
	root.SetVersionTemplate("powerz {{.Version}}\n")
	addCommon(root.PersistentFlags(), g)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return withHint(usageErr("%s: %v", c.CommandPath(), err), flagHint(c, err))
	})
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		if g.timeout < minTimeout {
			return usageErr("--timeout must be at least %v", minTimeout)
		}
		if g.wait < 0 {
			return usageErr("--wait must not be negative")
		}
		return nil
	}

	subs := map[string]*cobra.Command{}
	for _, name := range commandNames() {
		c := commands[name]
		sub := &cobra.Command{Use: name, Short: c.summary, Args: noArgs}
		runFn := c.setup(sub.Flags())
		sub.RunE = func(cc *cobra.Command, _ []string) error { return runFn(cc.Context(), *g) }
		subs[name] = sub
		root.AddCommand(sub)
	}
	// Without a command: the dashboard on a terminal, one reading otherwise.
	root.Args = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		hint := "commands: " + strings.Join(commandNames(), ", ")
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			hint = "did you mean `powerz " + s[0] + "`? " + hint
		}
		return withHint(usageErr("unknown command %q", args[0]), hint)
	}
	root.RunE = func(c *cobra.Command, _ []string) error {
		name := "read"
		if g.env.tty && !g.json {
			name = "watch"
		}
		return subs[name].RunE(subs[name], nil)
	}
	g.root = root
	return root
}

// noArgs rejects positional arguments with a usage error.
func noArgs(c *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErr("%s: unexpected argument %q", c.CommandPath(), args[0])
	}
	return nil
}

// report prints an error (JSON on stderr in JSON mode) and returns its exit
// status.
func report(g globals, err error) int {
	ce := classify(err)
	ce.ExitCode = ce.exit
	if g.jsonMode() {
		_, _ = fmt.Fprintln(g.env.stderr, string(errorJSON(ce)))
	} else {
		_, _ = fmt.Fprintf(g.env.stderr, "powerz: %s\n", ce.Message)
		if ce.Hint != "" {
			_, _ = fmt.Fprintf(g.env.stderr, "hint: %s\n", ce.Hint)
		}
	}
	return ce.exit
}

// errorJSON is the stderr form of an error.
func errorJSON(ce *cliError) []byte {
	b, _ := json.Marshal(struct {
		SchemaVersion int       `json:"schema_version"`
		Error         *cliError `json:"error"`
	}{schemaVersion, ce})
	return b
}

// emit writes one JSON value as a single line to stdout.
func (g globals) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = g.out().Write(append(b, '\n'))
	return err
}

// emitIndented writes one JSON document (pretty-printed on a terminal).
func (g globals) emitIndented(v any) error {
	if !g.env.tty {
		return g.emit(v)
	}
	enc := json.NewEncoder(g.out())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// meter bundles an open device with its USB descriptor strings.
type meter struct {
	*km003c.Device
	usb km003c.USBDescription
	// firmwareErr is why the firmware version is unknown, if it is.
	firmwareErr error
}

// close releases the USB interface; errors on release are not actionable.
func (m *meter) close() { _ = m.Close() }

// detect reads the firmware version, which selects the decoding of the
// firmware-dependent ADC fields. Failure is not fatal: those fields are then
// null and firmwareErr says why.
func (m *meter) detect(ctx context.Context) {
	if _, err := m.DetectLayout(ctx); err != nil && ctx.Err() == nil {
		m.firmwareErr = err
	}
}

// firmwareError is the JSON form of firmwareErr.
func (m *meter) firmwareError() *string {
	if m.firmwareErr == nil {
		return nil
	}
	return new(m.firmwareErr.Error())
}

// open opens the meter, retrying for up to --wait while another process
// holds it. It sends nothing: commands that need the firmware version call
// detect.
func open(ctx context.Context, g globals) (*meter, error) {
	deadline := time.Now().Add(g.wait)
	var t km003c.Transport
	var desc km003c.USBDescription
	var err error
	for {
		t, desc, err = g.env.openUSB(g.serial)
		if err == nil || !errors.Is(err, km003c.ErrBusy) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	d := km003c.NewDevice(t)
	d.Timeout = g.timeout
	if g.trace {
		d.Trace = func(dir string, f []byte) {
			_, _ = fmt.Fprintf(g.env.stderr, "%s %-9s %s\n", time.Now().Format("15:04:05.000"), dir, hexdump(f))
		}
	}
	return &meter{Device: d, usb: desc}, nil
}

const rootLong = `powerz - read-only CLI for the ChargerLAB POWER-Z KM003C USB-C power meter

Without a command it shows the live dashboard on a terminal and prints one
JSON reading otherwise. Start with:  powerz schema   and
powerz measure --duration 10s

For AI agents and scripts:
  * stdout carries only data; diagnostics and errors go to stderr.
  * JSON whenever stdout is not a terminal (--json / --text override).
  * Every documented key is always present (null / [] when unavailable);
    units are in field names; timestamps are UTC ISO 8601.
  * Current and power are positive when flowing from the meter's female
    receptacle to its male plug (the normal set-up); see "direction".
  * Errors are JSON on stderr:
    {"schema_version":1,"error":{"code","exit_code","message","retryable","hint"}}
  * Exit codes: 0 ok, 1 error, 2 usage, 3 not_found, 4 busy, 5 timeout,
    6 permission_denied, 7 partial, 130 interrupted.
  * A non-zero exit does not always mean stdout is empty: measure and pd
    print what they collected when interrupted or when the meter fails
    mid-run (end_reason), and log lines already printed stay valid.
  * Run one powerz process per meter at a time, or pass --wait 10s to queue.

Safety: every frame sent to the meter is checked against a read-only
whitelist. powerz never changes settings, never triggers fast-charge
protocols (it cannot raise VBUS) and never touches firmware.`

type versionResult struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
}

func setupVersion(*pflag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		if g.jsonMode() {
			return g.emit(versionResult{SchemaVersion: schemaVersion, Version: Version})
		}
		_, err := fmt.Fprintln(g.out(), "powerz", Version)
		return err
	}
}
