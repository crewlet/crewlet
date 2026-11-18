---
paths:
  - "cmd/**"
  - "internal/api/**"
  - ".claude/worktrees/*/cmd/**"
  - ".claude/worktrees/*/internal/api/**"
---

# The Binary and Its Commands
```
cmd/crewlet/          # Its commands: run / validate / schema / migrate / budgets /
                      #   backup / retention / worklet / objects / fleet / feeds /
                      #   doctor / seats / artifacts / chatlet / secrets / config /
                      #   iam / llm / search / assistant / gitlet, the six vendor
                      #   CLIs (gitlab, github, jira, slack, confluence, mattermost)
                      #   and mcp-relay (started by a seat's coding harness, not a
                      #   person)
```

**A CLI command that talks to a running node authenticates with `CREWLET_API_TOKEN` only.**
- Never a flag: a token argument lands in shell history and `ps`, so `-token` is an unknown flag.
- Never Tier A's token list: that is what a node accepts, not a wallet for the harness.
- The one exception is `iam token -login`: it signs in as the caller for that one request to mint their own token, then signs out. The password and any second-factor code are read from the terminal or piped lines, never a flag.
- `crewlet mcp-relay` is not an exception, because it is no client of a node's API: it is the stdio end of a seat's tool session, started by a harness and carrying no credential. The engine's own `0700` socket directory admits it.
- A cookie write states `api.external_url`'s origin; a bearer write states none.
