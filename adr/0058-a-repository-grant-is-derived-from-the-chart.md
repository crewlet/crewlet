# ADR-0058: A repository grant is derived from the chart

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestGrantsFollowTheChart`, `internal/code.TestAPersonsGrantAsksNoRoster`, `internal/code.TestRefRuleTable`, `internal/code.TestASeatsNamespaceIsUnderTheSeatsRoot`, `internal/gitref.TestOnlyTheRootAndASeatsNamespaceAreReservedBranches`, `internal/config.TestRoleCodeAccessIsTheOneVocabulary`, `internal/code/host.TestAnUnboundTokenCannotPush`, `internal/code/query.TestAUnitRepositoryRefusesASeatOutsideIt`, `internal/estate.TestANodeBehindTheConfigurationDecidesNoGrant`, `internal/estate.TestEveryOperationThatJudgesAGrantIsDeclared`, `internal/code/query.TestAGitNodeBehindItsConfigurationServesNoSeat`, `internal/code.TestAReadForAnotherSeatIsHeldToBothGrants`, `internal/code.TestAReadMadeForAColleagueIsHeldToTheColleaguesGrant`, `internal/code/query.TestAReadMadeForASeatIsHeldToThatSeatsGrant`, `internal/agent/builtin.TestACodeReadIsDecidedAgainForWhoeverWokeTheTurn`, `internal/agent/builtin.TestACodeStatusInATurnSomebodyWokeIsHeldToTheirGrantToo`
- **Measured:** nothing a figure decides: the grant is a pure function of the chart and the repository row
- **Cost-when-tried:** not tried here. The alternative — access rows per repository — was rejected on paper: a list of who may push is a second chart that drifts from the first the day a seat moves units
- **Tag-status:** unreleased

## The decision

What a principal may do on a repository is DERIVED, never stored: a person, who always holds a human seat
(ADR-0032), holds maintain everywhere and a credential bound to no seat reads (ADR-0055, unchanged); a seat holds the minimum of its tier's cap
(`role.code.access`: read_only → read, review → review, full_access → maintain; unset → read) and its relation to
the repository on the chart — the owning unit's effective lead maintains, the owning unit's subtree writes, the
lead chain above it reads, and every other seat reads only a `visibility: company` repository. A seat owns
`refs/heads/seats/<handle>/` on every repository it may read when its tier is full_access — under a root of its
own, never at `refs/heads/<handle>/`, because a handle is a role name's slug and people name branches under
those (`docs/`, `release/`); `refs/heads/seats` and `refs/heads/seats/<handle>` themselves are nobody's —
refused `reserved_ref` to every principal and as a repository's `default_branch` — because no ref is created
beside a directory of its own name (`ref_conflict`: no clone can hold a file and a directory of one name), so a
branch named `seats` would block every seat's namespace and one named `seats/<x>` the namespace of the seat `x`.
Both shapes are `gitref`'s (`SeatsRoot`, `ReservedSeatBranch`), the one grammar the decide and the configuration
read (ADR-0008). The grant is judged by the node that answers or decides, from ITS epoch's chart, through one
function (`code.GrantOf`); M2's protection rules are judged after it and are never skipped for a grant. A node
CONFIRMED behind the company's configuration — shed or stuck, never lag alone — judges no seat's grant at all,
even as the last node left to ask, since the revision it failed to apply may be the one that revoked it: a data
node answers such a read `behind_config` (the estate's `judgesGrant` declaration, on every operation whose asker
is a seat) and a git node answers it unready, so a current node answers instead, or nobody does, while a
person's read consults no chart and is answered there as before. The window in which a stale chart can still
judge is the control plane's confirmation of the lag: about a minute after an activation at worst.

A seat's read made inside a turn somebody woke is decided AGAIN for whoever woke it — the requester ceiling
(`authz.DecideWithin`) on a repository's grant. The tool's own row is asked for the requester first, so a turn a
stranger woke, or a person who may not act in the company now, reads nothing, and a person, who maintains
everywhere, bounds nothing; a colleague SEAT rides on every read the turn makes (`code.ReadQuery.CeilingSeat`
and its siblings on the resolve and the read by object id) and is judged by the node that answers, from the same
chart and row, through one function (`code.JudgeRead`): served only what both seats may read, and refused
`ErrAboveCeiling`, never `ErrNotPermitted`, where the colleague may not. Never judged where the turn runs: a
ceiling judged once a turn would keep reading for a colleague an activation had shut out. A tool that reads
repositories beside a view of its own — `code_status`, the health grant's — holds those reads to the same
ceiling, its requester decided on the code host's read verb (`code.read`) rather than on its own row, which
grants a view of the deployment that the ceiling does not bound.

## Why the obvious alternative is wrong

Access lists per repository duplicate the chart and drift from it; GitHub's tier as the only vocabulary made the
native host's one per-seat setting a GitHub field. `visibility` narrows SEATS only: a person and a credential acting
as no seat read every repository, because a CI token that could not read a unit repository breaks its builds
and a Tier A token is the deployment's own credential.

## What this does not decide

The decide's enforcement of the push rows for seats (M3 P5), a box's own grant on top of the seat's (M3 P6),
read-side directory access lists (not planned: sensitive code goes in its own repository with
`visibility: unit`), the review and merge levels' use (M4, M5). Nor the requester ceiling of a seat's code
WRITES (M3 P5's write tools, each held at the relation its write needs): the ceiling above bounds what a seat
reads for somebody, and nothing it writes. Nor a ceiling carried past its turn: a colleague a seat asks answers
in a turn the asking seat woke, and the answer wakes the asking seat in a turn of its own work, so each turn is
decided for whoever woke it and never for whoever woke the asker — as the artifacts tools' ceiling is — and
nothing here bounds what a seat already knows, from its memory or a read made in a turn nobody woke.
