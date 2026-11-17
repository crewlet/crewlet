---
paths:
  - "cmd/**"
  - "internal/api/**"
  - ".claude/worktrees/*/cmd/**"
  - ".claude/worktrees/*/internal/api/**"
---

# The Binary and Its Commands
```
cmd/crewlet/          # The one binary: run / validate / schema / migrate / budgets /
                      #   backup / retention / worklet / objects / fleet / feeds /
                      #   seats / artifacts / chatlet / secrets / config / iam / llm /
                      #   search / assistant / gitlet, the six vendor CLIs (gitlab,
                      #   github, jira, slack, confluence, mattermost) and mcp-relay,
                      #   which a seat's coding harness starts rather than a person.
                      #   Every command the switch dispatches must appear in usage():
                      #   nothing connects them, a test asserts it
```

**A CLI command that talks to a running node authenticates with `CREWLET_API_TOKEN` and nothing else** — never a flag (a token typed as an argument lands in shell history and `ps`, so `-token` is an unknown flag), and never Tier A's token list, which is what a node accepts rather than a wallet for the harness. The one exception is `iam token -login`, which mints the caller's own token by signing in as them for that one request (the password and any second-factor code read from the terminal or piped lines, never a flag) and signs out after. `crewlet mcp-relay` is no exception, because it is not a client of a node's API: it is the stdio end of a seat's tool session, which a harness starts and which carries no credential at all — the engine's own `0700` socket directory is what admits it. A cookie write states `api.external_url`'s origin; a bearer write states none.
