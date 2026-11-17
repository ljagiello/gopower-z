# Contributing

Thanks for helping. Bug reports, protocol observations from other firmware
versions, and pull requests are all welcome.

## Ground rules

- **Read-only toward the meter.** Every frame goes through the whitelist in
  `km003c/guard.go`. Changes that write settings, trigger fast-charge
  protocols, authenticate, or touch firmware will not be accepted.
- **Never write while a reply is pending.** The meter stalls and its watchdog
  reboots it if it receives a request while a reply or zero-length packet is
  still queued. Use `Device.exchange`/`memoryRead`, which handle this.
- **The JSON output is a contract.** Renaming or removing a field requires
  bumping `schemaVersion` in `cmd/powerz/main.go` and updating `powerz schema`.
- **Docs are HTML.** Documentation lives in `docs/*.html` (self-contained,
  inline CSS, light/dark). Please don't add Markdown documentation.

## Development

Requires Go (see `go.mod`) and libusb with pkg-config
(`brew install libusb pkgconf` or `apt install libusb-1.0-0-dev pkg-config`).

```sh
make test   # go test -race ./...
make lint   # golangci-lint run
make build  # ./powerz
```

Unit tests replay frames captured from a real KM003C
(`km003c/testdata/fw1.9.5/`) and use a fake transport that models the
meter's stall behaviour, so no hardware is needed.

If you have a meter on a different firmware version, captured frames are very
useful. Run the failing command with `-trace` and attach the output to an
issue, together with `powerz info -json`.

## Pull requests

- Keep changes focused, and include tests for decoding or protocol changes.
- CI runs `go vet`, `go test -race`, golangci-lint and actionlint on every PR.
- By contributing, you agree that your contributions are licensed under the
  Apache License 2.0.
