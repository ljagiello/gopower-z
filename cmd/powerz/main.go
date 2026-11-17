// Command powerz is a read-only command-line client for the ChargerLAB
// POWER-Z KM003C USB-C power meter, designed to be driven by humans and by
// AI agents alike: when stdout is not a terminal every command prints JSON,
// errors are JSON objects with stable codes and exit statuses, and
// `powerz schema` describes the whole interface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/ljagiello/gopower-z/km003c"
	"golang.org/x/term"
)

// schemaVersion is bumped whenever a JSON field is renamed or removed.
const schemaVersion = 1

// version is set at release time with -ldflags "-X main.version=vX.Y.Z".
// Builds via `go install …@vX.Y.Z` fall back to the module version.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

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

type globals struct {
	serial  string
	trace   bool
	timeout time.Duration
	wait    time.Duration
	json    bool
	text    bool
}

// addCommon registers the flags every command accepts.
func addCommon(fs *flag.FlagSet, g *globals) {
	fs.StringVar(&g.serial, "serial", g.serial, "USB serial number of the meter to use (see `powerz devices`)")
	fs.BoolVar(&g.trace, "trace", g.trace, "dump every raw USB frame (hex) to stderr")
	fs.DurationVar(&g.timeout, "timeout", g.timeout, "per-request timeout")
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
	return !isTTY(os.Stdout)
}

func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// colorOn reports whether ANSI styling may be used on stdout.
func colorOn() bool { return isTTY(os.Stdout) && os.Getenv("NO_COLOR") == "" }

// cliError carries a stable error code for machine consumers.
type cliError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint,omitempty"`
	exit      int
}

func (e *cliError) Error() string { return e.Message }

func usageErr(format string, a ...any) error {
	return &cliError{Code: "usage", Message: fmt.Sprintf(format, a...), exit: exitUsage,
		Hint: "run `powerz help` or `powerz schema` for the command reference"}
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
		return &cliError{Code: "partial", Message: err.Error(), exit: exitPartial, Retryable: true}
	case errors.Is(err, context.Canceled):
		return &cliError{Code: "interrupted", Message: "interrupted", exit: exitInterrupt}
	case errors.Is(err, km003c.ErrNotFound):
		return &cliError{Code: "not_found", Message: err.Error(), exit: exitNotFound,
			Hint: "connect the KM003C's data port (the USB-C side marked HID) to this computer; `powerz devices` lists detected meters"}
	case errors.Is(err, km003c.ErrBusy):
		return &cliError{Code: "busy", Message: err.Error(), exit: exitBusy, Retryable: true,
			Hint: "another program holds the meter (another powerz process, the ChargerLab app, or a browser WebUSB tab); powerz commands must not run concurrently against one meter - wait and retry"}
	case errors.Is(err, km003c.ErrPermission):
		return &cliError{Code: "permission_denied", Message: err.Error(), exit: exitPermission,
			Hint: "on Linux install a udev rule granting access to USB 5fc9:0063, or run with sufficient privileges"}
	case errors.Is(err, km003c.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return &cliError{Code: "timeout", Message: err.Error(), exit: exitTimeout, Retryable: true,
			Hint: "retry; if it persists, unplug and reconnect the meter"}
	}
	return &cliError{Code: "error", Message: err.Error(), exit: exitError}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	g := globals{timeout: time.Second}
	top := flag.NewFlagSet("powerz", flag.ContinueOnError)
	top.SetOutput(io.Discard)
	addCommon(top, &g)
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usageText)
			return exitOK
		}
		return report(g, usageErr("%v", err))
	}

	rest := top.Args()
	cmd := ""
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	if cmd == "" {
		if isTTY(os.Stdout) && !g.json {
			cmd = "watch"
		} else {
			cmd = "read"
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, ok := commands[cmd]
	if !ok {
		return report(g, usageErr("unknown command %q", cmd))
	}
	fs := flag.NewFlagSet("powerz "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addCommon(fs, &g)
	run := c.setup(fs)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandHelp(cmd, c, fs)
			return exitOK
		}
		return report(g, usageErr("%s: %v", cmd, err))
	}
	if fs.NArg() > 0 {
		return report(g, usageErr("%s: unexpected argument %q", cmd, fs.Arg(0)))
	}
	if err := run(ctx, g); err != nil {
		return report(g, err)
	}
	return exitOK
}

// report prints an error (JSON on stderr in JSON mode) and returns its exit
// status.
func report(g globals, err error) int {
	ce := classify(err)
	if g.jsonMode() {
		b, _ := json.Marshal(struct {
			SchemaVersion int       `json:"schema_version"`
			Error         *cliError `json:"error"`
		}{schemaVersion, ce})
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintf(os.Stderr, "powerz: %s\n", ce.Message)
		if ce.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", ce.Hint)
		}
	}
	return ce.exit
}

// emit writes one JSON value as a single line to stdout.
func emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

// emitIndented writes one JSON document (pretty-printed on a terminal).
func emitIndented(v any) error {
	if !isTTY(os.Stdout) {
		return emit(v)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// meter bundles an open device with its USB descriptor strings.
type meter struct {
	*km003c.Device
	usb km003c.USBDescription
}

// close releases the USB interface; errors on release are not actionable.
func (m *meter) close() { _ = m.Close() }

func open(ctx context.Context, g globals) (*meter, error) {
	deadline := time.Now().Add(g.wait)
	var t *km003c.USBTransport
	var err error
	for {
		t, err = km003c.OpenUSB(g.serial)
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
			fmt.Fprintf(os.Stderr, "%s %-9s %s\n", time.Now().Format("15:04:05.000"), dir, hexdump(f))
		}
	}
	return &meter{Device: d, usb: t.Description()}, nil
}

// command describes one subcommand. setup registers its flags and returns
// the function that runs it.
type command struct {
	summary string
	setup   func(fs *flag.FlagSet) func(ctx context.Context, g globals) error
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"read":    {"print one measurement snapshot", setupRead},
		"measure": {"sample for a fixed duration and print one summary (min/mean/max, energy, charge, PD)", setupMeasure},
		"log":     {"stream measurements as JSON lines or CSV", setupLog},
		"pd":      {"stream decoded USB Power Delivery messages captured passively by the meter", setupPD},
		"info":    {"device model, firmware, serial and settings", setupInfo},
		"devices": {"list connected meters without opening them", setupDevices},
		"watch":   {"live full-screen dashboard (interactive terminals only)", setupWatch},
		"schema":  {"print the machine-readable description of every command, flag, field and exit code", setupSchema},
		"version": {"print the version", setupVersion},
		"help":    {"print usage", setupHelp},
	}
}

const usageText = `powerz - read-only CLI for the ChargerLAB POWER-Z KM003C USB-C power meter

Usage:
  powerz [flags] <command> [command flags]

Commands:
  read       one measurement snapshot (default when stdout is not a terminal)
  measure    sample for -duration and print one summary: min/mean/max V, A, W,
             energy (Wh), charge (mAh), negotiated USB-PD contract
  log        stream measurements (JSON lines or CSV); use -duration or -n
  pd         stream decoded USB-PD messages; use -duration or -n
  info       model, firmware, serial number and settings
  devices    list connected meters
  watch      live dashboard (default on an interactive terminal)
  schema     machine-readable reference: commands, flags, fields, exit codes
  version    print the version

Common flags (accepted by every command):
  -json         force JSON output (automatic when stdout is not a terminal)
  -text         force human-readable output
  -serial S     select a meter by USB serial number
  -timeout D    per-request timeout (default 1s)
  -wait D       if another process holds the meter, retry for up to D
  -trace        dump raw USB frames (hex) to stderr

For AI agents and scripts:
  * stdout carries only data; diagnostics and errors go to stderr.
  * Errors are JSON on stderr: {"error":{"code","message","retryable","hint"}}.
  * Exit codes: 0 ok, 1 error, 2 usage, 3 not_found, 4 busy, 5 timeout,
    6 permission_denied, 7 partial, 130 interrupted.
  * Run one powerz process per meter at a time (the USB interface is exclusive).
  * Start with:  powerz schema   and   powerz measure -duration 10s

Safety: every frame sent to the meter is checked against a read-only
whitelist. powerz never changes settings, never triggers fast-charge
protocols (it cannot raise VBUS) and never touches firmware.
`

func printCommandHelp(name string, c command, fs *flag.FlagSet) {
	fmt.Printf("powerz %s - %s\n\nFlags:\n", name, c.summary)
	fs.SetOutput(os.Stdout)
	fs.PrintDefaults()
}

func setupHelp(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(context.Context, globals) error {
		fmt.Print(usageText)
		return nil
	}
}

func setupVersion(fs *flag.FlagSet) func(context.Context, globals) error {
	return func(_ context.Context, g globals) error {
		if g.jsonMode() {
			return emit(map[string]any{"schema_version": schemaVersion, "version": version})
		}
		fmt.Println("powerz", version)
		return nil
	}
}
