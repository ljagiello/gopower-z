## What and why

<!-- What does this change, and why is it needed? -->

## Checklist

- [ ] Only read-only requests are sent to the meter (`km003c/guard.go` unchanged, or the change is explained)
- [ ] Tests added or updated (`go test -race ./...` passes)
- [ ] JSON output unchanged, or `schemaVersion` bumped and `powerz schema` updated
- [ ] Documentation updated in `docs/*.html` (no Markdown docs)
