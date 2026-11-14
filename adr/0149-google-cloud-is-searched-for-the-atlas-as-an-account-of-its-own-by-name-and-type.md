# ADR-0149: Google Cloud is searched for the atlas as an account of its own, by name and type

- **Status:** accepted
- **Authority:** `internal/googlecloud`
- **Enforced-by:** `internal/googlecloud.TestTheAtlasReadsWhatEachProjectRunsAsItsOwnAccount`, `internal/googlecloud.TestTheSearchAsksOnlyForNamesAndTypes`, `internal/googlecloud.TestARefusedSearchIsAFindingAndWithdrawsNothing`, `internal/googlecloud.TestTheAssetAPIOffIsOneFindingNamingTheBlock`, `internal/googlecloud.TestNoSearchIsMadeUntilTheCompanyAsks`, `internal/googlecloud.TestEverySearchPageIsRead`, `internal/googlecloud.TestNoCredentialShapeReachesTheAtlasFromASearch`, `internal/googlecloud.TestTheBlockMakesTheAtlasAccountOnlyWhenAsked`, `internal/googlecloud.TestTheConnectFormOffersTheAtlasReader`
- **Cost-when-tried:** not tried. ADR-0137 reads each project as the provisioner and stops at its edge: reading inside a project takes an identity made for the atlas.
- **Tag-status:** unreleased

## The decision

**What a project runs is read as a service account of the atlas's own, where the company asks.** `integrations.google_cloud.provisioning.atlas_read`, off by default, adds to the identity Cloud Shell block the service account `crewlet-atlas` in the identity project, lets Crewlet's federated subject act as it, and enables the Cloud Asset API there. The engine impersonates it with the federated token itself, never through the provisioner, so Cloud Audit Logs name the atlas behind each search, and the provisioner gains nothing.

**It holds one permission, in a custom role, on the provisioner's projects.** The block creates `crewletAtlasReader` holding `cloudasset.assets.searchAllResources` on every active project where the provisioner holds a role, and grants it to the account. The atlas reads the same projects the provisioner reads (ADR-0137), so its reach follows the projects a person prepared for Crewlet. The Prepare a project block grants it on a project prepared later.

**Cloud Asset Viewer is not granted.** It also holds the permissions that list and export assets whole, and a whole asset is a function's environment or an instance's startup script. A search answers a resource's configuration (`versionedResources`) only when its read mask names it.

**Every search names its read mask and its types.** The mask is `name,assetType,displayName,location,state,additionalAttributes`; Google's default mask would add labels, tags and descriptions. The types are a closed list: Compute Engine instances, Cloud SQL, Memorystore for Redis, AlloyDB and Spanner instances, Cloud Run services and functions, Cloud Storage buckets and Pub/Sub topics. Of the additional attributes, only an instance's two address lists are decoded.

**What the read records.** An instance is a server with its name, addresses and Region; a database is a datastore, with the engine its type names (Cloud SQL's is in its configuration, which is not read); a Cloud Run service or function is a workload running on its project's account; a bucket is keyed by its global name as an S3 bucket is; a topic is a queue. Each belongs to, or runs on, its project's account, which ADR-0137 already names. A resource being deleted is left out.

**A search Google refuses is a part of its own.** A project whose search is refused is unread, with what an earlier read recorded of it carried (ADR-0130). A bucket's key names no project, so every bucket's claims are carried while any project is refused, and a bucket gone from a project that was read is retired once the refusal clears. An account Google will not mint a token for, or a Cloud Asset API that is off in the identity project, is one finding that names the identity block and carries every resource. Any other failure fails the read whole.

## Why the obvious alternative is wrong

**Granting Cloud Asset Viewer**, the role Google's documentation suggests for a search, lets the account list every resource's whole configuration, secrets included, with one call the engine never makes but anybody holding the token could.

**Reading each product's own listing** (instances, services, functions) needs a list permission per product, and every one of those answers the whole resource: an instance's metadata, a service's or a function's environment.

**Searching as the provisioner** widens the identity that grants agents their levels, and puts an hourly inventory of every project under its name.

**Searching the organization in one call** needs an organization-level grant that many companies cannot make (a project with no organization) and reaches projects nobody prepared for Crewlet.

**Granting the role on every project the person can see** reaches projects nobody chose for Crewlet. The provisioner's grants are the company's statement of which projects those are.

## What this does not decide

Kubernetes clusters, DNS zones and records, load balancers and addresses, which take types and their attributes this build does not decode. A Cloud SQL instance's engine. Whether the atlas reads an organization at once.
