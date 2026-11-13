# ADR-0146: DigitalOcean and Expo are read for the atlas under a token a person pastes for Atlas alone, proven read-only before every read

- **Status:** accepted
- **Authority:** `internal/digitalocean`
- **Enforced-by:** `internal/digitalocean.TestTheAtlasReadsTheTeamsResources`, `internal/digitalocean.TestTheDigitalOceanReadAndTheSettingsNameOneTeam`, `internal/digitalocean.TestATokenThatCanWriteReadsNothing`, `internal/digitalocean.TestAnAgentsTokenIsNeverTheAtlass`, `internal/digitalocean.TestAScopeTheTokenLacksIsAFindingAndRetiresNothing`, `internal/digitalocean.TestNoCredentialShapeReachesTheAtlasFromDigitalOcean`, `internal/digitalocean.TestAListingThatFailsFailsTheDigitalOceanRead`, `internal/digitalocean.TestADigitalOceanWithNoAtlasTokenIsNotConfigured`, `internal/digitalocean.TestTheConnectFormTestsTheAtlasToken`, `internal/expo.TestTheAtlasReadsTheAccountsProjectsAndBuilds`, `internal/expo.TestARobotThatIsNoViewerReadsNothing`, `internal/expo.TestAnAgentsRobotIsNeverTheAtlass`, `internal/expo.TestNoCredentialShapeReachesTheAtlasFromExpo`, `internal/expo.TestAProjectListingIsReadToItsEndOrNotAtAll`, `internal/expo.TestAnExpoWithNoAtlasTokenIsNotConfigured`, `internal/expo.TestTheConnectFormTestsTheAtlasRobot`
- **Cost-when-tried:** not tried. Every source before this one read under a credential the company had already given the engine. DigitalOcean and Expo are the first two vendors where the company's half holds no credential at all: every token is one agent's.
- **Tag-status:** unreleased

## The decision

**The atlas reads DigitalOcean and Expo with an identity a person makes for it alone.** Neither vendor lets software create a credential, so a person creates one more: at DigitalOcean a custom-scoped personal access token named `crewlet-atlas`, at Expo a robot named `crewlet-atlas` with the Viewer role. Each is pasted in the vendor's connect form, sealed like every other credential, and named by one new setting (`integrations.digitalocean.atlas.token`, `integrations.expo.atlas.token`).

**It is optional, and its absence is not a fault.** A company without it is not configured for the read: the source's card says it is not read yet, the connect form offers the field as a way to see more in the atlas, and nothing an agent does waits on it.

**The identity is proven before every read, never only when it is pasted.** A sealed token can be replaced by one of another kind, and at Expo a robot's role can be edited after its token was pasted. So the same check answers the form's Test button and opens every read. At DigitalOcean, where a token cannot read its own scopes, it is the proof the seats' tokens already take: requests DigitalOcean refuses with a `403` before they can have an effect. A token that holds any write scope the proof reaches, or any scope that reads a secret (billing, stored secrets, registry credentials, Spaces keys, cluster credentials, database passwords, consoles), reads nothing. At Expo the token must be a robot's and the robot must hold the Viewer role and no other. A refused identity fails the read, which records and retires nothing.

**The atlas never reads as an agent.** A DigitalOcean token sealed for a seat, and an Expo robot that holds a token sealed for a seat, is refused as the atlas's. An agent's credential standing in for the atlas would put the atlas's hourly reads under the agent's name at the vendor, and would tie the atlas's access to the agent's level.

**The scope list is explicit, and the decoders are closed.** DigitalOcean: `account:read`, `droplet:read`, `kubernetes:read`, `database:read`, `load_balancer:read`, `domain:read`, `app:read` and `project:read`. Three of those listings carry more than the atlas keeps (a database's connection and users, an app's environment variables, a cluster's endpoint), so each row is decoded into a type that has no field for them. A domain's records are asked for by type (A, AAAA, CNAME), so a TXT record is never sent. Expo's read is one GraphQL query that names every field it is sent, none of them a credential, an environment value, a message or an artifact.

**A scope the token lacks is a part left unread** (ADR-0130): the listing is a finding on the source's standing, the rest is read, and what an earlier read of that part recorded stands. An app's attachment to a managed database takes both listings to say, so a read refused either one carries it.

**What each thing is.** A Droplet, a Kubernetes cluster and a load balancer are each a `host`; a managed database is a `datastore`, and a Kafka cluster a `queue`; a Spaces bucket is a `bucket`; an App Platform app and an Expo project are each the `service` their name or slug names, so what a monitor or a repository says of that service is said of the same entry. A name whose record points at an address one of the team's own Droplets or load balancers answers on resolves to that resource; two entries of one voice sharing an address merge nothing, so claiming a second server by its address would leave the name pointing beside the Droplet. An Expo project's builds are one `pipeline` per platform, keyed by the project's own address (`expo.dev`, the account and the slug) rather than by a repository, since a project need not be linked to one and two may share one.

**A bucket is named from its project.** DigitalOcean lists Spaces buckets to a Spaces access key alone, which no token of the atlas may create or hold. A project lists its resources by name, a bucket among them, under `project:read`.

**A DigitalOcean team is keyed by its name's slug.** A team's name may hold a space and an account's key may not, so the settings and the read both key the account through `config.DigitalOceanTeamRef`. Before this record the settings keyed it by the name lowercased, and a team named with a space was no entry.

## Why the obvious alternative is wrong

**Reading with one of the agents' tokens** needs no setup step, and is wrong three ways: the vendor's own log then shows an agent making reads no agent made, the atlas's reach becomes whatever level that agent holds (an `administer` token reads database passwords), and turning the agent off blinds the atlas.

**Checking the token once, when it is pasted,** trusts a sealed name for ever. The form's check is a button a person may not press, the sealed value can be replaced from the command line, and an Expo robot's role is edited at Expo. The read is where the token is used, so the read is where it is proven.

**Reading with a token that can write, since the atlas only sends reads,** makes the narrowness of the identity a property of the engine's code rather than of the credential. The point of a dedicated identity is that the vendor enforces what it can do.

**Leaving out `app:read` and `database:read` because their answers carry more than the atlas keeps** would leave out the two things a person most wants on the map. The scopes that return the secrets themselves (`database:view_credentials`, `kubernetes:access_cluster`, `app:access_console`) are refused; what arrives beside the mapped fields under the plain read scopes has nowhere to go, and the corpus test plants a credential in each such field.

**Listing every record of a domain and skipping the TXT ones** receives the verification tokens it then drops. Asking by type never receives them.

**A kind for a load balancer, a cluster or a mobile build** adds kinds for what the vocabulary already holds: something a name resolves to and a workload runs on is a host, and something that builds code and has a last result is a pipeline.

**Keying an Expo build pipeline by the linked GitHub repository** leaves an unlinked project with no pipeline at all, and gives two projects of one monorepo the same two keys.

**A finding on the source for a missing token** would mark every company that connected DigitalOcean for its agents as partly read, for an option it never asked for.

## What this does not decide

Whether a build's history, its artifacts or an update channel is read. Whether a Kubernetes cluster's workloads are read, which takes cluster credentials the atlas refuses to hold. Whether DigitalOcean's firewalls, volumes or monitoring alerts become entries. Whether an account may relate to another account. How a not yet read source is shown, which the dashboard decides.
