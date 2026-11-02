# ADR-0034: An artifact is shared, and a share is a relation

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestASharedRowAdmitsTheRoleItNamesAndEveryRoleAbove`, `internal/authz.TestACallerWithNoRelationIsToldTheArtifactIsNotFound`, `internal/authz.TestTheLeadArmOpensOnlyAnAgentOwnersArtifacts`, `internal/api/stream.TestTheAudienceHoldsExactlyTheAppsThePrincipalCan`
- **Tag-status:** unreleased

## The decision

Artifacts are the company's third app, beside the tracker and the knowledge
base: **`artifacts`** is a member of `iam.Apps`, admin includes it through
`iam.Expand`, a seat holds it, and a refusing node keeps it. This is the
extension [ADR-0033](0033-access-is-admin-and-one-permission-per-app.md) names
for itself (a third app is a new member of `iam.Apps` and a new set of rows),
and nothing about the shape of the vocabulary changes.

**The app gates authoring, and only authoring.** Creating an artifact,
publishing a version, changing a datastore schema, trashing and restoring ask
the app beside the relation. Reading an artifact, its versions and its history,
being present in it and writing its data within the collection rules ask the
relation alone. Somebody handed no app still opens, and fills in, every artifact
shared with them.

**A share is a relation, never a permission.** Each artifact carries an
owner and a list of shares, each naming an audience and one role on a closed
ladder: `viewer < commenter < contributor < editor < owner`, each role
including every role below it. An audience is a seat (whoever holds it, so the
access follows the seat as an inbox does), a unit (every seat in it or below
it, decided live against the running org) or the whole company. There is no
login, address or outside party an audience can name. `owner` is never shared;
it moves only by transfer. A share never grants above the sharer's own role,
and every narrowing (an unshare, a lower role, a trash, a transfer) moves the
artifact's access epoch, which every view ticket carries.

One class decides every artifact row, `ClassShared`, with the least role the
row admits (`minRole`) as a field on the row. A `ClassShared` row with no
`minRole` is refused by a walk, because an unset field must not read as the
open end. The class asks, in order: the admin path; whether the row has been
read at all (an unread row is `ErrUnresolved`, decided again on the row);
whether the caller is the owner, who holds every role; and otherwise the best
role among the caller's seat shares, the company share, the unit shares the
chart says the caller sits in (three-valued: a chart that cannot tell is not a
no) and, **only when the owner is an agent**, the owner's role for that agent's
lead. A person's lead reads nothing of that person's private artifacts.

**A caller with no relation is told the artifact does not exist**, with the
same answer and the same timing path as an id nothing holds: `404
not_found`. A caller with a relation below the row's role is told `403` naming
the role that would change the answer. Admin is never told 404 for an artifact
that exists. An administrator a share already admits is decided as that
sharer, at the share's role; an admission only the admin path made stands at
owner and is elevated, and every content read it makes writes a
`read_elevated` record before anything is shown. The artifact's owner and
editors see it in its history, and every administrator sees it on the company
trail of artifact history (`artifact_trail`, under `audit.read`), so an
override is found even when its owner never opens the history.

Four rules bound what an agent seat does with the relations it holds, each
decided in the artifacts writer or the tool surface rather than in the class:
an agent never widens a share to the whole company; trash, restore, transfer,
pin and a forced publish are human-only rows; an agent never manages an
artifact it does not own (publishing over it, updating it, resharing it)
unless the company turns `artifacts.agents_manage_others` on; and every
artifact tool passes the requester ceiling, so an agent reads for somebody only
what that somebody could read, and writes for them only what they could write
except on an artifact the agent owns.

The rows themselves, verb by verb, are `internal/authz`'s; this record does
not restate them. The socket's audience is a SET of apps read from `iam.Apps`
rather than one boolean per app, so a third app needed no second edit in a
switch nothing compared.

## Why the obvious alternatives are wrong

**Riding the `doclet` app** reads as economy and is two decisions wearing one
name. The Doclet app reads every page in the company; an artifact is a
per-object list of who may see it. One permission meaning both would let
"uses Doclet" silently mean "publishes script that runs in every colleague's
browser", and an engineer who uses only Worklet could not build a review
table for their own work. The tool-skill pages taught the same
lesson: a write's reach decides its gate.

**No app at all, relations only**, is the shape ADR-0033 already rejected for
everything else. A person handed nothing, and a service account holding no
permission (the documented way to read the overview), could publish runnable
pages to colleagues. Whether somebody may do that is something a founder
decides at an invitation, and ADR-0033 is explicit that what keeps a capability
from a person is that it belongs to a permission.

**A permission per artifact role** (an artifact read grant, an artifact edit
grant) is the eleven-grant vocabulary ADR-0033 retired, reintroduced for one
app. Who may open one sheet is a fact about that sheet, decided by its owner,
not a fact about a person decided at their invitation.

**Answering `403` to a caller with no relation** turns every private
artifact's id into an existence oracle: a caller could learn which ids exist,
and from the gallery's links, which titles. `Object.Unresolved` was introduced
to close exactly that oracle for logins, and the not-found rule is the same
closure for shares.

**Letting a person's lead read their drafts** extends a relation built for a
team's work queue to somebody's unfinished writing, which nobody asked for. An
agent's lead is different: a lead already steers, pauses and reads the trail
of their agents, and an agent that is paused or removed must not orphan what
it made.

## What this does not decide

It does not decide **how an artifact is served**: the origin that holds no
credential is [ADR-0035](0035-an-artifact-is-served-from-an-origin-that-holds-no-credential.md).
It does not decide **which rows of a datastore a role may write**: the
collection and field rules are evaluated where a write is applied
([ADR-0037](0037-a-datastore-write-is-decided-where-it-is-applied.md)), from the
role this table decided, stamped into the record. It does not decide **public
links**: the audience kinds are a closed set, a later build adds a link kind
with its own record, and this build refuses one as it refuses any unknown
kind. And it does not decide **who may purge**: a purge stays admin's, with a
recent proof, as for every other purge in the engine.
