# ADR-0055: A person reaches git with their own token, and git is one more way a person acts

- **Status:** accepted
- **Authority:** `internal/api/auth`
- **Enforced-by:** `internal/api/auth.TestGitAccessIsTheOneClassifier`, `internal/api/auth.TestBasicIsAcceptedOnlyOnGitRoutes`, `internal/api/auth.TestTheGitPrefixesAreGuardedEvenForReads`, `internal/code/host.TestAnUnboundTokenCannotPush`, `internal/code/host.TestEveryPushIsAuditedAsItsPerson`, `internal/code/host.TestEveryPushTheNodeAdmitsIsRecordedHoweverItEnds`, `internal/api.TestGitRoutesAreNotPublished`, `internal/api.TestADrainingNodeFinishesAFetchAndBeginsNoGitWork`
- **Measured:** git sends its first request with no credential and asks its helper only after a `401` carrying `WWW-Authenticate: Basic` (git 2.43); an `ERR` pkt-line after the service header prints as `fatal: remote error: <sentence>` for protocol v0, v2 and a push, and a synthesized report-status prints per ref as `[remote rejected] (<sentence>)`; git sends a 4-byte probe (`0000`) before any push larger than `http.postBuffer`, and a report written before the push's body is read is lost to a broken pipe — a 5 MB push printed `RPC failed; curl 55 Send failure: Broken pipe` and none of the report until the body was drained first (git 2.43); a push's advertisement carries no `Git-Protocol` header with protocol v2 configured
- **Cost-when-tried:** not tried here: no build mounted `/git/` on `api.public` or let a token bound to nobody push, and `TestGitRoutesAreNotPublished` and `TestAnUnboundTokenCannotPush` were written before either could be. The nearest record is ADR-0024's, whose cost was observed: a dashboard that could not act as the person its token named wrote nothing at all, and every change was a tool call a person copied into an assistant by hand
- **Tag-status:** unreleased

## The decision

Smart HTTP at `/git/<name>.git` on `api.port` is a PERSON transport beside the act route, the `/work` and
`/pages` routes and the operator's MCP (ADR-0024 is extended, not changed). `/git/` is guarded like every
route; the credential is a token presented as the HTTP Basic password on git routes only (the bearer header
works too): a person's own machine token, which acts as its owner, or a Tier A token the identity directory
binds to a seat. A credential acting as a human seat pushes AS THAT PERSON — every person holds one
(ADR-0032) — and a credential bound to no seat (a Tier A token or a service account the directory binds to
none) fetches only. Every push is audited as one `operator_acted` with transport `git`, the caller exactly as every write
names it (the seat, its kind and the credential beside it); a fetch is a read and is not. Which requests are
fetches and which are pushes is decided by ONE function, `auth.GitAccess`, which the guard, the drain gate
and the host all ask — a fetch already begun is served through a drain, a new one and a push are refused
like every other new work. A refusal on a git route is `text/plain` (`auth.GitText`), because git prints an
HTTP refusal's body only in that form; the guard, the drain gate and the host write it through one function.

## Why the obvious alternative is wrong

Two obvious alternatives, both tried on paper: a separate git credential (an SSH key or a deploy token
registry) would be a second identity system for one person, with its own revocation and its own audit; and
treating any valid token as a pusher would make the commit author a client-chosen string with no person
behind it. The token a person already holds is already how this engine knows who they are.

## What this does not decide

How a seat's box reaches git (ADR-0063: its run's token in a key domain of its own, on `/box/git/`, served where a
box can reach it),
what a seat may read and push on a repository (derived from the chart: ADR-0058, which leaves this record's rules
for persons and tokens as they are), and SSH (not planned).
