# ADR-0180: A seat job or helper may run on a fleet of one model class, written `fleet:<key>`, and every company has default-fleet

- **Status:** accepted
- **Authority:** `docs/concepts/llm-fleets.md`
- **Enforced-by:**
  - `internal/config.TestAFleetReferenceIsAcceptedWhereOneKeyIsAndRefusedInAList`
  - `internal/config.TestEveryFleetRuleRefusesWhereWrittenInItsWords`
  - `internal/config.TestDefaultFleetIsSynthesisedAndNeverServedUnwritten`
  - `internal/config.TestAPinnedSeatIsUnchangedByFleets`
  - `internal/api/configapi.TestDefaultFleetCannotBeDeletedOrRenamed`
  - `internal/api/configapi.TestDefaultFleetIsWrittenOnlyOnceChanged`
- **Tag-status:** unreleased

## The decision

**A fleet is a named set of `providers.llm` entries of one model class on one harness** (`providers.llm_fleets`), and `fleet:<key>` is accepted wherever one entry key is: a seat's `llm` in its single form, each `llm` mapping value, each `llm_<phase>`, `assistant.model` and `advisor.model`. It is refused inside a list, since a fleet replaces the hand-ordered fallback list. A field naming an entry key or a list is pinned and resolves exactly as before. `config` holds the shape and the references; resolution, the pick and the readings belong to the runtime, which asks `config.Company.FleetMembers`.

**default-fleet always exists.** Every read and validation sees it, synthesised as an empty balanced fleet until a write changes it; `GET /config` never serves it while synthesised, so an operator's rendered document and the engine's never drift on it. It is never deleted and never renamed. An empty fleet is valid and a seat job or helper may be set to one: it waits, warned of and never refused.

## Why the obvious alternatives are wrong

**A fleet as a longer list in `llm`** keeps the order fixed by the author, so the first entry is spent first on every seat, which is the imbalance fleets exist to remove; and a list cannot say which entries are interchangeable.

**Members of mixed classes** would let the pick move a seat's work to a weaker model without anyone choosing it.

**Writing default-fleet into every document** would change every stored revision on upgrade and make a rendered document differ from the one read back.

## What this does not decide

How a balanced fleet picks a member, how its readings age, and what a pick records: the runtime's package doc holds those.
