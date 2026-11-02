# ADR-0045: Every model is reached through its vendor's coding CLI

- **Status:** accepted
- **Authority:** `internal/providers/llm`
- **Enforced-by:** `internal/providers/llm.TestNoModelIsReachedOverAVendorsHTTPAPI`
- **Measured:** the two HTTP backends this replaced, with their shared HTTP layer and the API-key pool behind them, were 29 files and 15,712 lines, against one CLI backend every providers.llm entry already had
- **Cost-when-tried:** each vendor's request shape had to be re-learned for every model it released, and was carried here as rules that went stale on the next one: a table of Claude model ids and the thinking, effort and sampling dials each accepts (an unknown id was a validation advisory, because the table was always behind), reasoning blocks replayed per vendor in the shape each demanded, an OpenAI-compatible dialect per gateway, and a pool of API keys with a fleet-wide cooldown ledger in the coordination store to rotate them — all of it a second, permanently trailing copy of what the vendors ship in their own CLIs
- **Tag-status:** unreleased

## The decision

**The engine holds no HTTP client to a model vendor.** Every `providers.llm`
entry names a coding CLI — Claude Code for Anthropic's models, on a plan or an
API key; Codex for OpenAI's; OpenCode for open models, on any provider it
speaks to or a local server — and every model call runs that CLI, through
`internal/providers/llm/cliagent`. A second vendor or a second model is a
second entry, never a second backend.

**One vendor SDK stays, for one thing:** `github.com/openai/openai-go` is the
embeddings client (`providers.embeddings`), because no CLI computes an
embedding and the OpenAI embeddings shape is what every embeddings host
serves. No other package imports it, and no package imports any other model
vendor's SDK.

## Why the obvious alternative is wrong

**An HTTP client per vendor, beside the CLIs**, is the shape this replaced.
It is a copy of the vendor's harness that can only trail it: the vendor
changes how a model takes its thinking budget, which blocks must be replayed,
what is cached and which sign-in bills which account, and ships that in its
CLI the same day; a client here learns it from a failed request. The entry
then has two backends that disagree about what a field means, and a seat's
behaviour depends on which one its entry happened to name.

**One generic OpenAI-compatible client for everything** removes the copies by
removing what makes each model work: it cannot replay a vendor's reasoning,
cannot use a subscription sign-in, and leaves every vendor-specific dial to an
`extra_body` nobody validates. OpenCode already is that client, maintained by
people who track every provider it speaks to, and it runs as a CLI like the
rest.

## What this does not decide

It does not decide **which CLIs are supported**: that is the profile table in
`internal/providers/llm/cliagent/cliprofile`, and adding one is a profile, not
a backend. It does not decide **how a CLI is driven** — on this host or in a
box, the per-seat homes — which is `cliagent`'s own doc, nor how a phase's
tools reach it, which is [ADR-0049](0049-a-phases-tools-reach-its-cli-as-one-mcp-server.md). And it does not touch
**embeddings**, whose client stays an SDK for the reason above.
