# ADR-0040 — A repository grant is derived from the chart

- **Status:** accepted
- **Authority:** `internal/code`
- **Enforced-by:** `internal/code.TestGrantsFollowTheChart`, `internal/code.TestAPersonsGrantAsksNoRoster`, `internal/code.TestRefRuleTable`, `internal/code.TestASeatsNamespaceIsUnderTheSeatsRoot`, `internal/gitref.TestOnlyTheRootAndASeatsNamespaceAreReservedBranches`, `internal/config.TestRoleCodeAccessIsTheOneVocabulary`, `internal/code/host.TestAnUnboundTokenCannotPush`
- **Measured:** nothing a figure decides: the grant is a pure function of the chart and the repository row
- **Cost-when-tried:** not tried here. The alternative — access rows per repository — was rejected on paper: a list of who may push is a second chart that drifts from the first the day a seat moves units
- **Tag-status:** unreleased

## The decision

What a principal may do on a repository is DERIVED, never stored: a person bound to a human seat holds maintain
everywhere and a token bound to nobody read (ADR-0037, unchanged); a seat holds the minimum of its tier's cap
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
function (`code.GrantOf`); M2's protection rules are judged after it and are never skipped for a grant.

## Why the obvious alternative is wrong

Access lists per repository duplicate the chart and drift from it; GitHub's tier as the only vocabulary made the
native host's one per-seat setting a GitHub field. `visibility` narrows SEATS only: a person and a Tier A token
read every repository, because a CI token that could not read a unit repository breaks its builds and a Tier A
token is the operator's own credential.

## What this does not decide

The decide's enforcement of the push rows for seats (M3 P5), a box's own grant on top of the seat's (M3 P6),
read-side directory access lists (not planned: sensitive code goes in its own repository with
`visibility: unit`), the review and merge levels' use (M4, M5).
