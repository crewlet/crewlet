## What & why

<!-- What does this PR change, and what problem does it solve? -->

## Checklist

- [ ] `make check` passes — gofmt, `go mod tidy -diff`, `go vet`,
      golangci-lint, the build, the full suite under `-race`, and a
      cross-compile of every release target
- [ ] A suite that *skipped* is not a suite that passed (see CONTRIBUTING.md,
      "A skip is not a pass") — and neither is one `make check` never runs:
      it names those on the way out
- [ ] Tests added/updated for the change
- [ ] Docs updated (`docs/`) — config, CLI, or behavior changes are reflected
- [ ] Commit subjects are `type(scope): summary` (see CONTRIBUTING.md)
- [ ] The PR title takes the same form, and the summary after the prefix reads
      as a release note — it is what appears in the generated release notes
