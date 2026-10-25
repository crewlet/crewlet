# ADR-0051: Every authority row says what the assistant may do with it

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestEveryRowDecidesTheAssistant`, `internal/authz.TestAnAssistNeverRowRefusesAnAssistedPrincipal`, `internal/authz.TestAPresenceRowRefusesAnAssistedPrincipal`, `internal/authz.TestAReadGrantTakesOnlyAssistReadRows`, `internal/authz.TestTheAssistantIsNeverOneStepFromTheNeverList`, `internal/agent/builtin.TestAnAssistReadVerbIsAToolThatOnlyReads`, `internal/iamdomain.TestAnAdministrativeRecordIsNeverWrittenThroughTheAssistant`, `internal/iamdomain.TestATokenIsNeverMintedThroughTheAssistant`, `internal/api/configapi.TestACharterWriteIsAdmittedOnItsVerbOnlyForTheCharter`, `internal/api.TestAnApprovedPrinciplesChangeIsThePagesOwnWrite`
- **Tag-status:** unreleased

## The decision

**Every row of the authority table states what the Dashboard Assistant may do
with its verb**, in a field of its own beside `humanOnly`, `presence` and
`recency`: `AssistRead` (an assistant read grant may take it), `AssistApprove`
(only the one approved request an exec grant carries, ADR-0050) or
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
  credential, secret and setup verb, the company document's reads and writes
  (but for the one exception below), fleet operations, every identity
  directory write and proof, session control, every purge and transfer, nested
  model spend (`answer_knowledge`), seat-only verbs and bulk export, and every
  `assistant.*` verb itself.
- **The record refuses too.** `internal/iamdomain` refuses an administrative
  identity record and a token mint whose operator is an assistant grant,
  whatever authority reached it, as it already refuses one made through a
  machine token.

Layers only subtract. The person's own authority decision is still the whole
decision: no assistant field can admit what the person's grants and relations
refuse.

### The one exception: the company's principles

The company's mission, vision and standing policies are read and written on
verbs of their own, `config.principles.read` (`AssistRead`) and
`config.principles.write` (`AssistApprove`), decided for a person exactly as
`config.read` and `config.write` decide the whole document: an
administrator's, the write at the step-up window. They are the routes Company ›
Principles itself uses (`GET` and `PUT /config/charter`), so the assistant's
change and the person's Save are one write path, with no second route beside
it. The exception is narrow by construction:

- **The person holds the authority.** The assistant reads and proposes only as
  an administrator who could read and save the principles themselves.
- **The write is approval-gated with a server-rendered diff** (ADR-0050): the
  preview reads the principles as they are stored and shows a word diff of the
  mission and the vision and one row per policy added, removed or reworded,
  bound to the principles' entity tag; the approved write is sent under that
  tag as its `If-Match`, so a change made meanwhile is refused (`412`) rather
  than overwritten.
- **The step-up is kept.** The write asks the step-up window of the approver,
  through the assistant as at their keyboard.
- **The scope is the three settings and nothing else.** The charter resource
  is exactly `mission`, `vision` and `policies`: `GET /config/charter`
  answers those three fields, and `PUT` takes those three and refuses any
  other field, so no proposal can name another part of the document. A change
  the person asks for names the parts it changes, and the preview states the
  rest as they are stored. The charter write is admitted on its own verb only
  while what it changes is the charter: a draft
  that reached past it (another setting, a seat, a unit) is decided by the
  document's own rules, which the assistant is never admitted to. Every other
  company document verb (`config.read`, `config.write`, `config.org.read`,
  `config.org.write`) stays `AssistNever`, and the never-list walk pins the
  exception by verb.

Opening `config.write` instead would have put the assistant one approval from
every secret reference, provider, seat and integration in the document; a
page action into the Principles editor's draft alone left an explicit request
("remove this word from our mission") with no way to be carried out.

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
scoped and bound (ADR-0049, ADR-0050), and the class each verb is decided by,
which is the table's own doc.
