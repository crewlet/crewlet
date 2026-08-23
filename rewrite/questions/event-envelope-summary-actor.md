# q — `Summarizer` / `Actorer` cannot see the envelope

Status: **open** · Raised porting `src/crewlet/events/types.py` → `go/internal/events/types/`
· Contract: `go/internal/events/event.go`

Nothing was changed in `event.go`. This records where the Python catalogue does not
fit the Go contract, so the answer is decided once rather than re-derived per event.

## What Python does

`Event.actor` is a getattr chain over the *whole* event — envelope included:

```python
for field in ("role", "source"):     # payload field, then ENVELOPE field
    if val: return val
return getattr(self, "agent_id", "") or "system"
```

and almost every subclass's `summary` interpolates that value:

```python
return f"{self.actor} created task '{self.title}' for {self.target_role}"
```

Pydantic merges envelope and payload into one object, so `self.actor` is available
to `summary` for free. Go splits them: `Payload` is a separate value and both
`Summarizer.Summary() string` and `Actorer.Actor() string` are handed nothing.

## The two gaps

**1. The `agent_id` tail is unreachable.** A payload returns `""` from `Actor()` to
defer, and the envelope then answers `Source` or `"system"`. It cannot say "use
`agent_id`, but only if `source` is empty" — that decision needs both halves.
Affects every event with `agent_id` and no `role` (`agent_reassigned`,
`task_delegated` has neither, `external_notification` after its `sender` override).
Only observable when `role` and `source` are BOTH empty, which is rare in practice:
the engine's publishers stamp `source`.

**2. An actor-led `summary` cannot name a source-derived actor.** ~30 of the 60
events open their summary with `self.actor`. Where the payload carries `role` this
ports exactly. Where it does not — `task_created`, `task_delegated`,
`document_created`, `document_updated`, `provider_fallback`, `compaction_requested`,
`compaction_completed`, `subagent_batched`, `message_sent` and `org_started` /
`org_stopped` without a name — the actor lives only in the envelope's `source` and
the Go payload has no way to reach it. `provider_fallback` / `compaction_*` /
`subagent_batched` carry an `agent_handle` right there in the payload, but Python's
chain does not look at it, so using it would change behaviour rather than port it.

## What the port does meanwhile

`lead(actor, phrase)` in `go/internal/events/types/types.go`: with an actor it
renders Python's exact string; without one it capitalises the verb phrase, so
`task_created` reads `Created task 'Build API' for Engineer` instead of leading
with a blank. No detail is lost, only the name. Returning `""` (the contract's
documented "defer to the default") was the alternative and is worse — it collapses
the whole line to `Task Created`.

## The fix, if wanted

One added interface, no change to the existing two:

```go
// EnvelopeSummarizer renders a summary that needs envelope fields.
type EnvelopeSummarizer interface{ SummaryFor(*Event) string }
```

`Event.Summary()` would try it before `Summarizer`, and an `EnvelopeActorer` would
do the same for `Actor()`. That restores both behaviours exactly and keeps the
plain `Summarizer` for payloads that need nothing. The cost is that a payload can
then reach the whole envelope, which is the coupling the current contract avoids;
passing just the resolved actor (`SummaryFor(actor string) string`) buys the same
result with a narrower hole, but does not fix gap 1.
