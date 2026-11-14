# ADR-0144: Scaleway's products are read under an application made for the atlas, which the provisioner mints when a company asks

- **Status:** accepted
- **Authority:** `internal/scaleway`
- **Enforced-by:** `internal/scaleway.TestTheAtlasApplicationHoldsNamedReadOnlySetsAlone`, `internal/scaleway.TestThePassMakesTheAtlasApplicationOnceAndThenWritesNothing`, `internal/scaleway.TestACheckMakesNoAtlasApplication`, `internal/scaleway.TestSwitchedOffTheAtlasApplicationIsDeletedAndForgotten`, `internal/scaleway.TestAnAtlasApplicationDeletedAtScalewayIsNotMadeAgain`, `internal/scaleway.TestTheAtlasKeyIsRotatedAndAStrangersIsReported`, `internal/scaleway.TestTheAtlasPolicyIsHeldToItsReadOnlySets`, `internal/scaleway.TestASetScalewayDoesNotListIsLeftOutOfTheAtlasPolicy`, `internal/scaleway.TestAFailedCreateLeavesNoAtlasApplication`, `internal/scaleway.TestTheCompanyPassReportsAStrayAtlasApplication`, `internal/scaleway.TestTheAtlasReadsWhatTheProjectsHold`, `internal/scaleway.TestNothingScalewaySendsBesideTheMappedFieldsReachesTheAtlas`, `internal/scaleway.TestNoCredentialShapeReachesTheAtlasFromAScalewayProduct`, `internal/scaleway.TestWithTheSwitchOffNoProductIsRead`, `internal/scaleway.TestAProductScalewayRefusesIsAFindingAndRetiresNothing`, `internal/scaleway.TestAnAtlasKeyThatDoesNotWorkLeavesTheProductsUnread`, `internal/scaleway.TestAProductListingIsReadWholeOrNotAtAll`, `internal/engine.TestAnAtlasApplicationThatCannotBeConvergedIsAFindingNeverAFailedPass`
- **Cost-when-tried:** not tried. ADR-0137 read Scaleway's projects under the provisioner key and stopped there, because that key holds no product's permission set and widening it would put product reads on the one key that administers IAM.
- **Tag-status:** unreleased

## The decision

**What a project holds is read under an identity that is the atlas's alone.** Where a company switches `integrations.scaleway.atlas` on, the engine makes one IAM application named for the atlas and the company, one policy on it, and one API key. Nothing else reads Scaleway's products for the atlas: not the provisioner key, and never an agent's. At Scaleway the reads show as that application's, and deleting it revokes the atlas and nothing else.

**The provisioner mints it, so the setup step is a switch.** The provisioner key already makes an application, a policy and a key for every agent, and Scaleway checks no grant against its granter. The same three calls make the atlas's, so a person creates nothing at Scaleway. The company pass converges it beside its other work: it creates the application where none is recorded, holds the policy to its rule, and replaces the key before it lapses, on the lifetime and the renewal window an agent's key has. A converged pass writes nothing.

**The grant is nine named read-only permission sets on every project.** One per product the atlas draws: Instances, Kubernetes, Managed Databases, Redis, Load Balancers, Object Storage buckets (their configuration and names, never objects), Domains and DNS, Serverless Containers and Serverless Functions. No set spans products, none writes, and none is IAM, billing, secrets or keys. The rule is scoped to the Organization, which Scaleway reads as every current and future project, so a new project needs no write. A set the Organization's catalogue does not list is left out of the rule and said.

**The application is the engine's by its ID, and its key is sealed under fixed names.** The application's ID, its key's access key and the key's secret are three sealed values under names of their own, which no seat's sealed name can collide with. The engine binds them to no seat's tools or box, and the company document needs no field to name them. As with an agent's application, a name proves nothing: only the recorded ID is acted on, and an application under the atlas's name that nothing records is reported and left alone. The create seals the ID only after the policy exists, and takes the application back if either step fails.

**Off deletes it; a delete at Scaleway is a person's revoke.** Switching the setting off, or disconnecting Scaleway, deletes the key, the policy and the application and forgets the sealed values. An application somebody deleted at Scaleway is not made again until the setting is switched off and on. Its policy and its key are the engine's to keep, so those are written again where they go missing.

**Nothing about it can degrade the integration.** Every finding about the atlas application is an advisory on a ready integration, and a convergence that fails is such a finding, never a failed pass. The atlas is optional reading; every agent's access does not wait on it.

**The read lists, and decodes only what the atlas keeps.** Each product is one part of the source's read (ADR-0130): a listing per locality with `GET` alone, everything listed before anything is claimed. A `403` leaves that part unread and carries what it recorded; a `401`, which is a key the pass must replace, leaves every product unread and the projects read; any other failure fails the read. The decoders hold the fields the atlas keeps (an id, a name, a project, a locality, an address, an engine and its version, a record's type, name and target) and no other. A server's user data, a cluster's kubeconfig, a database's users and a bucket's objects are never asked for.

**A server, a cluster and a load balancer are hosts; a container and a function are workloads.** The vocabulary is closed, and each product is placed in the kind whose relations it needs: what a name resolves to and what something stands on is a host, a managed database or cache is a datastore that runs on its project's account, and a serverless unit is a workload that runs on its project's account. Each entry is keyed the vendor, the product and the vendor's own id, so a part is told from its neighbours and a rename moves nothing. A project is said to use what was found in it, in the atlas's capability words.

**A name pointing at an address the same read holds resolves to that entry.** The fold reads one source saying an address twice as a shared address, and joins nothing on it. So an A or AAAA record whose address is a server's or a load balancer's public address in the same read is related to that entry directly, and only an address the read does not hold becomes a server known by its address alone. The record mapping is one function in `internal/atlas/collect`, which Cloudflare's read uses too.

## Why the obvious alternative is wrong

**Adding the read-only sets to the provisioner** puts every product read on the key that can grant anything to any application, makes "holds more than it needs" true by design, and shows the atlas's reads at Scaleway as the provisioner's.

**Reading under an agent's key** reads what one seat was given on the projects it names, attributes the atlas's reads to that agent, and stops the day somebody turns the agent off.

**A console step** (create an application, attach a policy, paste a key) asks a person to do by hand what the provisioner key already does for every agent, and adds a secret field to the company document that the engine would have to rotate through a person.

**`AllProductsReadOnly`** is one name instead of nine, and grants reads of Secret Manager, IAM, billing and the contents of every bucket.

**Making the application again when it is gone at Scaleway** undoes a revoke within one pass, which is the opposite of separately revocable.

**Failing the pass when the atlas application cannot be converged** reports every agent's access broken over an optional read.

**Claiming a server known by its address for every address record** leaves each name pointing at a second entry beside the load balancer or the server that holds the address, which no later read would join.

**Decoding the endpoints of a database, or the environment of a container,** records connection details and whatever a person put in a variable. Neither is something the atlas draws.

## What this does not decide

Whether the vocabulary gains kinds for a cluster and a load balancer, which would re-key those entries. What is read inside a cluster, a database or a bucket: nothing is. Whether other Scaleway products (queues, registries, block storage, MongoDB) are read. That the read-only sets reach more than the listings (a server's user data, the plain environment of a container or a function, and a cluster's objects other than Secrets through its own API): the engine calls none of those, the integration's page says so, and a company that does not want one sealed key to hold that reach leaves the switch off.
