# Scaleway

Scaleway gives each agent its **own IAM application**, with one policy granting its level on the projects you choose and its own API key, and the **Scaleway CLI** (`scw`) to use it through, in its box or as the CLI's own MCP server. An agent reads servers, storage, networks, DNS and databases in its projects as itself and, where you allow it, restarts servers and changes firewalls, IPs, load balancers and private networks.

The engine makes, narrows, rotates and deletes those applications, policies and keys itself, through one API key you create for it. Nothing here receives events from Scaleway yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Read servers, disks, bucket settings and object names, load balancers, private networks, VPCs and their gateways, IP addresses, DNS zones, databases, Redis clusters, registry images and Cockpit, in its projects, plus the Organization's projects, quotas and audit trail | Change anything, read an object's contents, or read a secret or a key |
| `operate` | Everything `read` can, plus start and stop servers, change security groups, flexible IPs, private NICs and placement groups, and manage load balancers, private networks, VPCs, their gateways, IP addresses and Grafana dashboards | Create, rebuild or delete a server or a disk, reach a database's or a cluster's contents, or open a console |
| `administer` | Everything `operate` can, plus create, rebuild and delete servers, disks, databases, Redis clusters, Kubernetes clusters and registry images | Anything on the never-granted list below |

Every agent's application is its own, so Scaleway's audit trail names the agent behind each change, and turning one agent off never touches another.

**No level grants**, whatever the level: IAM or Organization writes, billing, Secret Manager, Key Manager, SSH keys, Cockpit token minting, object contents or bucket policies and ACLs, DNS writes, and the all-products bundles. A test holds every level to that list.

Two things a `read` agent can see deserve a word:

- **A server's user data.** InstancesReadOnly reads the cloud-init script a server was started with. Keep secrets out of user data: put them in Secret Manager, which no level reaches.
- **DNS.** DomainsDNSReadOnly reads your DNS zones and your domains' registration. A domain's transfer code exists only once a person deliberately unlocks the domain for transfer, and no level can unlock one, since DomainsDNSFullAccess is never granted.

## Setting it up from the dashboard

Company › Integrations › Scaleway asks three things and checks them before it saves:

1. **Organization ID**: the UUID in your Organization settings in the Scaleway console.
2. **Access key**: the access key of the provisioner application's API key, `SCW` followed by 17 capital letters or digits.
3. **Secret key**: that API key's secret, a UUID. See [Creating the provisioner key](#creating-the-provisioner-key).

The form seals the secret key as `SCALEWAY_SECRET_KEY`, points the config at it and activates. The next pass makes an application for every agent seat that is on.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

### Creating the provisioner key

The provisioner is an IAM **application**, not a user, so it keeps working when whoever created it leaves, and its key carries nothing a person can do. Crewlet uses it for one job: making, narrowing, rotating and deleting the agents' own applications, policies and keys. No agent ever holds it.

1. In the Scaleway console, open **IAM**, then **Applications**, and create an application named `crewlet-provisioner`.
2. Open **Policies** and create one policy for that application with one rule at **Organization** scope, granting exactly these permission sets:

   | Permission set | Why Crewlet needs it |
   |---|---|
   | IAMApplicationManager | Create and delete the agents' applications, and mint and delete their API keys |
   | IAMPolicyManager | Write each agent's policy, and rewrite it when its level or projects change |
   | ProjectReadOnly | List the projects an agent may be given |

   IAMManager holds all three and works too, as more than Crewlet needs; the Test button says so.
3. Back on the application, open **API keys** and generate a key. Give it a **short expiry**, such as 30 days. Scaleway shows the secret key once.

Paste the access key and the secret key into the form, or seal the secret yourself with `crewlet secrets set SCALEWAY_SECRET_KEY`.

### What the provisioner key can do

Be clear about what this key is. It can mint a key for, and grant anything to, **any application in the Organization**, not only the agents'. Scaleway checks no grant against what the granter holds, and an API key has no attribute naming its application, so no condition can keep the provisioner to the agents' applications: a condition on the resource refuses the agents' keys as well, and the Test button refuses it. Treat the key as an Organization administrator for IAM:

- **Give it a short expiry and rotate it.** A key's expiry cannot change once it is created, so rotating is a new key: generate one, replace it on the connection with **Replace key**, then delete the old one.
- **Keep it sealed.** The form seals it; nothing in the company document or a seat's environment may name it.
- **Pin it to the engine's address where you can.** On an engine with a fixed public egress address, add a condition to the provisioner's rule, `request.ip == "203.0.113.7"` with your engine's address, and a leaked key is useless anywhere else. An engine whose address changes, such as one on a laptop, cannot use the pin.
- **Read the audit trail.** Every application, policy and key the engine makes is there, under the provisioner's name.

What bounds an agent is Crewlet's fixed level table and never-granted list, and its rule of acting only on applications its ledger recorded when it made them.

### Testing the key before you save it

The form offers **Test key** under the secret key. It runs read-only calls in order and stops at the first that fails, so the result names what to fix. Nothing is stored by a test.

Before any call, each value is held to Scaleway's shape: the Organization ID and the secret key are UUIDs and the access key is `SCW` and 17 capital letters or digits. A value of any other shape, or one with a space, a line break or an invisible character inside, is refused beside its field and nothing is sent. Saving refuses the same values.

| Stage | Call | What a failure means |
|---|---|---|
| The key works | `GET /iam/v1alpha1/api-keys/{access_key}`, else the key listing filtered to it | The secret is wrong, expired or deleted, the access key is another key's, or the key belongs to a user rather than an application |
| It is in the Organization | `GET /iam/v1alpha1/applications/{id}` | The key's application is in another Organization |
| It holds what it needs | `GET /iam/v1alpha1/policies?application_ids=...` and each policy's rules | A rule at Organization scope does not grant IAMApplicationManager, IAMPolicyManager and ProjectReadOnly, or the rule granting IAMApplicationManager carries a condition on the resource |
| Every level resolves | `GET /iam/v1alpha1/permission-sets` | A permission set a level names is not offered to the Organization |
| It can see the projects | `GET /account/v3/projects` | It lacks ProjectReadOnly |

When every stage passes, the result lists the application, the number of projects, the key's expiry and any condition on its rules as facts, and gives each thing worth acting on as its own warning with what to do: what the key can reach, an expiry that is near or absent, a set it holds beyond the three, a level the Organization cannot be granted, and the IP pin where no rule carries one. Once connected, each pass makes the same check and reports what it finds on the card.

## Giving each agent its own Scaleway application

Each agent seat is turned on or off, and given its level and projects, on the integration's **Agents** tab (or the agent's own page, which uses the same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.scaleway` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a Scaleway application at all. Required in every block. A seat with no block is off, and a new agent seat is off until somebody turns it on. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. Choosing `administer` needs a fresh sign-in. |
| `projects` | The Scaleway project IDs its policy grants its level on. Required while the agent is on: an agent in no project can reach nothing. Each must be a project of the Organization. |

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

`administer` creates, rebuilds and deletes servers, disks, databases, Kubernetes clusters and registry images, and that reach can leave access behind after the level is taken away:

- **Servers:** user data and rescue boot run code on a server, which can install an SSH key or any credential, and the serial console reaches a login prompt. Snapshots and images copy a disk with every credential on it.
- **Databases and Redis:** it can create users and read connection details.
- **Kubernetes:** a cluster's kubeconfig carries the cluster's admin token.
- **The registry:** it can make an image public.

Granting it needs a fresh sign-in. When you move an agent off `administer`, or turn it off, the Agents tab says the same, and you should rotate what it could reach: SSH keys and startup scripts on its servers, database users, kubeconfigs and image visibility.

### What each change does at Scaleway

| You do | Crewlet does at Scaleway |
|---|---|
| Turn a seat on | Creates an application named `crewlet-<company>-<handle>` (the company name in lowercase, with anything that is not a letter or digit turned into `-`) (`POST /iam/v1alpha1/applications`) and one policy of the same name granting its level on its projects (`POST /iam/v1alpha1/policies`), then mints an API key expiring in 90 days with its first project by ID as its default (`POST /iam/v1alpha1/api-keys`), and seals the secret and the access key. A policy Scaleway refuses takes its application back with it. |
| Change its level or projects | Rewrites the policy's rules in place (`PUT /iam/v1alpha1/rules`), and moves the key's default project where the seat no longer names it (`PATCH /iam/v1alpha1/api-keys/{access_key}`). The key does not change, so the agent's tools keep working. |
| Turn it off | Deletes every key on the application, so the agent stops at once, and its policy, then the application (`DELETE` on each), and clears the sealed values. If the application's delete fails, the next pass deletes it. |
| Turn it back on | Makes a new application, policy and key. No person is needed. |
| Remove it (only while off) | Deletes whatever off left, and stops tracking it. |
| Rotate its key | Mints a new key, seals it, then deletes the old one. |

### Only what the ledger recorded is the engine's

Application names are not unique, and anyone with IAM rights can create one under any name, so a name is never proof of who made it. Crewlet acts only on the applications whose IDs its ledger recorded when it made them. On such an application, the policy under the name the ledger recorded is the engine's, and the keys carrying its description, `crewlet seat <handle>`, are the ones it minted.

Anything else on an agent's application is somebody else's, and Crewlet never deletes it while the agent is on: each pass reports a policy under another name, and a key besides the sealed one, as a note on the card. Turning the agent off deletes the application and every key on it, since the application is the engine's and is going.

### The key is the engine's, and named for the seat

The key's secret is sealed under one name the engine derives for the seat, `SCALEWAY_SECRET_KEY_<HANDLE>`, and its access key under `SCALEWAY_ACCESS_KEY_<HANDLE>` (for example `SCALEWAY_SECRET_KEY_SRE` and `SCALEWAY_ACCESS_KEY_SRE`). The company document never names them in a way that could point a seat's key at another variable or at the provisioner's. Scaleway shows a secret once, so a key that was minted and **could not be sealed is deleted again** at once rather than left live and held by nobody.

### Expiry and rotation

Every agent key expires **90 days** after it is minted. When fewer than **30 days** remain, the next pass mints a new key, seals it and deletes the old one: Scaleway lets no key's expiry change after it is created. An agent's key therefore never lapses while the integration is connected, and a leaked value is useful for at most 90 days.

**Rotate credential** on the Agents tab does the same on demand, for a key you believe has leaked.

An Organization whose security settings cap an API key's lifetime below 90 days refuses the agents' keys. The seat reports it, naming the cap and linking the Organization's security settings, and nothing is minted shorter. Raise the cap to at least 90 days.

### The provisioner key's own expiry

Nothing rotates the provisioner key: a person created it, and only a person can replace it. Each pass reads its expiry, and inside **14 days** of it reports a `credential_expiring` finding naming `integrations.scaleway.provisioning.secret_key` and the date. The integration stays connected, since the key works today. On the day it lapses every pass is refused, and no agent's application, policy or key can be made, changed or rotated. Keys already minted keep working until their own expiry.

### The agent's Scaleway tools

A key is half of an agent's access; the other half is the tool it uses Scaleway through. Either route below counts: the Agents tab reports the tool live when the box carries the seat's key or a per-seat MCP server reads it.

**The scw CLI in its box, as itself.** The CLI reads its key from `SCW_SECRET_KEY` and `SCW_ACCESS_KEY`. While an agent is on at Scaleway, Crewlet sets both in its box to the agent's own sealed key, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)); an agent turned off is given neither from its next run. What the box needs from you is the CLI itself:

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

The engine puts the settings beside the key in the box too, beneath every line you write: `SCW_DEFAULT_ORGANIZATION_ID` (the connection's Organization), `SCW_DEFAULT_PROJECT_ID` (the seat's first project by ID) and `SCW_DISABLE_CHECK_VERSION=true`. A line of your own under any of those names wins, and region and zone stay at the CLI's defaults (`fr-par`, `fr-par-1`) unless you set `SCW_DEFAULT_REGION` or `SCW_DEFAULT_ZONE`. The key is the other way round: a line of your own under `SCW_SECRET_KEY` or `SCW_ACCESS_KEY` is ignored for the agent's own key, and reported as an advisory at that line. While no key is sealed yet, a run starts without it and its start says it is waiting for the Scaleway integration (`sandbox_env_unresolved`, at info).

The step downloads the one scw release Crewlet pins and checks it against that release's published checksum before installing it, the pattern [recipe 1](../concepts/code-sandbox.md#bring-your-own-image) uses for `gh`. An image you build can bake the same step in.

**The scw CLI's own MCP server.** `scw mcp server serve` serves each CLI command as a tool, over stdio. Declare it once, as a per-seat server, with no credential of its own:

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

The engine adds each seat's own key to that seat's instance as `SCW_SECRET_KEY` and `SCW_ACCESS_KEY`, so no seat block names it. The `namespaces=` filter serves the products the levels reach and nothing else: a command for any other product would be refused by the agent's policy anyway. The server has no project of its own, so an agent names the project in a command that needs one. It runs on the engine host, so scw has to be installed on every engine host that runs agent seats, at the same release. On an Apple silicon Mac:

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

Each pass reads every application this engine manages, including the application of a seat that has since left the document.

- **A policy's rules changed at Scaleway** by somebody else are restored to the seat's level and projects and recorded as a `drift_restored` event, with the before and after.
- **A policy somebody deleted** at Scaleway is read as a person turning the agent off there, in case it was incident response, and no pass writes it back. The tab offers **Turn back on** or **Keep off**.
- **An application deleted at Scaleway** is dropped from the engine's records, and the same pass makes a fresh one if the seat is on.
- **A key deleted at Scaleway**, or one about to expire, is replaced in the same pass.
- **A seat that leaves the document** is treated as off: its keys, policy and application are deleted.

A refusal names the step that failed, Scaleway's own reason, what to do about it and who does it, with a link to the console page that settles it. The step is retried on every pass and on **Retry**.

### Disconnecting

Disconnecting deletes every application this engine made, with its policy and keys, and clears their sealed values. That includes the [Atlas application](#see-more-in-atlas), where you turned it on. The provisioner application and its key are yours, so they stay at Scaleway and in the secret store until you delete them.

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
| `provisioning` | yes | The Organization and the key the engine makes each agent's application with. Required when enabled: the integration receives nothing from Scaleway, so an enabled block without it would do nothing at all. |
| `provisioning.organization_id` | yes | The Organization the agents' applications are created in: a UUID, or a `${VAR}`. |
| `provisioning.access_key` | yes | The provisioner key's access key, `SCW` followed by 17 capital letters or digits, or a `${VAR}`. It names the key and grants nothing alone. |
| `provisioning.secret_key` | yes | The [provisioner key's](#creating-the-provisioner-key) secret. Keep it a `${VAR}` reference into the secret store; the dashboard seals it as `SCALEWAY_SECRET_KEY`. |
| `atlas` | no | [See more in Atlas](#see-more-in-atlas): the engine makes a read-only IAM application named for Atlas and reads what the projects hold under it. Off by default; off deletes the application, its policy and its key. |

## What the atlas reads

The engine reads the Organization's projects for the company's [atlas](../concepts/atlas.md#what-integrations-read), once an hour unless the company's `atlas.refresh_interval_seconds` says otherwise. It reads under the provisioner key, with the `ProjectReadOnly` permission set its policy already holds. Nothing at Scaleway is changed.

| Part | Calls | Permission set | What the atlas gains |
|---|---|---|---|
| Projects | `GET /account/v3/projects`, 100 a page | `ProjectReadOnly` on the Organization | the Organization, and each project as an account, with its id and its name |

**The default project is the Organization.** Scaleway gives the default project the Organization's own id, so the atlas shows the two as the one account the settings already name.

**Nothing inside a project is read under the provisioner key.** Its policy holds project read and IAM management and no product's permission set. No description, no creator and no key is read from a project either. What a project holds is read only where you turn on [See more in Atlas](#see-more-in-atlas).

**A refusal is a finding, never a failed read.** Where Scaleway refuses the listing with a `403` (the provisioner's policy lost `ProjectReadOnly`), the source's standing on the atlas says what to grant, and the projects an earlier read recorded stay as they were. A key Scaleway does not know fails the read as a rejected credential.

**Cost and limits.** A read costs one call per hundred projects. A `429` is waited out as every call of this integration does; a longer wait, or a listing that fails part way, stops the read, which records nothing and is made again five minutes later. A project that two complete reads at least a day apart both miss is retired from the atlas.

### See more in Atlas

The atlas can also show what your projects hold: servers, Kubernetes clusters, databases, load balancers, buckets, DNS zones, and Serverless Containers and Functions. It is optional, and nothing else waits on it.

**To turn it on**, open Company › Integrations › Scaleway, switch on **See more in Atlas** and save. That is the whole step: there is nothing to create at Scaleway, because the provisioner key you already gave Crewlet makes what is needed. In the company document the same switch is `integrations.scaleway.atlas: true`.

Within one check of the connection (ten minutes by default) the engine makes, in your Organization:

- one IAM application named `crewlet-atlas-<company>`, described as Crewlet Atlas, so it shows as Atlas, never as an agent, in your IAM pages and your audit trail;
- one policy on it, with one rule granting the nine read-only permission sets in the table below on every project of the Organization, and nothing else;
- one API key for it, expiring after 90 days and replaced by the engine once fewer than 30 remain. The key is sealed as `SCALEWAY_ATLAS_SECRET_KEY`, beside `SCALEWAY_ATLAS_ACCESS_KEY` and `SCALEWAY_ATLAS_APPLICATION_ID`. The engine hands it to no agent, and the company document has no field for it.

**To undo it**, switch **See more in Atlas** off and save. The next check deletes the key, the policy and the application, and clears the three sealed values. What the atlas recorded from them is retired after two complete reads a day apart, like anything else it stops seeing. You can also revoke it at Scaleway alone: delete the `crewlet-atlas-<company>` application on your [IAM applications page](https://console.scaleway.com/iam/applications). The engine does not make it again until you switch the setting off and on, and the Scaleway card says so. [Disconnecting](#disconnecting) Scaleway deletes it too.

Each product is one part of the read, behind its own permission set, and every call is a `GET`:

| Part | Calls | Permission set | What the atlas gains |
|---|---|---|---|
| Servers | `GET /instance/v1/zones/{zone}/servers`, per zone | `InstancesReadOnly` | each Instance as a host: its name, its zone and its public and private addresses |
| Kubernetes clusters | `GET /k8s/v1/regions/{region}/clusters`, per region | `KubernetesReadOnly` | each cluster as a host: its name and its region |
| Load balancers | `GET /lb/v1/zones/{zone}/lbs`, per zone | `LoadBalancersReadOnly` | each Load Balancer as a host: its name, its zone and its addresses |
| Databases | `GET /rdb/v1/regions/{region}/instances`, per region | `RelationalDatabasesReadOnly` | each Managed Database as a datastore: its name, its region, its engine and its version |
| Redis clusters | `GET /redis/v1/zones/{zone}/clusters`, per zone | `RedisReadOnly` | each cluster as a datastore: its name, its zone and its version |
| Buckets | Object Storage's `ListBuckets`, per project and region | `ObjectStorageBucketsRead` | each bucket, by its name and its region |
| DNS zones | `GET /domain/v2beta1/dns-zones`, then each zone's `/records` | `DomainsDNSReadOnly` | each zone as a domain, and each name an A, AAAA or CNAME record gives, with what it points at |
| Serverless Containers | `GET /containers/v1beta1/regions/{region}/namespaces` and `/containers`, per region | `ContainersReadOnly` | each container, by its name |
| Serverless Functions | `GET /functions/v1beta1/regions/{region}/namespaces` and `/functions`, per region | `FunctionsReadOnly` | each function, by its name |

Each entry belongs to its project's account, and each project is said to use what was found in it (compute, a database, object storage, DNS). A name whose record points at the address of a server or a load balancer the same read holds resolves to that entry.

**What is never read.** The engine asks for no server's user data, no cluster's kubeconfig, no database's users, backups or logs, and no object or bucket policy. From the listings it does make, it never decodes a container's, a function's or a namespace's environment variables or secret environment variables, a server's encrypted administrator password, a database's or a cache's endpoints, users and settings, a TXT, MX or any other record that is not A, AAAA or CNAME, or any tag, description or comment. Every value it does keep is held to the atlas's grammars, which refuse anything shaped like a credential.

**What the application could reach, so you can judge it.** Scaleway grants by permission set, and each read-only set covers every read of its product, more than the listings above. The key the engine holds could therefore also read a server's user data (`InstancesReadOnly`), the plain environment variables of a container or a function (`ContainersReadOnly`, `FunctionsReadOnly`; Scaleway returns secret ones only as hashes), and, through each cluster's own API server, the objects the cluster's `scaleway:cluster-read` role allows, which by default includes config maps and pod logs and excludes Secrets (`KubernetesReadOnly`). The engine calls none of those. If that reach is more than you want one sealed key to hold, leave the switch off: the atlas then shows your projects and nothing inside them.

**A refusal is a finding, never a failed read.** A product Scaleway refuses the Atlas application's key (somebody changed its policy, or the Organization cannot use the product) is left unread: the source's standing on the atlas names the part and the permission set, what an earlier read of that part recorded stays, and every other product is still read. A permission set Scaleway does not list for your Organization is left out of the policy, and the card says which. A key Scaleway no longer accepts leaves every product unread until the next check replaces it; the projects are still read under the provisioner key.

**Cost and limits.** A read costs about 40 calls, plus three per project for the buckets and one per DNS zone, once an hour. The Atlas application counts as one application, one policy and one API key against the Organization's IAM quotas. A locality where Scaleway does not serve a product answers `404`, which is read as nothing there.

## Scaleway's limits worth knowing

These are Scaleway's, not Crewlet's.

| Limit | What it means |
|---|---|
| IAM quotas: by default 100 applications, 100 API keys and 50 policies per Organization | One application, one policy and one key per agent that is on, plus the provisioner's and every one people made by hand. A full quota refuses the next agent, naming the [quotas page](https://console.scaleway.com/organization/quotas); ask Scaleway support to raise it. The narrow provisioner cannot read the quotas, so the Test button does not report them |
| The Organization's maximum API key lifetime | Set in the Organization's security settings. Below 90 days, no agent key can be minted |
| Rate limits | A `429` carries `Retry-After`. The engine's calls wait up to 30 seconds, up to 3 times; a longer wait is left to the next pass |
| Propagation | Scaleway documents up to a minute for a policy change to take effect. Measured, it took effect at once, and a deleted key stopped working within seconds |

## What is not supported yet

- **Inbound events.** Nothing from Scaleway can wake an agent yet: there is no webhook route for its alerts.
- **A CLI command.** The integration is set up from the dashboard or the company document; there is no `crewlet scaleway` command.
- **Secrets, keys and object contents.** No level reaches Secret Manager, Key Manager or the contents of a bucket, and none writes DNS; see [Each agent's access is a level](#each-agents-access-is-a-level).

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The `provisioning` block is missing, or the Organization ID, the access key or the secret key resolved to nothing | Add the block, seal the provisioner secret as `SCALEWAY_SECRET_KEY`, or correct the field it names |
| `credential_rejected` | Scaleway refused the provisioner key, or it is a user's key or another Organization's | Create a new key for the provisioner application and replace it with **Replace key** |
| `credential_expiring` | The provisioner key expires within 14 days | Create a new key for the provisioner application and replace it before the date the finding names |
| `grant_short` | The provisioner application's policies do not grant IAMApplicationManager, IAMPolicyManager or ProjectReadOnly on the Organization, or the rule granting IAMApplicationManager carries a condition on the resource, which refuses the agents' keys | Add the set the finding names to a rule at Organization scope, or remove the condition. A condition on `request.ip` alone is the IP pin, and is fine |
| `grant_excess` | A note, not a fault: the provisioner holds a set beyond the three it needs, or an agent's application or the Atlas application carries a policy Crewlet did not write, or a key besides its sealed one | Remove what nobody needs at Scaleway. Crewlet never deletes a policy or a key it did not make while the agent is on |
| `unknown_tier` | A permission set the level an agent is on names is not offered to the Organization (subject `scaleway level read`, `scaleway level operate` or `scaleway level administer`) | Check the Organization can use that product, or move the agents at that level to another one |
| `level_unavailable` | A note, not a fault: a level no agent is on names a set the Organization does not offer. The Agents tab offers that level disabled with this reason | Nothing, unless you want the level |
| `coverage_partial` | A note, not a fault, about [See more in Atlas](#see-more-in-atlas) (subject `integrations.scaleway.atlas`): the Atlas application is not made yet, was deleted at Scaleway, has no key because the Organization caps a key's lifetime below 90 days, lacks a permission set Scaleway does not list, or could not be brought up to date on this check | Usually nothing: the next check finishes it. For a deleted application, switch **See more in Atlas** off and on. For a capped lifetime, raise the cap in the Organization's security settings |
| `registration_orphaned` | Scaleway holds applications named like this company's agents (`crewlet-<company>-...`), or under the Atlas application's name (`crewlet-atlas-<company>`), that the engine has no record of | Delete the ones nobody uses at Scaleway. Crewlet never deletes an application it did not make |
| `identity_failed` | An agent's application, policy or key could not be made, changed or rotated; a project it names is not in the Organization; somebody deleted its policy at Scaleway; or Scaleway would not read it back. Scaleway's reason is in the detail | Fix what the detail names, then **Retry** on the agent's row. For a policy deleted at Scaleway, choose **Turn back on** or **Keep off** |
| `identity_missing` | For up to about a minute after you first connect, "access migration has not finished": the one-time step every per-agent integration passes through | Nothing; it clears by itself |

An agent's own failures are also reported on that agent's row on the **Agents** tab, with the control that answers them.
