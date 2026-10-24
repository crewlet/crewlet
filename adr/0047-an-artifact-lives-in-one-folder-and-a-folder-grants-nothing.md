# ADR-0047: An artifact lives in one folder, and a folder grants nothing

- **Status:** accepted
- **Authority:** `internal/artifactsdomain`
- **Enforced-by:** `internal/artifactsdomain.TestAFolderGrantsNothingToItsContents`, `internal/artifactsdomain.TestFilingNeverMovesTheAccessEpoch`, `internal/artifactsdomain.TestAFolderCountHoldsOnlyWhatTheReaderMaySee`, `internal/artifactsdomain.TestABreadcrumbNeverNamesAHiddenAncestor`, `internal/artifactsdomain.TestTheTotalEqualsTheRowsTheViewerMaySee`, `internal/artifactsdomain.TestAMoveRowNamesNoFolder`, `internal/artifactsdomain.TestDeletingAFolderReparentsWhatItHolds`, `internal/artifactsdomain.TestAFolderCycleIsRefusedWhereItIsApplied`, `internal/artifactsdomain.TestAnAgentNeverSharesAFolderWithTheCompany`, `internal/api/artifactsapi.TestAnInvisibleFolderIsNotFoundLikeAnUnknownOne`, `internal/api/artifactsapi.TestFilingAsksTheFolderAndTheArtifact`, `internal/api/artifactsapi.TestTheAdminPathListsNoPrivateFolder`
- **Tag-status:** unreleased

## The decision

The company's artifacts are filed in **one folder tree**, and each artifact
sits in **at most one folder** (`artifacts.folder_id`, empty for the top
level). **A folder grants nothing on what it holds**: who may open an
artifact is still its owner and its own shares
([ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md)), and
filing it, moving it, or removing its folder never widens or narrows that by
one reader. No decision about an artifact ever reads its folder.

**A folder's own visibility is its top-level folder's**: that folder's owner
and its shares on the artifact ladder, decided as a relation by the same class
an artifact's are (`authz.KindArtifactFolder`), and set by a preset when a
top-level folder is made: the whole company as contributor, or its owner
alone. A subfolder holds no share of its own. So a tree is seen whole or not at
all: every ancestor of a folder a reader sees is one they see too, a breadcrumb
never stops at a folder they may not know of, and siblings, which share one
visibility, can be held to unique names (by their case fold) without a refused
name telling anybody about a folder they cannot see. At the top level the
company's folders are unique among themselves and each owner's among their own,
for the same reason. A folder a reader cannot see is answered exactly as a
folder that does not exist, and an artifact filed in one is at that reader's
top level, never hidden by it.

**Filing asks two decisions**: editor on the artifact and contributor on the
destination folder, each through the requester ceiling for a tool, so an agent
files its own work into a team's folder and nobody plants a page in a folder
they could not file into. Managing a folder (renaming, moving, removing it,
taking an artifact out) is its own owner's, the owner's lead's or an
administrator's, and a person's. An agent creates folders only inside one,
because a company-wide top-level folder is a company-wide audience, which an
agent never grants.

**The tree is one subject on the artifacts log**, because every folder write
reads the whole tree (sibling names, the ancestor chain a move must not close
into a cycle, the depth beneath it, the live count), and one subject makes the
writer's snapshot the one the broker arbitrates. An artifact's move rides the
artifact's own subject, ordered against its publishes, and names the folders
it reads in its scope; a create or a move into a folder removed since it was
decided is refused where it applies. **Removing a folder moves everything in
it up one level**, child folders and artifacts, live and trashed, in one
record: never refused for what it holds, never a cascade into the trash.

Every count the estate answers beside a page (a listing's total, a folder's
artifacts, the artifacts in no folder) comes from the listing's own visibility
predicate in the same transaction, and every history row names folders by id,
never by name.

## Why the obvious alternatives are wrong

**Inheriting folders**, where a folder's shares flow to what it holds, are the
familiar shape and the one with the familiar incident: moving an artifact into
a folder silently shares it with everybody who sees the folder. Here it would
also let an agent widen an artifact to the company by moving it, past the rule
that an agent never shares with the company, let a folder's editors become
editors of an artifact its owner never shared with them, and turn every
narrowing of a folder into an access-epoch move of every artifact beneath it in
one record.

**A company-wide namespace with no visibility**, every folder seen by
everybody, broadcasts every folder name to every person and every agent's
listing, a name an agent may have made up from a confidential brief ("Layoffs
Q1"), and an outsider's turn reads it through the agent.

**Per-folder audiences on every folder** would let a reader see a folder whose
parent they cannot, so a breadcrumb would have to stop short, sibling names
could not be unique without disclosing hidden siblings, and every listing would
walk ancestors per row.

**Refusing to remove a folder that is not empty** tells its remover that
something they cannot see is inside it, and **trashing its contents** destroys
other people's work, which only their owners may trash.

## What this does not decide

It does not decide **how a folder looks or how a reader moves things**: that is
the dashboard's. It does not offer **folder share changes after creation**,
unit or seat audiences on a folder, or default folders per seat: a later build
adds each as ops on the same tree subject. It does not decide **path addressing
for an S3 client**: the listing speaks `ListObjectsV2`'s vocabulary over this
tree, and a gateway would be its own record.
