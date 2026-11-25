# ADR-0150: An agent's access at an integration is its level plus grants a person makes

- **Status:** accepted
- **Authority:** `internal/seataccess`
- **Enforced-by:**
  - `internal/seataccess.TestALapsingGrantLeavesThePlanBeforeItsExpiry`
  - `internal/seataccess.TestAWriteGrantNeedsTheStepUpTheAdministeringLevelDoes`
  - `internal/seataccess.TestAGrantIsAddedOrRemovedNeverEdited`
  - `internal/seataccess.TestAGrantIsCheckedAgainstTheLiveCatalogue`
  - `internal/seataccess.TestARequestsOptionsCarryItsID`
  - `internal/cloudflare.TestAGrantRidesTheTokenBesideItsLevel`
  - `internal/cloudflare.TestAGrantLeavesTheTokenBeforeItsExpiry`
  - `internal/cloudflare.TestAGrantTheProvisionerLacksIsReportedNamingTheGroup`
  - `internal/cloudflare.TestGrantsThatMakeUpAHigherLevelAreReadAsGranted`
  - `internal/cloudflare.TestTheGrantCatalogueIsWhatTheProvisionerCanMint`
  - `internal/api/setupapi.TestAGrantIsStampedByTheEngineNeverByTheBody`
  - `internal/api/setupapi.TestAGrantTheCatalogueCannotGiveIsRefused`
  - `internal/api/setupapi.TestAWriteGrantIsAskedOfAPerson`
  - `internal/api/setupapi.TestARequestsGrantIsBuiltFromTheRequest`
  - `internal/agent/builtin.TestAPermissionRequestIsTheEnginesDecision`
  - `internal/agent/builtin.TestAPermissionRequestNobodyCouldGrantIsRefused`
  - `internal/agent/builtin.TestAPermissionIsAnsweredOnlyOnceItsAgentHoldsIt`
  - `internal/integration.TestASurfaceHoldingALapseIsDueAtIt`
  - `internal/capability.TestASeatIsToldTheGrantsItsCredentialHolds`
  - `internal/seataccess.TestEveryGrantLaneSaysHowAnAgentReadsItsGrants`
  - `internal/agent/builtin.TestRequestAccessNamesEveryGrantLanesRefusalAndPermission`
  - `internal/scaleway.TestAGrantRidesThePolicyBesideItsLevel`
  - `internal/scaleway.TestAGrantLeavesThePolicyBeforeItsExpiry`
  - `internal/aws.TestAGrantAttachesBesideTheLevelAndLapsesAlone`
  - `internal/aws.TestARefusedPolicyIsNeverAttached`
  - `internal/googlecloud.TestAGrantBindsBesideTheLevel`
  - `internal/googlecloud.TestAGrantOutsideTheGrantsListIsRefusedNamingIt`
- **Tag-status:** unreleased

## The decision

**A seat's access at an integration is its level plus its live grants.** A
level stays the safe default, a fixed set the engine names. A grant is one
permission from the vendor's live catalogue (a Cloudflare permission group),
on a scope (some of the company's zones, or the permission's whole reach),
for a number of hours or for good, with who granted it, when and why. It is
stored where the seat's level is, in the seat's block of the company document
(`roles[].integrations.cloudflare.grants`), and saved through the Agents tab's
draft, so its preview, plan, history and revert are the level's.

- **The engine writes who and when.** A draft adds a grant without its id,
  author or time, which the save stamps from the person saving; a grant is
  added or removed, never edited, so the record says what the engine saw.
- **Only what the catalogue offers and the provisioner holds is granted.**
  The vendor's session lists every permission a seat may be granted
  (`seataccess.Granter`), the vendor's never-granted permissions left out
  (for Cloudflare: token, member, billing and Access administration groups)
  and each the provisioner does not hold marked unavailable. A save refuses
  one it cannot grant; a stored one the provisioner later lacks is reported
  (`grant_short`, naming what to add) and left off the credential while the
  rest of it converges.
- **A write is a raise.** Adding a grant of a permission not shown to be
  read-only, or one the catalogue cannot vouch for, needs what an
  administration level needs: `setup.access.raise`, a person present, a
  fresh step-up.
- **A grant never outlives its expiry.** Every plan reads only live grants,
  a grant leaves the plan a minute before it lapses (`seataccess.LapseLead`),
  and the integration loop runs the kind's pass at the earliest lapse
  (`integration.Paced`) rather than on its settled interval. Once no
  credential holds a lapsed grant, a sweep drops it from the document in an
  engine-attributed revision.
- **An agent asks for one with `request_access`** (ADR-0118's tool, its
  integration form): the engine checks the permission against the live
  catalogue, stores the request on the coordination store and posts it as an
  engine-composed decision whose grant options carry the request's id. A
  person grants by saving the grant built from the stored request (its
  reason, its hours), and the engine accepts the answer only once the active
  revision holds it. Every grant asked, granted, declined, removed, lapsed or
  refused is a `seat_grant_changed` event.
- **The seat is told what its credential holds** (ADR-0104): "Your access"
  lists each grant the last sync read back, and a lane that takes grants says
  to ask with `request_access` when the vendor refuses a call, never to have a
  person edit the token at the vendor.

## Why the obvious alternatives are wrong

**One more level for every product an agent needs.** Every Cloudflare product
an agent was refused became an engine change, and each level then granted it
to every agent on that level. A grant gives one agent one permission.

**Keeping grants on the coordination store, outside the document.** The level
they sit beside is in the document, and so is every other part of a seat's
access the Agents tab saves; two homes for one seat's access would be two
previews, two histories and two places an operator-managed document does not
see.

**Granting on the answer in the background.** A person who chose the option
would have approved a change they never saw, and a grant that failed would
leave the agent told yes with nothing changed (ADR-0118's reasoning).

## What this does not decide

Which integrations take grants beyond these: Cloudflare, Scaleway (a
permission set in the seat's own policy), AWS (an AWS managed policy under the
seat boundary) and Google Cloud (a role a project's grants list names) do,
each by setting `LaneSpec.Grants` and its `GrantTerms`, implementing
`Granter`, reading its grants back in `Observe` and taking `OpGrants`; another
adds them the same way. DigitalOcean and Expo take none, since a person makes
each of their credentials by hand and no grant could lapse without one;
GitHub's access already is a set of permissions `request_access` edits, and
its tokens outlive an hour's expiry; SSH narrows no credential. An operator-managed company's document is
the operator's, which renders no grants, so its seats keep their levels.
