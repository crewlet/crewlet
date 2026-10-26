# ADR-0054: A proposal is checked by its route before the person sees it

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestACheckIsTheWriteDecidedAtTheReadsProof`, `internal/api/configapi.TestEveryWriteThatReadsDryRunDeclaresIt`, `internal/assistant/catalog.TestAChangeTheRouteWouldRefuseIsNeverACard`, `internal/assistant/catalog.TestAWriteThatKeepsAHistoryCarriesTheLineThePersonApproved`, `internal/assistant/catalog.TestACreatesCardNamesWhereItLandsOrIsNoCard`, `internal/api.TestASeatTheUnitWouldRefuseIsRefusedBeforeThePersonSeesIt`
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
guarantee is that it changes nothing would write. ADR-0049 rejected exactly
this; the declaration at the mount, held to the handler by a behavioural walk,
is what makes it safe.

**Keeping the step-up on a check.** The person's own session could then not
check a change on an old sign-in either, and the assistant, asking as them,
would draw an unchecked card for every person who had not confirmed recently,
which is the failure this record removes. The step-up guards a consequence,
and a check has none.

**Carrying the summary in `X-Summary`.** The header is outside the request
digest, so the history line the route records would not be the one the person
approved.

## What this does not decide

How a card renders a value (a list, multi-line text), what a route validates,
and which routes offer a check beyond the company document's writes. A route
that gains a dry-run form declares it at its mount, and the walk in
`internal/api/configapi` is the model for holding the declaration to the handler.
