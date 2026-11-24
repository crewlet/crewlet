# LLM Fleets

A fleet is a named set of [`providers.llm` entries](llm-backends.md#configuration-reference) of one model class, such as three Claude plans that all run Opus. A seat job or a helper set to `fleet:<key>` runs each new piece of work on whichever member has room, so seats spread across sign-ins instead of each waiting at its own entry's limit. The decision is [ADR-0180](../../adr/0180-a-seat-job-may-run-on-a-fleet-of-one-model-class.md).

```yaml
providers:
  llm:
    claude-max:  { model: opus, effort: high }
    claude-team: { model: opus, effort: medium, state_dir: /var/lib/crewlet/llm-harness/team }
    claude-api:  { model: opus, auth: { mode: api-key } }
  llm_fleets:
    claude-opus:
      name: Claude Opus
      strategy: balanced                            # or sequential
      members: [claude-max, claude-team, claude-api] # the sequential order
roles:
  - name: Engineer
    llm: fleet:claude-opus          # every job of the seat
    llm_review: fleet:default-fleet # or one job
assistant: { model: fleet:claude-opus }
advisor:   { model: claude-max }    # pinned to one entry, as before
```

## The fleet

| Field | Meaning |
|---|---|
| key | Its address and reference, `fleet:<key>`: lowercase letters, digits and `-`, starting and ending with a letter or a digit, at most 63 characters. It never changes. |
| `name` | Required, 1 to 60 characters, unique across fleets ignoring case. A rename changes this only. |
| `strategy` | `balanced` (absent) or `sequential`, [below](#strategies). |
| `members` | `providers.llm` keys, each once, of one model class on one harness. Their effort and sign-in may differ. An entry may be in several fleets. Empty is valid. |
| `threshold_points` | Balanced only: 1 to 50, absent 5. |

The class is read from the entry's model: Claude Opus, Claude Sonnet, Claude Haiku, GPT-5 Codex, else the model id itself. The first member sets it, and a member of another class or harness is refused ("Runs Claude Sonnet; this fleet runs Claude Opus."): any member must be able to take any of the fleet's seats' work.

## The reference

`fleet:<key>` is accepted wherever one entry key is: `llm` in its single form, each value of the `llm` mapping, each `llm_<phase>`, `assistant.model` and `advisor.model`. It is refused inside a list ("A fleet stands alone: choose the fleet or a list of models"): a fleet replaces a hand-ordered [fallback list](llm-backends.md#falling-back-between-entries). A key that names no fleet is refused at the field. A seat set to an entry key, or a list of them, is **pinned**: it runs exactly as before and waits at its model's limit.

A helper on a fleet holds every member to the [helper rule](../guides/assistant.md), and the engine builds a helper's run of each.

## `default-fleet`

Every company has `default-fleet`, `{name: default-fleet, strategy: balanced, members: []}`, synthesised by the engine: every read and validation sees it, while `GET /config` serves it only once a write changed it, so a document that never touched it reads back as written. It is never deleted (`409 not_removable`) and never renamed. New seats and helpers are written `fleet:default-fleet` unless pinned.

## A fleet with no members

Valid, `default-fleet` included, and a seat job or helper may be set to one. It cannot run: its inbox is held as a [parked seat's](llm-backends.md#falling-back-between-entries) is, with no probe, its status reads `No members yet`, and the first member to join releases it. Validation warns once per seat or helper on each such fleet ("default-fleet has no members yet: Engineer waits until one joins") and refuses nothing.

## Strategies

A member takes new work while every limit reading of its sign-in is under its warning level and it is not [sitting out](llm-backends.md#falling-back-between-entries). A plan is a member signed in with `auth.mode: subscription`; its share is the higher of its session and weekly usage, in whole points. Keys, cloud accounts and gateways report no usage, so their share is 0.

- **Sequential** fills the first member with room, in `members` order, then the next.
- **Balanced** gives new work to the plan with the lowest share, ties to the one that served least recently. A seat job stays on the member that served its last turn while that member is within `threshold_points` of the lowest, so a follow-up keeps its cache. Keys, cloud accounts and gateways take work only when no plan has room.

Balancing never moves a running turn; a limit or a refused sign-in does. When no member has room the fleet's seats wait as a parked chain does, with one probe at the earliest reset among the members.

## Changing fleets

Fleets are written through [`/config/llm-fleets/{key}`](../reference/api-endpoints.md#per-entity-read-and-write) or any whole-document write. Creating a fleet moves no seat. Removing one repoints every seat job and helper on it in the same write ([`DELETE /config/llm-fleets/{id}`](../reference/api-endpoints.md#delete-configllm-fleetsid-remove-a-fleet-and-repoint-what-runs-on-it)), and removing an entry a fleet lists answers the fleet too: the repoint drops it from the members or replaces it.
