# ADR-0052: Every authority row says what the assistant may do with it

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestEveryRowDecidesTheAssistant`, `internal/authz.TestARowStatesItsNatureAndTheListAloneSaysNever`, `internal/authz.TestTheHumanOnlyListIsExactlyTheIntendedSet`, `internal/authz.TestAHumanOnlyRefusalSaysWhyInPlainWords`, `internal/assistant/catalog.TestAHumanOnlyGestureIsRefusedInThePersonsWords`, `internal/authz.TestAnAssistNeverRowRefusesAnAssistedPrincipal`, `internal/authz.TestAPresenceRowRefusesAReadGrantAndAdmitsAnApprovedOne`, `internal/authz.TestAReadGrantTakesOnlyAssistReadRows`, `internal/agent/builtin.TestAnAssistReadVerbIsAToolThatOnlyReads`, `internal/iamdomain.TestTheAssistantWritesNoSignInCeremony`, `internal/iamdomain.TestATokenIsNeverMintedThroughTheAssistant`, `internal/api.TestEveryGuardedRouteAdmitsTheAssistantAsItsPerson`, `internal/api.TestPermissionParityAcrossFivePrincipals`, `internal/api.TestTheAssistantRouteTableIsTheAppsOwn`, `internal/api.TestAHumanOnlyRouteIsNeverOffered`, `internal/api.TestAnAgentSeatIsAddedThroughTheGenericChange`, `internal/api/queries.TestAnAssistedQuestionIsAnsweredAsItsPersonsOwn`, `internal/api/configapi.TestACharterWriteThroughTheAssistantIsTheAdministratorsOwn`, `internal/api.TestAnApprovedPrinciplesChangeIsThePagesOwnWrite`, `internal/assistant/catalog.TestACallOnTheActRouteIsTitledByItsTool`
- **Tag-status:** unreleased

## The decision

**The Dashboard Assistant may do with the API whatever its person may, but for
a short human-only list.** Every row of the authority table states what its
verb is to the assistant, in a field of its own beside `humanOnly`, `presence`
and `recency`: `AssistRead` (a read a turn's read grant takes on the person's
behalf) or `AssistApprove` (a change it carries only as the one request the
person approved, ADR-0051). The zero is invalid, and the table's completeness
walk fails the build on a row that does not decide, exactly as it does for a
row with no recency.

`AssistNever` is spelled in ONE place: `assistantNever` in
`internal/authz/rules.go`, a verb and a reason per entry. A row states its
verb's nature and never Never, so the human-only set cannot widen by a row
nobody reads beside the list. Each entry also carries the sentence the person
is told when their assistant meets it, one per family, in plain words and
naming where they do it themselves ("Creating a person or an invitation, or
resetting somebody's password or second factor, hands out a way to sign in,
so it stays with you: use People in Settings."). The list holds four kinds of
verb and no other:

- **Sign-in and session ceremonies:** minting or revoking a machine token
  (`iam.credential.write`, whose mint answers the token in clear), changing how
  somebody proves who they are (`iam.credential.proof`), issuing a password or
  invitation link or clearing a second factor (`iam.signin.write`), and ending
  sessions (`iam.session.end`, `iam.invalidate`). Signing in, stepping up and
  enrolling are `/auth/`'s, which the request guard refuses a grant before
  routing.
- **A credential or a secret in clear,** in the answer or in the request body,
  where it would sit in a model's context, a transcript and a stored proposal:
  revealing a secret (`secrets.reveal`), writing one (`secrets.write`),
  entering an integration's vendor credentials (`setup.connect`) and a frame's
  view ticket (`artifacts.ticket`).
- **The assistant's own decisions:** every `assistant.*` verb, so it can never
  approve, decline or read its own conversations, and the request guard
  refuses a grant `/assistant/` besides.
- **An effect a grant would falsify or hide:** asking the knowledge base's own
  model (`answer_knowledge`, a model spend outside the turn's budget meter; the
  assistant searches the same pages itself), and an open frame's render report
  (`artifacts.ready`, which a grant that renders nothing would falsify).

**Why "add a seat" works and "add a person" does not.** A seat is a part of the
company document: adding one changes the org chart, and nothing in the answer
is a credential. Creating a person, or inviting one, answers a sign-in link
for them, a bearer credential in clear that would pass through the model, the
transcript and the stored proposal, so it stays with the person, who does it
from People in Settings.

The seat-only verb (`a2a_ask`) is on the list too, because no person takes it,
so parity keeps it from the person's assistant.

It is checked inside `authz.Decide`, beside the machine-token presence check
and before the class: a principal acting through an assistant grant on a
listed verb is refused `assist_refused`, and a read grant on an `AssistApprove`
row or on a row whose `presence` needs a person at the keyboard is refused
`assist_scope` (the remedy is a proposal the person approves, never a
permission). Both are asked before the class because no answer the chart could
give would lift either. The bar reads the credential's class, never its
spelling, and reads a zero class as never.

**A presence row is admitted to an approved exec grant.** `presence` keeps a
machine token out because a token proves nobody is at a keyboard. An exec grant
carries one request whose server-rendered preview the person approved from
their own browser, a moment ago, under their own session, Origin and step-up
(ADR-0051): that approval is the person at the keyboard, deciding that
request. A read grant carries no such gesture and is refused every presence
row. A machine token is still refused it, exactly as before.

**The record refuses the ceremonies too.** `internal/iamdomain` refuses an
enrolment (a first password link, or a redemption), an invitation and ending
every session whose operator is an assistant grant, whatever authority reached
it, and refuses a token mint through one, as it refuses both through a machine
token. Every other administrative record (an edit, a login or seat move, a
stage, a removal, a withdrawn invitation) is the administrator's own, approved.

**The model reaches the whole API through three generic tools** beside the
curated ones (internal/assistant/catalog): `describe_api` lists the routes the
person may use and answers one route's documentation from the API reference the
binary embeds, `api_request` reads any of them, and `propose_api_change`
proposes a change on any of them, which is always a proposal previewed from the
resource as it is now, with a request body of at most 64 KiB: the card shows the
fields the change moves, as rows every approver reviews, and the request itself,
its body redacted, is an administrator's technical details. A call names a route
of the table the API binds (`App.AssistantRoutes`), never a URL; a route whose
verb is on the list is not in that table, and every call is decided by the
route's own row again. The descriptions steer the model to a curated tool
wherever one fits, since its card and preview are shaped for the gesture, and a
call on the act route that a curated write makes whole is refused there, naming
the write: a tool's call shows only what it sends, where the write's preview
reads what it changes.

**Parity is walked route by route.** For every route the table binds and five
people, the assistant's admission (a read grant for a GET or a HEAD, an
approved exec grant for any other method) equals the person's, and differs on
exactly the human-only list; a route bound with no verb fails the walk unless
it is one of the kinds that decide inside their handlers (a question, a named
read, an overview read, the act route), each walked on its own. A GET mounted on
a write verb would be a read the assistant could not make for its person, so
none is: the dashboard source preview reads on `artifacts.source.read`.

### Why it still meets "no more access than the person"

- **The grant is the person's own session**, re-composed on every request
  (ADR-0050): no assistant field can admit what the person's own grants and
  relations refuse, and a sign-out ends the assistant at its next step.
- **Reads are what the person can read**, the same answer to the same request,
  redacted and fenced before the model reads it (ADR-0040).
- **Every write needs the person's approval of a server-rendered request**
  (ADR-0051): the card is the engine's reading of the resource, bound to its
  revision (an entity tag becomes the write's `If-Match`), and the exec grant
  carries that one request once.
- **Step-up is kept**: a row's recency is asked of the approver, through the
  assistant exactly as at their keyboard.
- **What a model must never hold is never handed to it**: the list keeps every
  credential in clear and every sign-in ceremony away, whoever approved what.

### The company's principles

The mission, vision and standing policies keep verbs of their own,
`config.principles.read` and `config.principles.write`, decided exactly as
`config.read` and `config.write` decide the whole document. They were once the
one part of the company document the assistant could reach; the document is now
the administrator's own through their assistant like every other route, and the
charter route stays the narrower write Company › Principles saves through, with
its word-diff preview and its entity tag.

## Why the obvious alternatives are wrong

**A curated allow-list of verbs, every other verb kept from the assistant.**
That was this decision's first form, and it left an administrator asking for a
seat on their own org chart told the change was not one the assistant could
propose. A narrow list does not make an approval safer: the approval and the
grant's narrowing are what make a write safe, and a verb the person may take
behind their own approval is no wider through the assistant.

**A list of verbs the assistant may not take, kept beside the table.** A new
verb would then be decided by nobody. The row still states the verb's nature,
the walk makes that mandatory, and the human-only list is pinned verb by verb by
a walk that holds its families (`assistant.`, `iam.credential.`,
`iam.session.`, `iam.signin.`) as well.

**Deciding in the assistant's catalogue alone.** The catalogue only chooses
what the model is offered; a request the assistant sends anyway, a route that
reaches a second verb from inside its handler, or a catalogue row added by
mistake would then be decided by nothing. The table is what every surface and
every tool already asks.

**Treating a presence row as never the assistant's.** It made every directory
edit, every purge and every brand adoption unreachable although the person
approves each one at their keyboard, step-up included. What presence guards
against, a credential acting with nobody there, an exec grant cannot be.

## What this does not decide

Which tools the model is offered and how a call is shaped
(`internal/assistant/catalog`), how a grant is scoped and bound (ADR-0050,
ADR-0051), and the class each verb is decided by, which is the table's own doc.
