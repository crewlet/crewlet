# ADR-0068: A check is a measurement of a commit, defined by the target branch and run where no author can reach it

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code/home.TestCheckDefinitionsComeFromTheTarget`, `internal/code/home.TestATipCheckIsDefinedAtItsLandingsBase`, `internal/code/home.TestARedTipIsBisectedOverLandingRows`, `internal/code.TestTipChecksRunOnAPersonsDirectLanding`, `internal/code/checkrun.TestACheckBoxHoldsNoModelCredential`, `internal/code/checkrun.TestACheckSpendsNoTokens`, `internal/code/checkrun.TestALosingClaimStartsNoBox`
- **Tag-status:** unreleased

## The decision

A check is a MEASUREMENT OF A COMMIT for a target branch: one row a
(repository, commit, target, check, attempt) on the repository's checks
subject (`code.CheckQueued`), whatever asked for it — a change's head, a
queue's candidate, a protected branch's tip or a bisect's probe — so a
fast-forward landing is judged by the attempts its change's head already
recorded, and a merge gate, a green row and a broken record all read the same
rows (`code.GateCheck`).

What a check RUNS is defined by the target branch, never by the commit it
measures. A change's head is dispatched under `.crewlet/checks.yaml` at its
target's tip; a queue's candidate under the file at its train's base; and a
protected branch's tip, and every probe of a bisect, under the file at the
BASE of the first landing above the branch's green row — the green itself, or
before any tip passed, the commit the first landing moved the branch from. The
attempt records the blob it was defined from (`defs_oid`) and carries its
definition whole, so a runner runs what was decided, whatever the file says by
then. A file that does not run, or that defines no check the gate requires,
errors that check at its dispatch saying so, and nobody runs it.

A check runs in a box on a git node, or in a remote cell, created for the
attempt after a git node CLAIMS its row: never on the engine host (`direct`,
`self`), never with a model's credential or a token budget — its results are
durations and exit codes — and fetching only the one commit through a token
its own row admits (`chk~…`, key domain `crewlet.git.v1`).

Every protected tip is checked. A landing whose record did not prove its
target's required checks — a merge a person's override landed, a person's own
push — lies above the target's green row; the repository's home dispatches the
target's NEWEST landing only (coalesced), moves the green row onto it once
every required check passed, and where one failed, BISECTS the failed checks
over the landing rows down to the first landing they failed on, which it
records broken (`code.Green`, `code.Broken`, `code_target_broken`).

## Why the obvious alternative is wrong

The obvious alternative reads the checks file at the commit being measured, as
most hosted CI does: the workflow file lives in the branch. Then a change that
edits the file — drops a step from `unit`, makes `lint` exit 0 — is judged by
the check it rewrote, and the gate that required `unit` counts a `unit` that
no longer tests anything. Reading the file at the TARGET's tip closes that for
a head. For a tip it does not: a tip already holds every landing's edits, so a
landing that rewrote the file would define its own tip's measurement. That is
the BASE correction: the file at the base of the first unproven landing is the
last version every landing above it was judged against.

The obvious bisect walks git's history between the green and the red tip. It
walks into a merge's second parent — a change's own commits, which never were
the target's tip and which no gate judged — and it costs a git read on the
home per step. The landing rows are the target's own history, one per commit
the target actually held, in the order it held them.

## What this does not decide

Who is woken when a landing is broken, and what they are offered (the code
wakes, M5's `target_broken`); whether a broken landing is ever reverted with
nobody asking (nothing reverts one: `code_revert_change` reverts a change when
somebody asks); what the gate requires of a change before
it merges (ADR-0066); and how a queue builds and lands its candidates, which
prove their checks before they land and so leave their tips green as they land.
