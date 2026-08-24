# q — `Summarizer` / `Actorer` cannot see the envelope

Status: **resolved** — the contract was extended; `internal/events/event.go` and
`internal/events/types/` both carry the answer. Raised porting
`src/crewlet/events/types.py`.

## What was wrong

`Event.actor` in Python is a getattr chain over the *whole* event — payload and
envelope in one object:

```python
for field in ("role", "source"):     # payload field, then ENVELOPE field
    if val: return val
return getattr(self, "agent_id", "") or "system"
```

and ~30 of the 60 subclasses interpolate that value into `summary`
(`f"{self.actor} created task '{self.title}' for {self.target_role}"`).

Go splits envelope from payload, and the original contract handed `Summarizer`
and `Actorer` nothing. Two gaps followed: the `agent_id` tail was unreachable
(a payload returns `""` to defer, and cannot say "use my agent id, but only if
`source` is empty"), and an actor-led summary on a payload with no `role` field
could not name the actor at all.

## The resolution

`event.go` now owns the whole chain — override → payload role → envelope source
→ payload agent id → `"system"` — and payloads contribute single facts through
narrow optional interfaces: `Roler`, `AgentIdentified`, `Actorer` (an outright
override), plus `ActorSummarizer`, whose `SummaryFor(actor string)` is handed the
already-resolved actor.

`SummaryFor(*Event)` was rejected deliberately: handing sixty payloads the whole
envelope lets each re-derive the chain, and the moment two of them disagree the
same turn reads differently on two surfaces.

## What the catalogue does with it

- `Role()` on all 32 payloads carrying a `role` field, `AgentID()` on all 33
  carrying an `agent_id` field, so the tail is reachable everywhere Python's
  getattr reached it. `Actor()` survives on `ExternalNotification` alone, where
  the actor is genuinely neither the role nor the publisher but the human who
  sent the message.
- The ~30 actor-led summaries are `SummaryFor` and render the Python string
  exactly, actor included. `lead()` remains, no longer as a workaround but as the
  renderer for an actor-led line; a handful of lines pass a party the payload
  names itself (an A2A channel's requester, a message's own sender) instead of
  the resolved actor.
- **Naming consequence, worth knowing before reading the structs:** Go forbids a
  field and a method sharing a name, so the fields carrying `role` and
  `agent_id` are `RoleName` and `Agent`. The JSON tags are unchanged; `Role()`
  and `AgentID()` are how anything reads them.

Pinned by `TestActorChain` (each link, in order), `TestPayloadsContributeWhatTheyKnow`
(derived from the wire tags, so a new event carrying a role and forgetting
`Roler` fails rather than quietly attributing its turns to whatever published
them) and `TestEverySummaryIsSpoken`.
