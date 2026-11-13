# ADR-0137: Google Cloud and Scaleway are read as the provisioner, and a project is an account

- **Status:** accepted
- **Authority:** `internal/googlecloud`
- **Enforced-by:** `internal/googlecloud.TestTheAtlasReadsProjectsAndWhatTheirServicesSay`, `internal/googlecloud.TestTheGoogleCloudReadAndTheSettingsNameOneIdentityProject`, `internal/googlecloud.TestAProjectsServicesGoogleRefusesAreAFindingAndWithdrawNothing`, `internal/googlecloud.TestAGoogleCloudProjectTwoReadsMissIsRetired`, `internal/googlecloud.TestNoCredentialShapeReachesTheAtlasFromGoogleCloud`, `internal/googlecloud.TestAListingThatFailsOrIsRateLimitedFailsTheGoogleCloudRead`, `internal/googlecloud.TestAReadWaitsOutGooglesRateLimitAndAWriteDoesNot`, `internal/googlecloud.TestTheGoogleCloudProjectsAreWalkedToTheirBounds`, `internal/scaleway.TestTheAtlasReadsTheOrganizationsProjects`, `internal/scaleway.TestTheScalewayReadAndTheSettingsNameOneOrganization`, `internal/scaleway.TestAListingScalewayRefusesIsAFindingAndRetiresNothing`, `internal/scaleway.TestAScalewayProjectTwoReadsMissIsRetired`, `internal/scaleway.TestNoCredentialShapeReachesTheAtlasFromScaleway`, `internal/scaleway.TestAListingThatFailsFailsTheScalewayRead`
- **Cost-when-tried:** not tried. Cloudflare, Datadog, GitHub and GitLab each hold things the vocabulary already had a kind for. A cloud is the first vendor whose first layer is a container of everything else, and the first whose existing credential reads that layer and nothing inside it.
- **Tag-status:** unreleased

## The decision

**Each cloud is read under the credential that already provisions it.** Google Cloud is read as the provisioner service account, through the federation the reconcile loop uses; Scaleway under the provisioner key. Neither gains a role, a permission set or a setting for the atlas. Every call the read adds is a `GET`.

**A project is a vendor account.** A Google Cloud project and a Scaleway project are each an entry of kind `vendor_account`, keyed the vendor and the project's own id, which is the key the settings already give Google Cloud's identity project and Scaleway's Organization. The read says the project's vendor, its id and the name the vendor holds for it. It names the entry itself only where the settings do not: the identity project and the Organization keep the name the settings give them, since two names would be a disagreement for a person to settle. Scaleway's default project carries the Organization's id, so it is the Organization's entry and is described no further.

**An enabled API says what a project is used for, in a closed word.** Google's Service Usage lists the APIs enabled on a project. The read maps each to one of the atlas's capabilities (`compute`, `database`, `dns`, `model_api`, `ci`, `code_hosting`, `identity`) through a fixed table, and says nothing for an API the table does not hold. The API's own name is never a value. The table leaves out every API a new project has enabled before anybody uses it (Cloud Storage, BigQuery, Datastore, logging and monitoring), because that enablement says nothing about use.

**The listing of projects proves the credential, and a project's services are a part of their own.** A refusal of the listing is a rejected credential and fails the read. Listing a project's services takes Service Usage Viewer there, which the provisioner holds only where the Prepare a project block ran: a `403` or a `404` leaves that project's services unread, the standing counts the projects and names the block, and ADR-0130 carries what an earlier read said those projects are used for. At Scaleway the one listing is the one part: a `403` is a key whose policy lacks `ProjectReadOnly` and is a finding, and a `401` is a key Scaleway does not know and fails the read.

**Only an active project is read.** A project Google is deleting stays listed for thirty days in a state other than `ACTIVE`. It is left out, so it is retired by the same two complete reads a day apart as one that is gone.

**The Google Cloud client waits out a read's rate limit, and never a write's.** Service Usage has a per-minute read quota that one read of a large company can meet. A `GET` answered `429` is sent again after the wait Google names, or five seconds where it names none, by the rule every vendor client shares (`httpx.VendorRateLimit`). A write answered `429` is handed back as it came: a create refused for a spent quota answers the same status, and no wait clears a quota. A limit a call cannot wait out fails the read whole.

**A company past a thousand projects is not read.** Each project costs a request, and half a listing would retire the other half. The standing says so and nothing is retired.

## Why the obvious alternative is wrong

**A kind for a project** adds a second kind for what the vocabulary already holds: an account is the company's place at a vendor, and a project is exactly that. The settings already name the identity project and the Organization as accounts, so a project kind would also split each of those into two entries.

**Saying each enabled API by its name** makes a value out of text a vendor chooses, in a list that grows with every product, and answers "which cloud runs our databases" with fifty service names per project. A capability is the question the atlas already answers for every other vendor.

**Counting every enabled API** shows object storage, a database and monitoring on every project Google ever created, because Google enables those on creation.

**Reading a refused project's services as none** withdraws what the project is used for the hour somebody removes a role, and restores it the hour they put it back.

**Naming every project from the vendor's display name** puts each company's connected account into Review for a name nobody chose, since the settings already name it.

**Relating a project to the organization or folder above it** needs a relation from an account to an account, which the vocabulary does not have, and for a folder a role the provisioner does not hold. A project's vendor already places it.

**Waiting out every `429`** makes a project at its quota of service accounts cost three waits on every create before the engine reports what Google said at once.

**A read identity made for the atlas** is what reading inside a project needs (instances, databases, clusters, buckets), and is a setup step a person takes. Reading the projects themselves needs nothing the provisioner lacks.

## What this does not decide

What is read inside a project, which takes an identity made for the atlas. Whether an account may belong to another account. Whether Scaleway's products are read, which the provisioner key cannot be asked. How a refused part is shown, which the dashboard decides.
