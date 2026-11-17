# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub private vulnerability reporting](https://github.com/ljagiello/gopower-z/security/advisories/new),
not as public issues.

Useful reports include:

- any way to make powerz send a frame outside its read-only whitelist
  (`km003c/guard.go`);
- crashes or hangs caused by malformed data from a device;
- vulnerabilities in dependencies that affect powerz.

You can expect an acknowledgement within a week. Fixes are released as new
tagged versions.

## Supported versions

Only the latest release receives fixes.
