# gopower-z

[![CI](https://github.com/ljagiello/gopower-z/actions/workflows/ci.yml/badge.svg)](https://github.com/ljagiello/gopower-z/actions/workflows/ci.yml)
[![CodeQL](https://github.com/ljagiello/gopower-z/actions/workflows/codeql.yml/badge.svg)](https://github.com/ljagiello/gopower-z/actions/workflows/codeql.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ljagiello/gopower-z.svg)](https://pkg.go.dev/github.com/ljagiello/gopower-z)

`powerz` is a read-only command-line client and Go library for the
**ChargerLAB POWER-Z KM003C** USB-C power meter (USB `5fc9:0063`). It is
designed for people and AI agents alike. Piped output is JSON with units in
the field names, errors are structured with stable exit codes, and
`powerz schema` describes the whole interface.

```sh
# macOS: brew install libusb pkgconf   ·   Debian/Ubuntu: apt install libusb-1.0-0-dev pkg-config
go install github.com/ljagiello/gopower-z/cmd/powerz@latest

powerz                        # live dashboard (or one JSON reading when piped)
powerz measure -duration 10s  # min/mean/max V·A·W, Wh, mAh, USB-PD contract
powerz info                   # model, firmware, serial, settings
powerz pd -duration 30s       # decoded USB Power Delivery traffic
powerz schema                 # machine-readable reference for agents
```

Prebuilt binaries are attached to
[GitHub Releases](https://github.com/ljagiello/gopower-z/releases) for macOS
(arm64, x86_64; libusb is built in) and Linux (arm64, x86_64; glibc 2.28 or
newer, plus the libusb 1.0 runtime package, e.g. `libusb-1.0-0` on
Debian/Ubuntu or `libusbx` on RHEL 8/9). See `INSTALL.txt` in the archive.

powerz only ever sends a small whitelist of read requests. It cannot change
meter settings, trigger fast-charge protocols (so it cannot raise VBUS), or
touch firmware.

## Documentation

All documentation is HTML in [`docs/`](docs/), published at
**https://ljagiello.github.io/gopower-z/**:

- [CLI reference and agent guide](https://ljagiello.github.io/gopower-z/cli.html)
- [KM003C USB protocol](https://ljagiello.github.io/gopower-z/protocol.html)
- [Hardware notes](https://ljagiello.github.io/gopower-z/hardware.html)
- [Official software reverse engineering](https://ljagiello.github.io/gopower-z/official-software-re.html)
- [Firmware analysis](https://ljagiello.github.io/gopower-z/firmware.html)

## License

[Apache License 2.0](LICENSE). Not affiliated with or endorsed by ChargerLAB.
