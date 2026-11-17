// Command powerz is a read-only command-line client for the ChargerLAB
// POWER-Z KM003C USB-C power meter. The commands live in internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/ljagiello/gopower-z/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=vX.Y.Z".
// Builds via `go install …@vX.Y.Z` fall back to the module version.
var version = "dev"

func main() {
	cli.Version = resolveVersion(version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Without this a closed stdout pipe (`powerz log | head -1`) kills the
	// process before the meter's USB interface is released cleanly; with it,
	// writes fail with EPIPE and the commands stop normally.
	signal.Ignore(syscall.SIGPIPE)
	code := cli.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// resolveVersion returns the stamped version, or the module version when
// none was stamped.
func resolveVersion(stamped string) string {
	if stamped != "dev" {
		return stamped
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return stamped
}
