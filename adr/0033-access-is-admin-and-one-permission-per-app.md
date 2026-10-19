# ADR-0033: Access is admin and one permission per app

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/iam.TestEveryAppIsAGrantAndAdminIsNot`, `internal/iam.TestExpandAddsEveryAppExactlyWhenAdminIsHeld`, `internal/iam.TestARefusingNodeKeepsEveryApp`, `internal/authz.TestEveryAppIsNamedByARow`, `internal/authz.TestEveryAppRowAdmitsAHolderAndRefusesANonHolder`, `internal/authz.TestSeatOnlyHumanOnlyAndSeatedAreCheckedBeforeTheClass`, `internal/authz.TestEveryDottedVerbIsAskedByANonTestCallSite`, `internal/iamdomain.TestAnAdministrativeRecordIsNeverWrittenThroughAMachineToken`, `internal/agent/turnctx.TestASeatHoldsEveryAppAndNeverAdmin`
- **Cost-when-tried:** the vocabulary before this was ELEVEN grants — a company read, an audit read, a read and a write for configuration, a read and a write for secrets, a write for work, a write for knowledge, a deployment operator, a manager of people and a sandbox run — and the people choosing them could not tell most of them apart, so neither could the code. The node's own writer held the operator grant while the identity domain asked the configuration write of the same record, so every invitation redemption was refused on a real deployment while the surface's suite, built on a stub writer, stayed green. The company read once gated nothing at all, and the walk that caught it existed only because nobody reading the table could see it. And a person invited with nothing ticked could open nothing — not the org chart, not the socket, not their own seat — and landed on a "no access yet" panel with no step they could take, because reading the company was itself a grant.
- **Tag-status:** unreleased

## The decision

There are five permissions. **`admin`** runs the company and the engine: its
configuration, its credentials, its people, its deployment and its
company-wide trail. **`worklet`**, **`doclet`**, **`artifacts`** and
**`chatlet`** each use one APP: Worklet, Doclet and Chatlet,
reading and writing, and the company's artifacts, authoring them (what opens
an artifact is its shares,
[ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md)).
`iam.Apps` is the one place an app is declared, and admin includes every app
through `iam.Expand`, which every comparison of permissions goes through.
`chatlet` is the fourth app and was appended LAST, so every list drawn in the
vocabulary's order kept its first members where they were.

**Everything else is everybody's.** Every principal the request guard resolves
sees the read-only overview — the org chart, the agents' status, spend,
schedules, who to ask — and manages their own account, with no permission at
all. What a lead may do to their own team, an author to their own work and a
person to their own record are RELATIONS, not permissions, and on an app's
verb each is asked beside the app.

Three facts about a verb are fields on its row rather than permissions anybody
holds: `humanOnly` keeps an agent off a person's gesture, `seatOnly` keeps a
person off an agent's — asking a colleague and starting a coding run, which
the seat's role offers or does not — and `seated` keeps a credential acting as
itself off a verb made as a seat: a question answered on the asker's seat's
model, and every chat verb, because a room renders a speaker by seat and its
membership is seats. An agent seat holds every app and never admin. A node
either serves admin or clamps it away (`api.auth.admin`), and a node that
clamps it keeps every app.

The table that says which verb needs what is `internal/authz`'s, row by row;
this record does not restate it.

## Why the obvious alternative is wrong

The obvious alternative is a capability per area — a read grant and a write
grant per surface, and a grant per kind of operator. It reads as least
privilege and it is not: each grant is a decision somebody has to make at
every invitation, about a distinction they were never shown the consequence
of, and each is a row the code has to keep pairing with another row in
another package. The incidents above are what that pairing cost, and none of
them was a person being given too much — each was the engine refusing what
everybody involved intended, or granting nothing while looking like it did.

A finer vocabulary also cannot say what a company actually decides about a
colleague. "Uses Worklet", "uses Doclet", "makes artifacts"
and "runs the place" — and, where the company runs its own chat, "talks in
it" — are the questions a founder answers when somebody joins; a reader of the
trail, a manager of people and an operator of the deployment were three grants
nobody could tell apart, and they are one.

**Gating the overview behind a read permission** is the other half of the same
alternative. A company is a shared place; somebody handed no app still sees
what it is doing, and what keeps a transcript or a credential from them is that
those belong to admin, not that the org chart is withheld.

**A token that may never carry admin**, which the old vocabulary enforced by
withholding the person-present grants from every mint, moved to where the
guarantee actually lives: the authority table's presence rows refuse a token
every gesture a person must be present for, and the identity domain refuses an
administrative record written through a machine token at the record itself, so
a CLI, a duty or a test is held to it exactly as a route is.

## What this does not decide

It does not decide **what a team lead may change** in the org chart, which is
unchanged in reach, nor whether lead authority travels through a machine token,
which it still does. It does not decide **step-up**: the recency rows are
unchanged, and a purge asks for a recent proof. It does not decide **who may write a managed company document**
([ADR-0030](0030-a-managed-company-document-names-its-writers.md)) or **that a
person holds a human seat**
([ADR-0032](0032-every-person-holds-a-human-seat.md)); both compose with it.
And it does not decide **which apps exist**: a new app is a new member of
`iam.Apps` and a new set of rows, and the walks named above fail until both
are there — which is how `artifacts`
([ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md)) and
`chatlet` arrived.
