# Contributing

Thanks for helping. Bug reports, protocol observations from other firmware
versions, and pull requests are all welcome.

## Ground rules

- **Read-only toward the meter.** Every frame goes through the whitelist in
  `hardware/km003c/guard.go`. Changes that write settings, trigger fast-charge
  protocols, authenticate, or touch firmware will not be accepted.
- **Never write while a reply is pending.** The meter stalls and its watchdog
  reboots it if it receives a request while a reply or zero-length packet is
  still queued. Use `Device.exchange`/`memoryRead`, which handle this.
- **The JSON output is a contract.** Renaming or removing a field requires
  bumping `schemaVersion` in `internal/cli/root.go` and updating `powerz schema`.
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
(`hardware/km003c/testdata/fw1.9.5/`) through a fake meter
(`internal/fakemeter`) that models the meter's stall behaviour, timeouts and
session, so no hardware is needed. The CLI tests run whole commands against
it and check their real output against `powerz schema`.

Layout: `hardware/<model>/` holds one package per supported meter
(`hardware/km003c`), `usbpd/` decodes USB Power Delivery messages and is
shared by all of them, `internal/cli/` holds the commands (cobra) and
`cmd/powerz/` is the entrypoint.

If you have a meter on a different firmware version, captured frames are very
useful. Run the failing command with `--trace` and attach the output to an
issue, together with `powerz info --json`.

## Releasing

Maintainers cut releases from GitHub: Actions → Release → Run workflow on
`main`, choosing `patch`, `minor` or `major`. The workflow picks the next tag
with `.github/scripts/next-version.sh`, builds and verifies every platform,
and only then pushes the tag and publishes the release with `checksums.txt`
and release notes grouped by commit type (`feat:`, `fix:`, other). Tick
`dry-run` to build and verify without tagging or publishing; pull requests
that change the release workflow run the same dry run.

## Pull requests

- Keep changes focused, and include tests for decoding or protocol changes.
- CI runs `go vet`, `go test -race`, golangci-lint and actionlint on every PR.
- By contributing, you agree that your contributions are licensed under the
  Apache License 2.0.
