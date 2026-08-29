## What & why

<!-- What does this PR change, and what problem does it solve? -->

## Checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes
- [ ] `go test ./... -race` passes — and a suite that *skipped* is not a suite
      that passed (see CONTRIBUTING.md, "A skip is not a pass")
- [ ] Tests added/updated for the change
- [ ] Docs updated (`docs/`) — config, CLI, or behavior changes are reflected
- [ ] Commit subjects are `type(scope): summary` (see CONTRIBUTING.md)
- [ ] The PR title takes the same form, and the summary after the prefix reads
      as a release note — it is what appears in the generated release notes
