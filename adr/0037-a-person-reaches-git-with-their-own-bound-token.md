# ADR-0037 — A person reaches git with their own bound token, and git is the third person transport

- **Status:** accepted
- **Authority:** `internal/api/auth`
- **Enforced-by:** `internal/api/auth.TestGitAccessIsTheOneClassifier`, `internal/api/auth.TestBasicIsAcceptedOnlyOnGitRoutes`, `internal/api/auth.TestTheGitPrefixesAreGuardedEvenForReads`, `internal/code/host.TestAnUnboundTokenCannotPush`, `internal/code/host.TestEveryPushIsAuditedAsItsPerson`, `internal/api.TestGitRoutesAreNotPublished`, `internal/api.TestADrainingNodeFinishesAFetchAndBeginsNoGitWork`
- **Measured:** git sends its first request with no credential and asks its helper only after a `401` carrying `WWW-Authenticate: Basic` (git 2.43); an `ERR` pkt-line after the service header prints as `fatal: remote error: <sentence>` for protocol v0, v2 and a push, and a synthesized report-status prints per ref as `[remote rejected] (<sentence>)`; git sends a 4-byte probe (`0000`) before any push larger than `http.postBuffer`, and a report written before the push's body is read is lost to a broken pipe — a 5 MB push printed `RPC failed; curl 55 Send failure: Broken pipe` and none of the report until the body was drained first (git 2.43); a push's advertisement carries no `Git-Protocol` header with protocol v2 configured
- **Cost-when-tried:** not tried here: no build mounted `/git/` on `api.public` or let a token bound to nobody push, and `TestGitRoutesAreNotPublished` and `TestAnUnboundTokenCannotPush` were written before either could be. The nearest record is ADR-0024's, whose cost was observed: a dashboard that could not act as the person its token named wrote nothing at all, and every change was a tool call a person copied into an assistant by hand
- **Tag-status:** unreleased

## The decision

Smart HTTP at `/git/<name>.git` on `api.port` is a PERSON transport beside the act route and the operator's
MCP (ADR-0024 is extended, not changed). `/git/` is always guarded; the credential is a Tier A API token,
presented as the HTTP Basic password on git routes only (the bearer header works too). A token bound to a
human seat (`contact.crewlet_operator_id`) pushes AS THAT PERSON; a token bound to nobody, and the anonymous
caller of a disabled guard, fetches only. Every push is audited as one `operator_acted` with transport `git`,
the token as the actor and the bound person beside it; a fetch is a read and is not. Which requests are
fetches and which are pushes is decided by ONE function, `auth.GitAccess`, which the guard, the drain gate
and the host all ask — a fetch already begun is served through a drain, a new one and a push are refused
like every other new work. A refusal on a git route is `text/plain` (`auth.GitText`), because git prints an
HTTP refusal's body only in that form; the guard, the drain gate and the host write it through one function.

## Why the obvious alternative is wrong

Two obvious alternatives, both tried on paper: a separate git credential (an SSH key or a deploy token
registry) would be a second identity system for one person, with its own revocation and its own audit; and
treating any valid token as a pusher would make the commit author a client-chosen string with no person
behind it. The bound token is already how this engine knows who a person is.

## What this does not decide

How a seat's box reaches git (a run token in a key domain of its own, served where a box can reach it),
per-repository read grants (derived from the chart), and SSH (not planned).
