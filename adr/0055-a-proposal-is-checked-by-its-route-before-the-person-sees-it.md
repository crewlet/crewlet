# ADR-0055: A proposal is checked by its route before the person sees it

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestACheckIsTheWriteDecidedAtTheReadsProof`, `internal/api/configapi.TestEveryWriteThatReadsDryRunDeclaresIt`, `internal/assistant/catalog.TestAChangeTheRouteWouldRefuseIsNeverACard`, `internal/assistant/catalog.TestAWriteThatKeepsAHistoryCarriesTheLineThePersonApproved`, `internal/assistant/catalog.TestACreatesCardNamesWhereItLandsOrIsNoCard`, `internal/api.TestASeatTheUnitWouldRefuseIsRefusedBeforeThePersonSeesIt`, `internal/assistant/catalog.TestARoomMembershipChangeRemovesOnlyWhatItNames`
- **Cost-when-tried:** three proposals in one browser session reached the person, were approved and stepped up for, and were refused only then: a unit carrying seat fields the route does not take, the same unit with no revision summary, and a page whose card said "Your team's space" for a person whose team owns none
- **Tag-status:** unreleased

## The decision

**A Dashboard Assistant proposal is drawn only once the route it will be sent to
has said it would take it.** Three rules carry that, each declared where the
route is mounted rather than copied into the assistant:

- **A route that can check a write declares it** (`authz.Policy.Checked`): the
  same request with `dry_run=true`, judged in the same order, storing nothing.
  The preview of a change on such a route asks that check through the turn's
  read grant, with the exact body and precondition the approval would send. A
  refusal is the step's, in the route's words, and goes back to the model; no
  card is drawn. The check's warnings are rows on the card.
- **A check is the write's verb at the read's proof.** It is decided by every
  rule the write is (class, app, the assistant's column, presence) except the
  step-up, for the person and their assistant alike, because it changes
  nothing. A read grant carries a write's method only as such a check: the edge
  admits it only with `dry_run=true`, and the route's guard only where the route
  declares the check, so a route that would ignore the flag is never reached.
  The router marks the request (`authz.WithCheck`), so every decision its
  handler asks is decided as the same check.
- **A write that records a history line declares it** (`authz.Policy.Summarised`),
  and the assistant's change carries the line as `_summary` in the body, which
  the request digest binds: the call's own `summary`, else the body's, else one
  naming the route. The card shows it, and the revision records it.

**A replace removes only what the call says it removes.** A route whose PUT
stores its body as the whole object its GET answers declares it
(`authz.Policy.Replacing`): every PUT of the company document, its charter and
its entities. The preview compares the body with the object it read, and every
field the body would leave out or empty, or list member it would drop by its
identity, must be named in the call's `removes`. One that is not refuses the
step in plain words ("This would remove project and space from unit core"), so
the model keeps the field or the person's request is what removes it; a named
path the change does not remove is refused too. A declared removal leads the
card, above every row, never only as a struck-through row among the others.
The removals are computed from the body and the object the source revision
names, both bound by the digest, so the argument admits the call and the card
never echoes it. A field the address itself states (a unit's `id`) is the
route's to fill and is not a removal. A curated write that states a whole set
holds the same rule: a chat room's membership is sent whole, so a member the
call leaves out is taken out only where its `removes` names them, and the card
leads with everyone it takes out.

A catalogue write whose call leaves a place implicit (a task's project, a page's
container) is previewed with the place the write would default to, read from
`GET /viewer` through the one derivation the write applies, and that value is
pinned into the request. A person with no such place is refused at the preview
in the write's own words (`org.NoHomeProject`, `org.NoHomeSpace`).

## Why the obvious alternatives are wrong

**Validating the body in the assistant.** A second copy of the company
document's rules would drift from the route's the first time either changed,
and the person would again approve a change the route refuses. The route is
the one source of what it takes.

**Letting the read grant send any request carrying `dry_run=true`.** The flag
means nothing to a route that does not read it, so the grant whose whole
guarantee is that it changes nothing would write. ADR-0050 rejected exactly
this; the declaration at the mount, held to the handler by a behavioural walk,
is what makes it safe.

**Keeping the step-up on a check.** The person's own session could then not
check a change on an old sign-in either, and the assistant, asking as them,
would draw an unchecked card for every person who had not confirmed recently,
which is the failure this record removes. The step-up guards a consequence,
and a check has none.

**Drawing a removal as a struck-through row, and leaving the rest to the
person.** That is what the card did when a model rebuilding a unit to add a
seat left out the unit's project and space: two struck rows under twenty rows
of the new seat, approved as "add a seat". A removal nobody asked for is the
call's mistake, and the model is the one to correct it before the person sees
anything.

**Carrying the summary in `X-Summary`.** The header is outside the request
digest, so the history line the route records would not be the one the person
approved.

## What this does not decide

How a card renders a value (a list, multi-line text), what a route validates,
and which routes offer a check beyond the company document's writes. A route
that gains a dry-run form declares it at its mount, and the walk in
`internal/api/configapi` is the model for holding the declaration to the handler.
