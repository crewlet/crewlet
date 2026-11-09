# ADR-0115: A setup finding carries its fix, and only a person sends it

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:** `internal/advisor.TestTheAdvisorImportsNoWritePath`, `internal/advisor.TestEveryOptionOfEveryChangeClearsItsFindingAndBreaksNoRule`, `internal/advisor.TestEveryClassHasWordsAndAFix`, `internal/advisor.TestTheLiveShapeRaisesExactlyTheStaticClasses`, `internal/config.TestEveryAdvisoryClassIsValidAndRaisedSomewhere`
- **Tag-status:** unreleased

## The decision

**A company's setup is advised about in one list, and every fix is a change
an administrator previews and sends.** `GET /advisor` (administrators only)
answers every finding about the fleet's company at the revision it is: what is
wrong, why it costs the company, where in the document it sits, and its fix.

- **Detection is decided where the setting is.** A finding about the company
  document is a `config.Warning`, so the check that raises it answers
  `crewlet validate`, every `/config` write and every apply log too. A
  warning that carries a `config.AdvisoryClass` gets a fix the advisor
  builds; one that carries none is still listed, in its own words, with the
  setting it is about.
- **A fix is data, never an act.** A configuration fix is one entity write per
  option: the entity, its id, the top-level fields to set and the revision
  summary. The dashboard sends it to the entity route's dry run (ADR-0055) and
  draws the preview from that answer, then sends it for real from the
  administrator's own session, under the revision the finding was read at
  (`If-Match`), so a document that moved in between is refused rather than
  overwritten. `internal/advisor` imports no store, coordination, state log
  or API package, so it has no way to send one.
- **The answer is read off the redacted document**, so no change a fix carries
  can hold a value redaction hides.

## Why the obvious alternatives are wrong

**Applying a fix the engine is sure of.** A grant, a model, a schedule or a
policy is the company's decision even when the evidence is plain: the agent
that gets the health grant is the one the company trusts with the engine's own
state, and a schedule that looks idle may be a promise to a customer. A change
nobody pressed is also one nobody reviewed.

**Building the advice as a second pass over the document.** The checks would
then exist twice, once for `crewlet validate` and once for the advisor, and a
company could pass one and fail the other.

**Previewing a fix from the advisor's own description of it.** The card would
then show the advisor's claim about the change rather than the server's reading
of it. The entity route's dry run is the same check the real write runs.

## What this does not decide

Findings read from what a company has done (its turns, refusals and schedule
runs), dismissing a finding, and where the dashboard shows the list: each
lands with the pieces that build it.
