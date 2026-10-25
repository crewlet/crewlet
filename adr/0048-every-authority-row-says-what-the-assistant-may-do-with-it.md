# ADR-0048: Every authority row says what the assistant may do with it

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestEveryRowDecidesTheAssistant`, `internal/authz.TestAnAssistNeverRowRefusesAnAssistedPrincipal`, `internal/authz.TestAPresenceRowRefusesAnAssistedPrincipal`, `internal/authz.TestAReadGrantTakesOnlyAssistReadRows`, `internal/authz.TestTheAssistantIsNeverOneStepFromTheNeverList`, `internal/agent/builtin.TestAnAssistReadVerbIsAToolThatOnlyReads`, `internal/iamdomain.TestAnAdministrativeRecordIsNeverWrittenThroughTheAssistant`, `internal/iamdomain.TestATokenIsNeverMintedThroughTheAssistant`
- **Tag-status:** unreleased

## The decision

**Every row of the authority table states what the Dashboard Assistant may do
with its verb**, in a field of its own beside `humanOnly`, `presence` and
`recency`: `AssistRead` (an assistant read grant may take it), `AssistApprove`
(only the one approved request an exec grant carries, ADR-0047) or
`AssistNever`. The zero is invalid, and the table's completeness walk fails the
build on a row that does not decide, exactly as it does for a row with no
recency.

- **Checked inside `authz.Decide`**, beside the machine-token presence check
  and before the class: a principal acting through an assistant grant on an
  `AssistNever` row, or on any row whose `presence` needs a person at the
  keyboard, is refused, and the grant's scope refuses an `AssistApprove` row
  under a read grant. Both refusals name a reason of their own
  (`assist_refused`, `assist_scope`), so an assistant is told it is the
  assistant rather than that the person lacks a permission they plainly hold.
  Both are asked before the class because no answer the chart could give
  would lift either: asked after it, a node that could not read its chart
  would answer "ask me again" to a grant that can never take the verb. The
  bar reads the credential's class, never its spelling, and reads a zero
  class as never.
- **`AssistNever` covers what a model must never be one step from**: every
  credential, secret and setup verb, the company document's writes, fleet
  operations, every identity directory write and proof, session control, every
  purge and transfer, nested model spend (`answer_knowledge`), seat-only verbs
  and bulk export, and every `assistant.*` verb itself.
- **The record refuses too.** `internal/iamdomain` refuses an administrative
  identity record and a token mint whose operator is an assistant grant,
  whatever authority reached it, as it already refuses one made through a
  machine token.

Layers only subtract. The person's own authority decision is still the whole
decision: no assistant field can admit what the person's grants and relations
refuse.

## Why the obvious alternatives are wrong

**A list of verbs the assistant may not take, beside the table.** A new verb
would then ship open to the assistant by default, looking exactly like a verb
somebody decided to allow. A field on the row is decided where the verb is
declared, and a walk makes the decision mandatory.

**Deciding in the assistant's catalogue alone.** The catalogue only chooses
what the model is offered; a request the assistant sends anyway, a route that
reaches a second verb from inside its handler, or a catalogue row added by
mistake would then be decided by nothing. The table is what every surface and
every tool already asks.

## What this does not decide

Which tools the model is offered (`internal/assistant/catalog`), how a grant is
scoped and bound (ADR-0046, ADR-0047), and the class each verb is decided by,
which is the table's own doc.
