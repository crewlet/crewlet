# Scaleway

Scaleway gives each agent its **own IAM application**, with one policy granting its level on the projects you choose and its own API key, and the **Scaleway CLI** (`scw`) to use it through, in its box or as the CLI's own MCP server. An agent reads servers, storage, networks, DNS and databases in its projects as itself and, where you allow it, restarts servers and changes firewalls, IPs, load balancers and private networks.

The engine makes, narrows, rotates and deletes those applications, policies and keys itself, through one API key you create for it. Nothing here receives events from Scaleway yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Read servers, disks, bucket settings and object names, load balancers, private networks, VPCs and their gateways, IP addresses, DNS zones, databases, Redis clusters, registry images and Cockpit, in its projects, plus the Organization's projects, quotas and audit trail | Change anything, read an object's contents, or read a secret or a key |
| `operate` | `read`, plus start and stop servers, change security groups, flexible IPs, private NICs and placement groups, and manage load balancers, private networks, VPCs, their gateways, IP addresses and Grafana dashboards | Create, rebuild or delete a server or a disk, reach a database's or a cluster's contents, or open a console |
| `administer` | `operate`, plus create, rebuild and delete servers, disks, databases, Redis clusters, Kubernetes clusters and registry images | Anything on the never-granted list below |

Each agent's application is its own, so the audit trail names the agent behind each change, and turning one agent off never touches another.

**No level grants**: IAM or Organization writes, billing, Secret Manager, Key Manager, SSH keys, Cockpit token minting, object contents, bucket policies, a bucket's settings and ACL, DNS writes, and the all-products bundles. A test holds every level to that list.

When one agent needs one permission set no level holds (a cluster upgrade on `read`, a bucket's ACL), give it that set alone as a **grant** beside its level; see [Giving one agent one more permission](#giving-one-agent-one-more-permission). The agent can ask for one itself when Scaleway refuses it. A bucket's settings and ACL are granted only this way, for one change and a few hours at a time; everything else on the list above is never granted.

Two things a `read` agent can see:

- **A server's user data.** InstancesReadOnly reads the cloud-init script a server was started with. Keep secrets out of user data: put them in Secret Manager, which no level reaches.
- **DNS.** DomainsDNSReadOnly reads your DNS zones and your domains' registration. A domain's transfer code exists only once a person unlocks the domain for transfer, and no level can unlock one, since DomainsDNSFullAccess is never granted.

## Setting it up from the dashboard

Company › Integrations › Scaleway asks three things and checks them before it saves:

1. **Organization ID**: the UUID in your Organization settings in the Scaleway console.
2. **Access key**: the provisioner application's API access key, `SCW` followed by 17 capital letters or digits.
3. **Secret key**: that API key's secret, a UUID. See [Creating the provisioner key](#creating-the-provisioner-key).

The form seals the secret key as `SCALEWAY_SECRET_KEY`, points the config at it and activates. The next pass makes an application for every agent seat that is on.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

### Creating the provisioner key

The provisioner is an IAM **application**, not a user, so it keeps working when its creator leaves, and its key carries nothing a person can do. Crewlet uses it only to make, narrow, rotate and delete the agents' applications, policies and keys. No agent ever holds it.

1. In the Scaleway console, open **IAM**, then **Applications**, and create an application named `crewlet-provisioner`.
2. Open **Policies** and create one policy for that application with one rule at **Organization** scope, granting exactly these permission sets:
   | Permission set | Why Crewlet needs it |
   |---|---|
   | IAMApplicationManager | Create and delete the agents' applications, and mint and delete their API keys |
   | IAMPolicyManager | Write each agent's policy, and rewrite it when its level or projects change |
   | ProjectReadOnly | List the projects an agent may be given |

   IAMManager holds all three and works too, as more than Crewlet needs; the Test button says so.
3. Back on the application, open **API keys** and generate a key with a **short expiry**, such as 30 days. Scaleway shows the secret key once.

Paste the access key and secret key into the form, or seal the secret yourself with `crewlet secrets set SCALEWAY_SECRET_KEY`.

### What the provisioner key can do

This key can mint a key for, and grant anything to, **any application in the Organization**, not only the agents'. Scaleway checks no grant against what the granter holds, and an API key has no attribute naming its application, so no condition can limit the provisioner to the agents' applications: a condition on the resource refuses the agents' keys too, and the Test button refuses it. Treat the key as an Organization administrator for IAM:

- **Give it a short expiry and rotate it.** Expiry cannot change after creation, so rotating means a new key: generate one, replace it on the connection with **Replace key**, then delete the old one.
- **Keep it sealed.** The form seals it; nothing in the company document or a seat's environment may name it.
- **Pin it to the engine's address where you can.** On an engine with a fixed public egress address, add a condition to the provisioner's rule, `request.ip == "203.0.113.7"` with your engine's address, and a leaked key is useless elsewhere. An engine whose address changes (a laptop) cannot pin.
- **Read the audit trail.** Every application, policy and key the engine makes is there, under the provisioner's name.

What bounds an agent is Crewlet's fixed level table, its never-granted list, and its rule of acting only on applications its ledger recorded when it made them.

### Testing the key before you save it

**Test key**, under the secret key, runs read-only calls in order and stops at the first failure, so the result names what to fix. A test stores nothing.

Before any call, each value is checked against Scaleway's shape: the Organization ID and secret key are UUIDs; the access key is `SCW` and 17 capital letters or digits. A value of another shape, or with a space, line break or invisible character, is refused beside its field and nothing is sent. Saving refuses the same values.

| Stage | Call | What a failure means |
|---|---|---|
| The key works | `GET /iam/v1alpha1/api-keys/{access_key}`, else the key listing filtered to it | Secret wrong, expired or deleted; access key is another key's; or the key belongs to a user, not an application |
| It is in the Organization | `GET /iam/v1alpha1/applications/{id}` | The key's application is in another Organization |
| It holds what it needs | `GET /iam/v1alpha1/policies?application_ids=...` and each policy's rules | A rule at Organization scope does not grant IAMApplicationManager, IAMPolicyManager and ProjectReadOnly, or the rule granting IAMApplicationManager carries a condition on the resource |
| Every level resolves | `GET /iam/v1alpha1/permission-sets` | A permission set a level names is not offered to the Organization |
| It can see the projects | `GET /account/v3/projects` | It lacks ProjectReadOnly |

When every stage passes, the result lists the application, project count, key expiry and any condition on its rules as facts, plus one warning per thing to act on, with what to do: what the key can reach, a near or absent expiry, a set held beyond the three, a level the Organization cannot be granted, and the IP pin where no rule has one. Once connected, each pass repeats the check and reports on the card.

## Giving each agent its own Scaleway application

Turn each agent seat on or off, and set its level and projects, on the integration's **Agents** tab (or the agent's own page, same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.scaleway` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a Scaleway application. Required in every block. A seat with no block is off; a new agent seat is off until somebody turns it on. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. `administer` needs a fresh sign-in. |
| `projects` | Scaleway project IDs its policy grants its level on. Required while the agent is on: an agent in no project reaches nothing. Each must be a project of the Organization. |
| `grants` | Permission sets its policy holds beside its level, each `{id, permission, scope, expires_at, granted_by, granted_at, reason, request, target}`. Written by the Agents tab and by approving the agent's request, never typed: the engine writes `id`, `granted_by` and `granted_at`, and copies `target` from the request. See [Giving one agent one more permission](#giving-one-agent-one-more-permission). |

```yaml
roles:
  - handle: sre
    integrations:
      scaleway:
        enabled: true
        level: operate
        projects: [aaaaaaaa-0000-4000-8000-000000000001]
```

### Each agent's access is a level

A level is a fixed set of Scaleway permission sets, the same in every company, granted on the seat's projects:

| Level | Permission sets |
|---|---|
| `read` | InstancesReadOnly, BlockStorageReadOnly, ObjectStorageBucketsRead, LoadBalancersReadOnly, PrivateNetworksReadOnly, VPCReadOnly, VPCGatewayReadOnly, IPAMReadOnly, DomainsDNSReadOnly, RelationalDatabasesReadOnly, RedisReadOnly, ContainerRegistryReadOnly and ObservabilityReadOnly |
| `operate` | Everything in `read`, plus InstancesServerStart, InstancesServerStop, InstancesSecurityGroupCreate, InstancesSecurityGroupWrite, InstancesSecurityGroupDelete, InstancesIpCreate, InstancesIpWrite, InstancesIpDelete, InstancesPrivateNicCreate, InstancesPrivateNicWrite, InstancesPrivateNicDelete, InstancesPrivateNetworkInterfaceCreate, InstancesPrivateNetworkInterfaceWrite, InstancesPrivateNetworkInterfaceDelete, InstancesPlacementGroupCreate, InstancesPlacementGroupWrite, InstancesPlacementGroupDelete, LoadBalancersFullAccess, PrivateNetworksFullAccess, VPCFullAccess, VPCGatewayFullAccess, IPAMFullAccess and ObservabilityGrafanaEditor |
| `administer` | Everything in `operate`, plus InstancesFullAccess, BlockStorageFullAccess, RelationalDatabasesFullAccess, RedisFullAccess, KubernetesFullAccess and ContainerRegistryFullAccess |

Every level also grants AuditTrailReadOnly, OrganizationReadOnly and ProjectReadOnly at Organization scope, in a second rule of the same policy, so the CLI can resolve the projects and the agent can read the audit trail.

Crewlet checks each set against the Organization's own list (`GET /iam/v1alpha1/permission-sets`) on every pass. A set the Organization does not offer at the scope a level grants it at is **a finding, never a fallback**: the level is offered disabled, naming the set, and a seat on it is not granted less, or more, than its level says.

`operate` changes power and networking only. It cannot create or rewrite a server, open its console, or copy a disk, so nothing it can do plants a credential.

### Administer, and what it can leave behind

`administer` creates, rebuilds and deletes servers, disks, databases, Kubernetes clusters and registry images. Its reach can leave access behind after the level is removed:

- **Servers:** user data and rescue boot run code, which can install an SSH key or any credential; the serial console reaches a login prompt. Snapshots and images copy a disk with every credential on it.
- **Databases and Redis:** it can create users and read connection details.
- **Kubernetes:** a cluster's kubeconfig carries the cluster's admin token.
- **The registry:** it can make an image public.

Granting it needs a fresh sign-in. When you move an agent off `administer` or turn it off, the Agents tab says the same; rotate what it could reach: SSH keys and startup scripts on its servers, database users, kubeconfigs and image visibility.

### What each change does at Scaleway

| You do | Crewlet does at Scaleway |
|---|---|
| Turn a seat on | Creates an application named `crewlet-<handle>` (from the one rule Crewlet names every agent identity by, at most Scaleway's 64 characters) (`POST /iam/v1alpha1/applications`) and a same-named policy granting its level on its projects (`POST /iam/v1alpha1/policies`). Then mints an API key expiring in 90 days, its default project the seat's first project by ID (`POST /iam/v1alpha1/api-keys`), and seals the secret and access key. A policy Scaleway refuses takes its application back with it. |
| Change its level, projects or grants | Rewrites the policy's rules in place (`PUT /iam/v1alpha1/rules`) and moves the key's default project if the seat no longer names it (`PATCH /iam/v1alpha1/api-keys/{access_key}`). The key is unchanged, so the agent's tools keep working. |
| Turn it off | Deletes every key on the application (the agent stops at once), then its policy, then the application (`DELETE` on each), and clears the sealed values. If the application's delete fails, the next pass deletes it. |
| Turn it back on | Makes a new application, policy and key. No person is needed. |
| Remove it (only while off) | Deletes whatever off left and stops tracking it. |
| Rotate its key | Mints a new key, seals it, then deletes the old one. |
### Giving one agent one more permission

A grant is one Scaleway permission set in one agent's policy beside its level: the SRE on `read` that has to upgrade a cluster gets KubernetesFullAccess on its own project for four hours, and nobody else's policy changes.

On the **Agents** tab, each agent's row lists its grants (an agent that is off lists none): the set, where it applies, how long it has left (or Permanent), and, on hover, who granted it and why. **Add grant** opens a picker over the Organization's own permission sets, read live from Scaleway (`GET /iam/v1alpha1/permission-sets`), by category, with the projects it applies to (none chosen is the agent's own projects; an Organization set takes none), how long (4 hours, 1 day, 7 days or Permanent; 1 hour or 4 hours for a set [granted for one change](#changing-a-buckets-acl-and-visibility)) and why, which the grant keeps. The remove on a grant's tag takes it off. Both change the tab's draft, so the review shows the steps and **Save and sync** writes one revision of the company document, under the seat's `grants`, and rewrites the agent's policy in place on the next sync (`PUT /iam/v1alpha1/rules`), so its key does not change.

- **The grant rides the agent's one policy.** A project set joins the level's rule where it applies to the agent's own projects, or gets a rule of its own on the projects it names; an Organization set joins the Organization rule.
- **Never granted:** the sets on the list under [What agents get](#what-agents-get) other than a bucket's settings: IAM and the Organization's and projects' managers, billing, secrets and keys, SSH keys, Cockpit's full access, object contents (ObjectStorageFullAccess, ObjectStorageReadOnly, ObjectStorageObjectsRead, ObjectStorageObjectsWrite), bucket policies (ObjectStorageBucketPolicyFullAccess), DNS writes, Kubernetes' system:masters and the all-products bundles. Each would outlive the agent's key, let it give itself more, or reach what the levels keep closed. A test holds the list.
- **A write asks what `administer` asks.** Adding a grant of any set whose name does not end in `Read` or `ReadOnly` asks the person saving for a fresh sign-in, and a machine token cannot save one. Removing or shortening a grant never does.
- **A grant never outlives its expiry.** It leaves the policy on the pass a minute before it lapses, and the reconcile loop runs that pass at the lapse rather than on its settled interval. A set somebody adds to the policy at Scaleway is taken off and reported, as any other change there is.
- **A grant Scaleway cannot give** (a set the Organization no longer lists, say) is reported as `unknown_tier` on the agent's row and left off its policy; the rest of the policy still matches its level and its other grants. The provisioner can grant any set the Organization lists, so a grant is short only where its own key is, which `grant_short` reports.
- **Grants a level already holds add nothing**: the picker says which level holds each, and an agent asking for one is told it already has it.

A grant is also a seat's own access for the turn: its "Your access" section lists each grant its policy holds, with where and until when, and the next turn after a change is told it.

#### Changing a bucket's ACL and visibility

No level changes a bucket. An agent that has to close a public bucket listing, or change any other setting of a bucket, is granted **ObjectStorageBucketsWrite** for that one change. It is the narrowest set Scaleway offers that allows `PutBucketAcl`, which is what a bucket's visibility is (Public or Private in the console: whether anyone can list its objects). Scaleway grants it whole, so it also allows, in every bucket of the projects it is granted on:

- creating buckets;
- changing a bucket's CORS, website, versioning and tags;
- changing its lifecycle rules, which can expire and delete objects;
- writing any ACL, including `public-read`, which lets anyone list the bucket, and `public-read-write`, which lets anyone, the agent included, write objects into it.

It reads no object, writes no object under the agent's own key, and changes no bucket policy (Scaleway's [S3 and IAM permissions equivalence](https://www.scaleway.com/en/docs/object-storage/reference-content/s3-iam-permissions-equivalence/) lists each action it covers). So Crewlet grants it **for one change only**:

- **At most 4 hours, never permanently.** An agent's request lasts 1 hour unless it asks for up to 4, the Inbox offers no **Add to this agent permanently**, the Agents tab offers 1 hour or 4 hours, and a save of a longer or permanent grant is refused. One stored for good is never applied.
- **The request names the bucket.** The agent puts the bucket in `target` and the change in `why`; the Inbox asks "Grant SRE ObjectStorageBucketsWrite on the agent's own projects at Scaleway, for one change to user-uploads?" with the agent's words beside it, and says what else the set allows. The grant keeps the target, "Your access" tells the agent it holds the set "for one change to user-uploads", and every `seat_grant_changed` event names it.
- **The grant still reaches every bucket in its projects.** Scaleway scopes a permission set to projects, never to one bucket, so the target records what the change is for and cannot hold the key to it.
- **Scaleway cannot say what was done with it.** Scaleway's Audit Trail does not cover Object Storage yet, and a bucket keeps no access log, so no call made under the grant can be recorded. The `seat_grant_changed` events record the grant's request, approval and lapse.

A bucket that has a **bucket policy** answers only the principals that policy names, so the agent's `PutBucketAcl` is refused there whatever its grant (`AccessDenied`), and a listing a policy opens to everyone (`"Principal": "*"`) stays open whatever the ACL says. Bucket policies are never granted to an agent, since one can hand any principal the bucket's objects: a person changes the policy at Scaleway.

```yaml
roles:
  - handle: sre
    integrations:
      scaleway:
        enabled: true
        level: read
        projects: [aaaaaaaa-0000-4000-8000-000000000001]
        grants:
          - id: g4k2m9q1x7z
            permission: ObjectStorageBucketsWrite
            expires_at: 2026-11-15T15:00:00Z
            granted_by: ada@example.com
            granted_at: 2026-11-15T14:00:00Z
            reason: Make bucket user-uploads private; its object listing is public
            request: k3m9q2x7za
            target: user-uploads
```

#### An agent asks for one itself

When Scaleway refuses an agent's call for a permission its key lacks (`403` with `permissions_denied` from the API, or `AccessDenied` from Object Storage), the agent asks for that permission set with `request_access`, naming the set as Scaleway names it, its projects (none is its own) and how many hours the change needs (1 to 168, 4 by default; 1 to 4, 1 by default, for a set granted for one change, which also needs `target`), on the work item that needs it and of an administrator. It never asks a person to edit its policy at Scaleway or to run the change by hand: the next pass would undo a policy edit as drift.

The engine checks the request against the Organization's live catalogue, stores it and puts it in that person's Inbox as a decision: **Grant for this change (expires in N hours)**, recommended, **Add to this agent permanently** (never for a set granted for one change), or **Not now**, with the agent's reason and what the set allows beyond its name. Choosing a grant shows the grant built from the stored request; **Grant** answers the question with that option, which is the approval itself: the engine saves the grant from the stored request first and only then posts the answer, so the agent is told yes only once its policy holds the set. It asks for an administrator signed in within the step-up window (and in person, for a set that changes things), the same from the Inbox, from Ask Crewlet and from the API, and an agent can never choose it. **Not now** answers at once. A request nobody answers is dropped after 30 days.

Every grant asked for, granted, declined, removed, lapsed or refused is a `seat_grant_changed` event, naming the agent, the set, its projects and its target, and the person or the engine behind it.

### Only what the ledger recorded is the engine's

Application names are not unique and anyone with IAM rights can create one under any name, so a name proves nothing. Crewlet acts only on applications whose IDs its ledger recorded when it made them. On such an application the policy under the recorded name is the engine's, and the keys described `crewlet seat <handle>` are the ones it minted.

Anything else on an agent's application is somebody else's and is never deleted while the agent is on: each pass notes on the card a policy under another name and any key besides the sealed one. Turning the agent off deletes the application and every key on it.

### The key is the engine's, and named for the seat

The secret is sealed as `SCALEWAY_SECRET_KEY_<HANDLE>` and the access key as `SCALEWAY_ACCESS_KEY_<HANDLE>` (for example `SCALEWAY_SECRET_KEY_SRE` and `SCALEWAY_ACCESS_KEY_SRE`). The engine derives both names, so the company document cannot point a seat's key at another variable or the provisioner's. Scaleway shows a secret once, so a key minted but **not sealed is deleted** at once rather than left live and unheld.

### Expiry and rotation

Every agent key expires **90 days** after minting. Scaleway allows no expiry change, so when fewer than **30 days** remain the next pass mints a new key, seals it and deletes the old one. A key never lapses while the integration is connected, and a leaked value works for at most 90 days.

**Rotate credential** on the Agents tab does the same on demand, for a key you believe leaked.

If the Organization's security settings cap an API key's lifetime below 90 days, it refuses the agents' keys. The seat reports it, naming the cap and linking the security settings, and mints nothing shorter. Raise the cap to at least 90 days.

### The provisioner key's own expiry

Nothing rotates the provisioner key; only a person can replace it. Each pass reads its expiry and, inside **14 days**, reports a `credential_expiring` finding naming `integrations.scaleway.provisioning.secret_key` and the date. The integration stays connected. Once it lapses every pass is refused: no agent's application, policy or key can be made, changed or rotated. Keys already minted work until their own expiry.

### The agent's Scaleway tools

An agent needs a tool as well as a key. Either route counts: the Agents tab reports the tool live when the box carries the seat's key or a per-seat MCP server reads it.

**The scw CLI in its box, as itself.** The CLI reads `SCW_SECRET_KEY` and `SCW_ACCESS_KEY`. While an agent is on at Scaleway, Crewlet sets both in its box to its own sealed key, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)); an agent turned off gets neither from its next run. The box needs only the CLI itself:

```yaml
roles:
  - handle: sre
    sandbox:
      setup:
        - name: scw
          commands:
            - |
              set -eu
              case "$(dpkg --print-architecture)" in
                amd64) a=amd64; sum=184e697e6ab8a8e825be1c5cf61d104341abe3598bb0e20b7a54618cb05231b3 ;;
                arm64) a=arm64; sum=28f2115b7fce9d23f5726dc38566c7c973f579804be78a46f908c1a57da71893 ;;
                *) echo "scw: no release for this architecture" >&2; exit 1 ;;
              esac
              cd "$(mktemp -d)"
              code=$(curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors -o scw -w '%{http_code}' \
                "https://github.com/scaleway/scaleway-cli/releases/download/v2.65.2/scaleway-cli_2.65.2_linux_${a}") || {
                s=$?; case $code in 408|429|5??) exit 75 ;; esac; exit "$s"; }
              echo "${sum}  scw" | sha256sum -c -
              mkdir -p "$HOME/.local/bin" && install -m 0755 scw "$HOME/.local/bin/scw"
          timeout_seconds: 300
```

The engine also sets, beneath every line you write: `SCW_DEFAULT_ORGANIZATION_ID` (the connection's Organization), `SCW_DEFAULT_PROJECT_ID` (the seat's first project by ID) and `SCW_DISABLE_CHECK_VERSION=true`. A line of yours under those names wins. Region and zone stay at the CLI's defaults (`fr-par`, `fr-par-1`) unless you set `SCW_DEFAULT_REGION` or `SCW_DEFAULT_ZONE`. A line of yours under `SCW_SECRET_KEY` or `SCW_ACCESS_KEY` is ignored and reported as an advisory at that line. Until a key is sealed, a run starts without it and says it is waiting for the Scaleway integration (`sandbox_env_unresolved`, at info; a warning where the integration cannot say whether it holds the agent up, unless this node is only still reading its findings: [`unsure`](../concepts/code-sandbox.md#failure-modes)).

The step downloads the pinned scw release and checks it against its published checksum, the pattern [recipe 1](../concepts/code-sandbox.md#bring-your-own-image) uses for `gh`. An image you build can bake it in.

**The scw CLI's own MCP server.** `scw mcp server serve` serves each CLI command as a tool over stdio. Declare it once as a per-seat server, with no credential:

```yaml
mcp_servers:
  - name: scaleway
    transport: stdio
    shared: false                       # one instance per agent seat that is on
    command: scw
    args: [mcp, server, serve, "namespaces=account,audit-trail,block,cockpit,dns,instance,ipam,k8s,lb,object,rdb,redis,registry,vpc,vpc-gw"]
    env:
      SCW_DEFAULT_ORGANIZATION_ID: 11111111-2222-4333-8444-555555555555
      SCW_DISABLE_CHECK_VERSION: "true"
```

The engine adds each seat's own key to its instance as `SCW_SECRET_KEY` and `SCW_ACCESS_KEY`. The `namespaces=` filter serves only the products the levels reach; any other command would be refused by the agent's policy. The server has no project, so an agent names one in a command that needs it. It runs on the engine host, so install scw, at the same release, on every engine host that runs agent seats. On an Apple silicon Mac:

```sh
curl -fsSLo scw https://github.com/scaleway/scaleway-cli/releases/download/v2.65.2/scaleway-cli_2.65.2_darwin_arm64
echo "17b756a7a02659e4db49b27e92d0039776a124f79587ee3366fda83c2d2e3e0b  scw" | shasum -a 256 -c -
chmod +x scw && sudo mv scw /usr/local/bin/scw
```
| Platform | Release file | SHA-256 |
|---|---|---|
| Linux, x86-64 | `scaleway-cli_2.65.2_linux_amd64` | `184e697e6ab8a8e825be1c5cf61d104341abe3598bb0e20b7a54618cb05231b3` |
| Linux, ARM64 | `scaleway-cli_2.65.2_linux_arm64` | `28f2115b7fce9d23f5726dc38566c7c973f579804be78a46f908c1a57da71893` |
| macOS, Intel | `scaleway-cli_2.65.2_darwin_amd64` | `90dc1d10c622cbd6b8dc42490fd2b1f7803e1ea2201952b466e39730fa394c47` |
| macOS, Apple silicon | `scaleway-cli_2.65.2_darwin_arm64` | `17b756a7a02659e4db49b27e92d0039776a124f79587ee3366fda83c2d2e3e0b` |

Adding the server from Company › Tools › MCP servers offers it from the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) as `scaleway`.

### Every pass reads each application back

Each pass reads every application this engine manages, including that of a seat that has left the document.

- **A policy's rules changed at Scaleway** by somebody else are restored to the seat's level and projects, and recorded as a `drift_restored` event with the before and after.
- **A policy somebody deleted** at Scaleway is read as a person turning the agent off there (perhaps incident response), and no pass writes it back. The tab offers **Turn back on** or **Keep off**.
- **An application deleted at Scaleway** is dropped from the engine's records, and the same pass makes a fresh one if the seat is on.
- **A key deleted at Scaleway**, or about to expire, is replaced in the same pass.
- **A seat that leaves the document** is treated as off: its keys, policy and application are deleted.

A refusal names the failed step, Scaleway's reason, what to do and who does it, with a link to the console page that settles it. The step is retried on every pass and on **Retry**.

### Disconnecting

Disconnecting deletes every application this engine made, with its policy and keys, and clears their sealed values, including the [Atlas application](#see-more-in-atlas). The provisioner application and its key are yours: they stay at Scaleway and in the secret store until you delete them.

## Configuration

```yaml
integrations:
  scaleway:
    enabled: true
    provisioning:
      organization_id: 11111111-2222-4333-8444-555555555555
      access_key: SCWXXXXXXXXXXXXXXXXX
      secret_key: "${SCALEWAY_SECRET_KEY}"   # the provisioner key's secret
    atlas: true                              # optional: see more in Atlas
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning` | yes | The Organization and key the engine makes each agent's application with. Required when enabled: Scaleway sends nothing in, so without it the block does nothing. |
| `provisioning.organization_id` | yes | The Organization the applications are created in: a UUID, or a `${VAR}`. |
| `provisioning.access_key` | yes | The provisioner key's access key, `SCW` plus 17 capital letters or digits, or a `${VAR}`. It names the key and grants nothing alone. |
| `provisioning.secret_key` | yes | The [provisioner key's](#creating-the-provisioner-key) secret. Keep it a `${VAR}` into the secret store; the dashboard seals it as `SCALEWAY_SECRET_KEY`. |
| `atlas` | no | [See more in Atlas](#see-more-in-atlas): a read-only IAM application named for Atlas reads what the projects hold. Off by default; off deletes the application, policy and key. |

## What the atlas reads

The engine reads the Organization's projects for the company's [atlas](../concepts/atlas.md#what-integrations-read) once an hour, unless the company's `atlas.refresh_interval_seconds` says otherwise. It reads under the provisioner key, with the `ProjectReadOnly` permission set its policy already holds. Nothing at Scaleway is changed.

| Part | Calls | Permission set | What the atlas gains |
|---|---|---|---|
| Projects | `GET /account/v3/projects`, 100 a page | `ProjectReadOnly` on the Organization | the Organization, and each project as an account, with its id and name |

- **The default project is the Organization.** Scaleway gives it the Organization's id, so the atlas shows one account.
- **Nothing inside a project is read under the provisioner key**: its policy holds project read and IAM management, no product's permission set. No description, creator or key is read from a project either. What a project holds is read only under [See more in Atlas](#see-more-in-atlas).
- **A refusal is a finding, never a failed read.** On a `403` (the provisioner's policy lost `ProjectReadOnly`), the source's standing on the atlas says what to grant, and earlier recorded projects stay. A key Scaleway does not know fails the read as a rejected credential.
- **Cost and limits.** One call per hundred projects. A `429` is waited out like every call of this integration; a longer wait, or a listing failing part way, stops the read, which records nothing and retries five minutes later. A project that two complete reads at least a day apart both miss is retired from the atlas.

### See more in Atlas

The atlas can also show what your projects hold: servers, Kubernetes clusters, databases, load balancers, buckets, DNS zones, and Serverless Containers and Functions. It is optional, and nothing else waits on it.

**To turn it on**, open Company › Integrations › Scaleway, switch on **See more in Atlas** and save. Nothing to create at Scaleway: the provisioner key makes what is needed. In the company document the switch is `integrations.scaleway.atlas: true`.

Within one check of the connection (ten minutes by default) the engine makes, in your Organization:

- one IAM application named `crewlet-atlas-<company>`, described as Crewlet Atlas, so it shows as Atlas, never as an agent, in your IAM pages and audit trail;
- one policy on it, with one rule granting the nine read-only permission sets in the table below on every project of the Organization, and nothing else;
- one API key, expiring after 90 days and replaced once fewer than 30 remain. It is sealed as `SCALEWAY_ATLAS_SECRET_KEY`, beside `SCALEWAY_ATLAS_ACCESS_KEY` and `SCALEWAY_ATLAS_APPLICATION_ID`. The engine hands it to no agent, and the company document has no field for it.

**To undo it**, switch **See more in Atlas** off and save. The next check deletes the key, policy and application and clears the three sealed values. What the atlas recorded from them is retired after two complete reads a day apart, like anything it stops seeing. You can also revoke it at Scaleway alone by deleting the `crewlet-atlas-<company>` application on your [IAM applications page](https://console.scaleway.com/iam/applications). The engine does not make it again until you switch the setting off and on, and the Scaleway card says so. [Disconnecting](#disconnecting) deletes it too.

Each product is one part of the read, behind its own permission set, and every call is a `GET`:

| Part | Calls | Permission set | What the atlas gains |
|---|---|---|---|
| Servers | `GET /instance/v1/zones/{zone}/servers`, per zone | `InstancesReadOnly` | each Instance as a host: name, zone, public and private addresses |
| Kubernetes clusters | `GET /k8s/v1/regions/{region}/clusters`, per region | `KubernetesReadOnly` | each cluster as a host: name and region |
| Load balancers | `GET /lb/v1/zones/{zone}/lbs`, per zone | `LoadBalancersReadOnly` | each Load Balancer as a host: name, zone and addresses |
| Databases | `GET /rdb/v1/regions/{region}/instances`, per region | `RelationalDatabasesReadOnly` | each Managed Database as a datastore: name, region, engine and version |
| Redis clusters | `GET /redis/v1/zones/{zone}/clusters`, per zone | `RedisReadOnly` | each cluster as a datastore: name, zone and version |
| Buckets | Object Storage's `ListBuckets`, per project and region | `ObjectStorageBucketsRead` | each bucket, by name and region |
| DNS zones | `GET /domain/v2beta1/dns-zones`, then each zone's `/records` | `DomainsDNSReadOnly` | each zone as a domain, and each name an A, AAAA or CNAME record gives, with its target |
| Serverless Containers | `GET /containers/v1beta1/regions/{region}/namespaces` and `/containers`, per region | `ContainersReadOnly` | each container, by name |
| Serverless Functions | `GET /functions/v1beta1/regions/{region}/namespaces` and `/functions`, per region | `FunctionsReadOnly` | each function, by name |

Each entry belongs to its project's account, and each project is said to use what was found in it (compute, a database, object storage, DNS). A name whose record points at the address of a server or load balancer the same read holds resolves to that entry.

**What is never read.** No server's user data, cluster's kubeconfig, database's users, backups or logs, and no object or bucket policy. From the listings it does make, it never decodes a container's, function's or namespace's environment variables or secret environment variables, a server's encrypted administrator password, a database's or cache's endpoints, users and settings, a TXT, MX or other non-A/AAAA/CNAME record, or any tag, description or comment. Every kept value is held to the atlas's grammars, which refuse anything shaped like a credential.

**What the application could reach, so you can judge it.** Scaleway grants by permission set, and each read-only set covers every read of its product, more than the listings above. The key could therefore also read a server's user data (`InstancesReadOnly`), the plain environment variables of a container or function (`ContainersReadOnly`, `FunctionsReadOnly`; secret ones come back only as hashes), and, through each cluster's own API server, what the cluster's `scaleway:cluster-read` role allows: by default config maps and pod logs, not Secrets (`KubernetesReadOnly`). The engine calls none of those. If that reach is more than you want one sealed key to hold, leave the switch off: the atlas then shows your projects and nothing inside them.

**A refusal is a finding, never a failed read.** A product Scaleway refuses the Atlas key (its policy was changed, or the Organization cannot use the product) is left unread: the source's standing names the part and permission set, what an earlier read recorded stays, and every other product is still read. A permission set Scaleway does not list for your Organization is left out of the policy, and the card says which. A key Scaleway no longer accepts leaves every product unread until the next check replaces it; projects are still read under the provisioner key.

**Cost and limits.** About 40 calls an hour, plus three per project for buckets and one per DNS zone. The Atlas application counts as one application, one policy and one API key against the Organization's IAM quotas. A locality where Scaleway does not serve a product answers `404`, read as nothing there.

## Scaleway's limits worth knowing

These are Scaleway's, not Crewlet's.

| Limit | What it means |
|---|---|
| IAM quotas: by default 100 applications, 100 API keys and 50 policies per Organization | One application, policy and key per agent that is on, plus the provisioner's and any made by hand. A full quota refuses the next agent, naming the [quotas page](https://console.scaleway.com/organization/quotas); ask Scaleway support to raise it. The narrow provisioner cannot read the quotas, so the Test button does not report them |
| The Organization's maximum API key lifetime | Set in the Organization's security settings. Below 90 days, no agent key can be minted |
| Rate limits | A `429` carries `Retry-After`. The engine's calls wait up to 30 seconds, up to 3 times; a longer wait is left to the next pass |
| Propagation | Scaleway documents up to a minute for a policy change. Measured, it was immediate, and a deleted key stopped working within seconds |

## What is not supported yet

- **Inbound events.** Nothing from Scaleway can wake an agent: there is no webhook route for its alerts.
- **A CLI command.** Set up from the dashboard or company document; there is no `crewlet scaleway` command.
- **Secrets, keys and object contents.** No level or grant reaches Secret Manager, Key Manager, a bucket's contents or policy, and none writes DNS; see [Each agent's access is a level](#each-agents-access-is-a-level). A bucket's settings and ACL are a grant for [one change](#changing-a-buckets-acl-and-visibility).
- **A record of the bucket calls made under a grant.** Scaleway's Audit Trail does not cover Object Storage yet, and buckets keep no access log.

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The `provisioning` block is missing, or the Organization ID, access key or secret key resolved to nothing | Add the block, seal the provisioner secret as `SCALEWAY_SECRET_KEY`, or correct the field it names |
| `credential_rejected` | Scaleway refused the provisioner key, or it is a user's key or another Organization's | Create a new key for the provisioner application and replace it with **Replace key** |
| `credential_expiring` | The provisioner key expires within 14 days | Create a new key for the provisioner application and replace it before the date the finding names |
| `grant_short` | The provisioner application's policies do not grant IAMApplicationManager, IAMPolicyManager or ProjectReadOnly on the Organization, or the rule granting IAMApplicationManager carries a condition on the resource, which refuses the agents' keys | Add the set the finding names to a rule at Organization scope, or remove the condition. A condition on `request.ip` alone is the IP pin, and is fine |
| `grant_excess` | A note, not a fault: the provisioner holds a set beyond the three it needs, or an agent's or the Atlas application carries a policy Crewlet did not write, or a key besides its sealed one | Remove what nobody needs at Scaleway. Crewlet never deletes a policy or key it did not make while the agent is on |
| `unknown_tier` | A permission set the agent's level names is not offered to the Organization (subject `scaleway level read`, `scaleway level operate` or `scaleway level administer`), or a grant an agent holds (subject its handle) names a set the Organization does not list, or was granted for one change for longer than its bound, so it is left off the agent's policy | Check the Organization can use that product, or move the agents at that level to another. For a grant, remove it on the Agents tab and grant one the catalogue offers |
| `level_unavailable` | A note, not a fault: a level no agent is on names a set the Organization does not offer. The Agents tab offers that level disabled with this reason | Nothing, unless you want the level |
| `coverage_partial` | A note, not a fault, about [See more in Atlas](#see-more-in-atlas) (subject `integrations.scaleway.atlas`): the Atlas application is not made yet, was deleted at Scaleway, has no key because the Organization caps key lifetime below 90 days, lacks a permission set Scaleway does not list, or could not be brought up to date on this check | Usually nothing: the next check finishes it. For a deleted application, switch **See more in Atlas** off and on. For a capped lifetime, raise the cap in the Organization's security settings |
| `registration_orphaned` | Scaleway holds applications named like an agent's (`crewlet-...`), or the Atlas application's name (`crewlet-atlas-<company>`), that the engine has no record of. A name holds no company, so in an organization two companies share, each lists the other's agent applications too | Delete the ones nobody uses at Scaleway. Crewlet never deletes an application it did not make |
| `identity_failed` | An agent's application, policy or key could not be made, changed or rotated; a project it names is not in the Organization; somebody deleted its policy at Scaleway; or Scaleway would not read it back. Scaleway's reason is in the detail | Fix what the detail names, then **Retry** on the agent's row. For a policy deleted at Scaleway, choose **Turn back on** or **Keep off** |
| `identity_missing` | For up to about a minute after you first connect, "access migration has not finished": the one-time step every per-agent integration passes through | Nothing; it clears by itself |

An agent's own failures are also reported on its row on the **Agents** tab, with the control that answers them.

If an agent is refused changing a bucket's ACL or visibility (`403 AccessDenied` from Object Storage), no level grants it: approve the request it makes with `request_access` for ObjectStorageBucketsWrite, or add that grant on the Agents tab, for the one change. If the call is still refused with the grant in place, or the bucket's listing stays public once its ACL is private, the bucket has a bucket policy: change that policy at Scaleway.
