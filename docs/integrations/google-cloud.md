# Google Cloud

Google Cloud gives each agent its **own service account**, with the level you choose on the projects you choose, and **Google's managed MCP servers** to use it through. An agent reads VMs, networks, GKE, Cloud Run, Cloud SQL, Cloud DNS, buckets, metrics and logs as itself. Where you allow it, it restarts VMs and pods, scales workloads and runs jobs, or, at the opt-in top level, changes infrastructure.

Crewlet creates, grants, disables and deletes those accounts itself, through one provisioner service account you create for it. **No service account key is ever made or downloaded:**

- Crewlet signs in as the provisioner with Workload Identity Federation, using a key of its own whose public half you hand to Google.
- It issues each agent's token for an hour at a time, and never stores it.

Nothing here receives events from Google Cloud yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `viewer` (the default) | Read the project and its allow policy, VMs, disks, networks, firewall rules and load balancers, GKE objects except Secrets, Cloud Run services and jobs, Cloud SQL instances, Cloud DNS zones and records, bucket metadata, metrics and logs, and call Google's MCP tools | Change anything, or read an object in a bucket, a Secret, an API key or billing |
| `operator` | Everything `viewer` can, plus start, stop, reset, suspend and resume VMs, resize managed instance groups, delete and evict pods, scale deployments, stateful sets and replica sets, restart Cloud SQL instances, run and cancel Cloud Run jobs as they are defined, edit alerting, dashboards and uptime checks, and read and write objects in buckets | Change a VM's metadata, a pod's spec, a database's users, passwords or certificates, or any allow policy |
| `administer` (opt-in) | Everything `operator` can, plus create, change and delete VMs that run as no service account, disks, images, networks, routes, load balancers, firewall rules and certificates; manage GKE clusters and node pools; manage Cloud SQL fully; manage DNS zones and records; and manage buckets and their access | Act as another service account, grant anything on the project, read a Secret or an API key, or touch billing |

**Administer can create access that outlives the agent's token.** It can add an SSH key or a startup script to a VM, create a database user or a client certificate, or grant another principal access to a bucket or a DNS zone. Each such grant stays after the agent is turned off. Give it only to an agent you trust with that, and review its Cloud Audit Logs.

Raising an agent to `operator` or `administer` asks for a fresh sign-in (step-up). `administer` is never the default.

## Setting it up from the dashboard

Open **Company › Integrations › Google Cloud**. Setting it up takes four steps.

### Before you start: the identity project

1. **Create an empty project** for Crewlet's accounts, on the console's [new project page](https://console.cloud.google.com/projectcreate). Put it in the same organization or folder as the projects agents will work on.
2. **Leave it empty.** It holds the provisioner and one service account per agent, and nothing else.
   - **Never enable Compute Engine or another product there.** The provisioner holds Service Account Admin in this project, so any other service account in it is one the provisioner could act as.
   - The card warns about any account that is neither the provisioner nor Crewlet's.

### Connecting Crewlet

1. **Enter the provisioner service account** as `crewlet-provisioner@<identity project>.iam.gserviceaccount.com`, and press **Save**.
   - This makes Crewlet's federation key. It is an ES256 key, sealed as `GOOGLE_CLOUD_FEDERATION_KEY`, and its private half never leaves the secret store.
   - Then the form shows a **Cloud Shell block**.
2. **Open Cloud Shell** in the identity project (the terminal icon at the top of the console), paste the block whole, and run it. It:
   - enables the IAM, IAM Service Account Credentials, Security Token Service, Cloud Resource Manager and Service Usage APIs;
   - creates the provisioner if it is missing, and grants it Service Account Admin and Service Usage Viewer on the identity project;
   - creates the workload identity pool and provider holding Crewlet's public key, written inline so nothing is uploaded, or updates the provider's key if it exists;
   - lets Crewlet's subject act as the provisioner (Workload Identity User);
   - prints the provider's full name as its last line.
3. **Paste that last line** into **Workload identity provider**, press **Save**, then **Test**.

The block pins the provider to this company's subject (`crewlet:<company>`), so no other identity can use it. A provider holds at most 8 keys; when Crewlet's key changes, the block updates the provider with `update-oidc`.

### Prepare a project for Crewlet

Each project an agent may use is prepared once, by somebody who can change its allow policy (an Owner). Paste this into Cloud Shell, with `IDENTITY` set to your identity project and `TARGET` to the project:

```sh
IDENTITY=example-crewlet-id
TARGET=example-prod
PROV=crewlet-provisioner@${IDENTITY}.iam.gserviceaccount.com
export CLOUDSDK_CORE_DISABLE_PROMPTS=1

gcloud services enable cloudresourcemanager.googleapis.com --project="$TARGET"

PERMS=cloudsql.instances.restart,compute.instanceGroupManagers.update,compute.instances.reset,compute.instances.resume,compute.instances.start,compute.instances.stop,compute.instances.suspend,container.deployments.updateScale,container.pods.delete,container.pods.evict,container.replicaSets.updateScale,container.statefulSets.updateScale
if gcloud iam roles describe crewletOperator --project="$TARGET" >/dev/null 2>&1; then
  gcloud iam roles update crewletOperator --project="$TARGET" --permissions="$PERMS"
else
  gcloud iam roles create crewletOperator --project="$TARGET" --title="Crewlet Operator" \
    --description="Restart and scale only, for Crewlet agents at the Operator level. Do not add permissions." \
    --stage=GA --permissions="$PERMS"
fi

cat > crewlet-viewer.yaml <<'YAML'
title: Crewlet Viewer roles only
description: Crewlet may grant or revoke only the Viewer level's roles.
expression: "api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(['roles/browser', 'roles/cloudsql.viewer', 'roles/compute.viewer', 'roles/container.viewer', 'roles/dns.reader', 'roles/logging.viewer', 'roles/mcp.toolUser', 'roles/monitoring.viewer', 'roles/run.viewer', 'roles/storage.bucketViewer'])"
YAML
cat > crewlet-operator.yaml <<YAML
title: Crewlet Operator roles only
description: Crewlet may grant or revoke only the Operator level's additional roles.
expression: "api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(['roles/monitoring.editor', 'roles/run.jobsExecutor', 'roles/storage.objectUser', 'projects/${TARGET}/roles/crewletOperator'])"
YAML
cat > crewlet-administer.yaml <<'YAML'
title: Crewlet Administer roles only
description: Crewlet may grant or revoke only the Administer level's additional roles.
expression: "api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly(['roles/cloudsql.admin', 'roles/compute.instanceAdmin.v1', 'roles/compute.networkAdmin', 'roles/compute.securityAdmin', 'roles/container.clusterAdmin', 'roles/dns.admin', 'roles/storage.admin'])"
YAML

for C in viewer operator administer; do
  gcloud projects add-iam-policy-binding "$TARGET" --member="serviceAccount:$PROV" \
    --role=roles/resourcemanager.projectIamAdmin --condition-from-file=crewlet-$C.yaml --format=none
done
for ROLE in roles/serviceusage.serviceUsageViewer roles/iam.roleViewer; do
  gcloud projects add-iam-policy-binding "$TARGET" --member="serviceAccount:$PROV" \
    --role="$ROLE" --condition=None --format=none
done
```

**What this does:**
- It creates `crewletOperator`, a custom role with exactly the 12 restart and scale permissions above. Google has no predefined Compute or GKE operator role that holds no credential-making permission; see [Roles no level grants](#roles-no-level-grants-and-why).
- It gives the provisioner **Project IAM Admin three times**, each under a condition listing one level's roles. The provisioner can then grant or revoke those roles and nothing else. Google limits one such list to 10 roles, so each level has its own.
- It adds **Service Usage Viewer** (to check the project's APIs) and **Role Viewer** (to check `crewletOperator`).

**Afterwards:**
- On the project's IAM page, the provisioner should show Project IAM Admin three times, each with its "Crewlet ... roles only" condition, plus Service Usage Viewer and Role Viewer, and nothing else.
- Enable the APIs of the products your agents use on the project. Each product's MCP server is enabled with its API.

**Crewlet finds your prepared projects itself.** It offers a project once all three conditions are there exactly and `crewletOperator` holds exactly its 12 permissions. A project that is half prepared is named on the card with what it is missing.

### Testing the connection

**Test** checks with reads only. Each failure says what to change:
- Google's Security Token Service accepts Crewlet's federation token;
- Crewlet can act as the provisioner;
- the identity project's APIs are on;
- each prepared project carries the three conditions and an exact `crewletOperator`;
- the identity project holds no account but the provisioner's and Crewlet's;
- the identity project's service account count is under its quota.

## Giving each agent its own Google Cloud account

On the integration's **Agents** tab, turn an agent on, choose its level and its projects, and review.

- **An agent that is on must have at least one project.** An empty list means no project, never every project. Otherwise adding a project to the company would silently widen every agent.
- **The tab offers the projects you prepared,** by name with the project ID beside it.

### Each agent's access is a level

Each level is a fixed set of Google's roles, granted on each of the agent's projects. The project's IAM page shows them under the agent's account:

| Level | Roles, as the IAM page names them |
|---|---|
| `viewer` | Browser, Cloud SQL Viewer, Compute Viewer, Kubernetes Engine Viewer, DNS Reader, Logs Viewer, MCP Tool User, Monitoring Viewer, Cloud Run Viewer, Storage Bucket Viewer |
| `operator` | Viewer's roles, plus Monitoring Editor, Cloud Run Jobs Executor, Storage Object User and Crewlet Operator |
| `administer` | Operator's roles, plus Cloud SQL Admin, Compute Instance Admin (v1), Compute Network Admin, Compute Security Admin, Kubernetes Engine Cluster Admin, DNS Administrator and Storage Admin |

**Caveats:**
- **Cloud Run Viewer reads a service's configuration, environment variables included.** A secret written as a plain environment variable is readable; reference Secret Manager instead.
- **Storage Object User and Storage Admin read every object in the project's buckets.** Keep Terraform state and exported keys out of the projects agents work on.
- **Storage Bucket Viewer is a Beta role at Google.**

### What each change does at Google Cloud

| Change | At Google Cloud |
|---|---|
| Turn on | Creates the agent's service account in the identity project, lets the provisioner make tokens for that account alone, then grants the level's roles on each project |
| Change the level | Adds or removes the roles between the two levels on each project. The account and its unique ID stay the same |
| Change the projects | Grants the level's roles on an added project, and removes them from a dropped one |
| Turn off | Disables the account first, so it stops everywhere at once, then removes its roles from every project. The account and its audit history stay |
| Turn back on | Enables the account and grants its roles again. An account somebody else disabled at Google is never turned back on without being asked |
| Remove (only when off) | Deletes the account. Cloud Audit Logs keep its entries |

### The account is the engine's, and named for the seat

**The account's name:**
- Its ID is `crewlet-` plus the agent's handle (cut to 17 characters) plus 4 characters that tell two handles apart. Google limits an ID to 30 characters.
- Its display name is `Crewlet <company> <handle>`.

**The engine acts only on accounts it recorded when it made them,** by Google's unique ID, and adopts nothing by name. An account named like Crewlet's that it has no record of is reported, and never deleted.

### Tokens are issued for an hour, never stored

- An agent's credential is `GOOGLE_CLOUD_ACCESS_TOKEN_<HANDLE>`: a token Crewlet makes for the agent's account by impersonation when a tool needs it, good for one hour.
- It is held in the engine's memory and renewed before it expires. Nothing is sealed, so there is nothing to rotate.
- An agent that is off is issued nothing, and disabling its account stops a token already issued at once.
- Until the agent's account is made and Google has applied Crewlet's grant on it, a tool that names the token waits, and tries again on its own. A refusal after that names what to fix, on the connection or the agent's row of the Agents tab.

### The agent's Google Cloud tools

**Google's managed MCP servers** are the agent's tools. Add them from **Company › Tools › MCP servers**, one instance per agent:

- Cloud Monitoring;
- Cloud Logging;
- Compute Engine;
- Google Kubernetes Engine;
- Cloud Run;
- Cloud SQL;
- Cloud Storage;
- Resource Manager.

Each reads the agent's own token as a bearer `Authorization` header. The Agents tab shows the tool as live once one of them is declared for the agent.

**The Google Cloud CLI works in the agent's box too.** Crewlet writes the token to `.config/crewlet/gcloud-token` under the home of every Google Cloud agent's box, keeps it current while the run lasts, and sets `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` to its path in the box and `CLOUDSDK_CORE_PROJECT` to the agent's first project. A setting of the same name in the agent's own sandbox environment wins. The Agents tab lists these as provided by Crewlet. Install the Google Cloud CLI in your box image to use it: Crewlet never downloads binaries, and the tool reads live only for a Google Cloud MCP server, since Crewlet cannot see whether the CLI is installed. A token the run prints, `gcloud auth print-access-token` or the file's own, is kept in its session redacted, as every credential shape Crewlet knows is ([Code Sandbox](../concepts/code-sandbox.md#each-launchs-session-kept)). The run's live output shows the commands it runs and a failed call's first line, never a command's output, redacted by the same shapes ([Security model](../concepts/code-sandbox.md#security-model)).

### Agents in GKE clusters

**No level lets an agent write Kubernetes objects inside a cluster beyond deleting pods and scaling.** This is a deliberate limit:
- Kubernetes Engine Developer and Admin read every Secret.
- They make Kubernetes service account tokens.
- They let a workload run as any Kubernetes service account. Through Workload Identity, that means any Google service account bound to one: acting as another identity.

**Administer manages clusters and node pools** (Kubernetes Engine Cluster Admin), never the objects in them.

To let one agent do more inside a cluster, bind its account email to a Kubernetes Role yourself, for example:

```sh
kubectl create rolebinding sre-agent-edit --role=deployer --namespace=app \
  --user=crewlet-sre-a1b2@example-crewlet-id.iam.gserviceaccount.com
```

### Google applies changes within minutes

**Google's IAM is eventually consistent:**
- A new account can take a moment before a project accepts a grant to it.
- A grant can take minutes before it applies.
- A change can read back stale for a while.

**Crewlet waits these out, never reporting them as a failure.** The sync ends as done once Crewlet's writes are made, and the agent's tool on the Agents tab says what it waits on:
- "Waiting: Google is still applying the grant", for up to 10 minutes after Crewlet grants the agent access (turning it on, raising its level, adding a project), since its first token can take a minute or two;
- "Waiting: Google is still applying the change", for up to 3 minutes after any other change (lowering its level, dropping a project).

Each ends by itself. A sync that still cannot see its own write after 3 minutes says so, and the next pass tries again: for a new account, "Google is still creating the agent's account".

### Disconnecting

**Disconnecting turns every agent off:** it disables each account this engine made and removes its roles. When you also choose to remove the accounts, it deletes them.

The identity project, the provisioner, the pool and the provider are yours, so they stay until you delete them. Remove the provider's key if you will not reconnect.

## Configuration

```yaml
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@example-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
roles:
  - handle: sre
    integrations:
      google_cloud:
        enabled: true
        level: operator          # viewer (the default), operator or administer
        projects: [example-prod]
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning.provisioner` | yes | The provisioner's email, `<id>@<identity project>.iam.gserviceaccount.com`. The identity project is read from it. |
| `provisioning.provider` | once the key is made | The provider's full resource name, as the Cloud Shell block prints it. |
| `provisioning.federation_key` | yes | Crewlet's ES256 key, made by the dashboard and sealed as `GOOGLE_CLOUD_FEDERATION_KEY`. Keep it a `${VAR}` reference. |
| `roles[].integrations.google_cloud.enabled` | yes | Whether the agent has its own account. Off disables it and removes its roles. |
| `roles[].integrations.google_cloud.level` | no | `viewer` (the default), `operator` or `administer`. |
| `roles[].integrations.google_cloud.projects` | when enabled | The project IDs the agent acts on, each one prepared for Crewlet. At least one. |

There is no company-wide project list: Crewlet reads the projects you prepared from the provisioner's own grants.

## Roles no level grants, and why

No level ever grants a role that mints or reveals a credential outliving the agent's own access, acts as another identity, grants on the project, or reads billing:

| Role | Why not |
|---|---|
| Owner, Editor | Basic roles with thousands of permissions, acting as other service accounts among them |
| Viewer (basic) | Reads every API key's string |
| Project IAM Admin, Security Admin | Grants anything on the project |
| Service Account User, Service Account Token Creator, Service Account Key Admin | Acts as, or makes credentials for, other service accounts |
| Secret Manager Secret Accessor | Reads secret payloads |
| Kubernetes Engine Developer, Kubernetes Engine Admin | Reads Secrets, mints Kubernetes tokens, and runs workloads as other identities (see [Agents in GKE clusters](#agents-in-gke-clusters)) |
| Cloud Run Developer, Cloud Run Admin | Runs a job with overridden arguments as the job's own identity |
| Cloud SQL Editor (below Administer) | Creates client certificates |
| Storage HMAC Key Admin | Makes an HMAC key for any service account |
| Any billing role | No level touches billing |

Each prepared project's conditions refuse these to the provisioner too, so it cannot grant one even if asked.

## What the atlas reads

The engine reads the company's projects for its [atlas](../concepts/atlas.md#what-integrations-read), once an hour unless the company's `atlas.refresh_interval_seconds` says otherwise. It reads as the provisioner, through the same federation, and needs no role beyond the ones the two setup blocks grant. Every call it adds is a `GET`, and nothing at Google Cloud is changed.

| Part | Calls | Role | What the atlas gains |
|---|---|---|---|
| Projects | `GET /v3/projects:search`, 100 a page | whatever lets the provisioner see the project | each active project as an account, with its id and its display name. The identity project is the account the settings already name |
| Services | `GET /v1/projects/{id}/services` for the enabled ones, once per project | Service Usage Viewer on the project, which the [Prepare a project](#prepare-a-project-for-crewlet) block grants | what the project is used for |

**What an enabled API says.** Each enabled API is read as one of the atlas's capabilities, and its own name is never recorded:

| Capability | Enabled API |
|---|---|
| `compute` | Compute Engine, Cloud Run, Kubernetes Engine, Cloud Functions, App Engine, Batch |
| `database` | Cloud SQL, Spanner, Firestore, Memorystore for Redis and for Memcached, AlloyDB, Bigtable |
| `dns` | Cloud DNS |
| `model_api` | Vertex AI, the Gemini API |
| `ci` | Cloud Build, Cloud Deploy |
| `code_hosting` | Cloud Source Repositories, Secure Source Manager |
| `identity` | Identity Platform, Identity-Aware Proxy |

An API that is not in the table says nothing. That includes every API a new project has enabled before anybody uses it (Cloud Storage, BigQuery, Datastore, Cloud Logging and Cloud Monitoring): being enabled is no sign that the project uses them.

**A project that is being deleted is not read.** Google keeps a deleted project for thirty days in a state other than active. The atlas leaves it out from the first read after the deletion and retires it a day later.

**A refusal is a finding, never a failed read.** Where Google refuses a project's services (the provisioner lacks Service Usage Viewer there, as on a project the Prepare a project block never ran on), the project is still read as an account, the source's standing on the atlas counts the projects and names the block, and what an earlier read said those projects are used for stays as it was. A provisioner Google refuses the listing of projects itself, or does not trust at federation or impersonation, fails the read as a rejected credential.

**What is never read.** A project's labels and tags, its IAM policy, its organization or folder, and everything inside it: no instance, database, cluster, bucket or address. Reading those takes an identity made for the atlas, which this integration does not create.

**Cost and limits.** A read costs one call per hundred projects and one per active project, beside the two that mint the provisioner's token once an hour. When Google answers a read with its rate limit, the call waits as long as Google asks (five seconds where it names no wait), up to three times and thirty seconds a wait; past that, or when any listing fails part way, the read stops and records nothing, and the engine reads again five minutes later. A provisioner that can read more than 1,000 projects is not read: the standing says so, and nothing is retired. A project that two complete reads at least a day apart both miss is retired from the atlas.

## Google Cloud's limits worth knowing

These are Google's, not Crewlet's.

| Limit | Value | What it means |
|---|---|---|
| Service accounts per project | A per-project quota ("Service Account Count", commonly 100) | The identity project holds the provisioner and one account per agent that is on or off but not removed |
| Service account ID | 6 to 30 characters | Crewlet's names fit; see [The account is the engine's](#the-account-is-the-engines-and-named-for-the-seat) |
| Roles in one grant condition | 10 | Why each level has its own condition |
| Keys on a workload identity provider | 8 | Reconnecting updates the key in place |
| Token lifetime | 1 hour | Crewlet renews before expiry |
| Rate limits | A `429`, with `Retry-After` on some limits | A read waits as long as Google asks, or 5 seconds where it names no wait, up to 30 seconds and up to 3 times; a longer wait is left to the next pass. A write is never sent again: Google answers a spent quota with the same status |

## What is not supported yet

- **Inbound alerts.** Cloud Monitoring alerts cannot wake an agent yet.
- **Google Analytics and Search Console.** They will be optional lanes on the same agent account. Neither can be granted to an agent with the containment the rest of this integration keeps.
- **Deploys.** Deploying a Cloud Run revision, or creating a VM or node pool that runs as a service account, needs Service Account User on that account, which Crewlet never grants. Grant it yourself on one runtime account if an agent should deploy as it.
- **Billing.** No level reads costs or budgets.
- **An engine running on Google Cloud** still uses federation with Crewlet's key, not the attached service account.

## Troubleshooting

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The provisioner is unset, the federation key did not resolve, or the provider is not pasted yet | Save the provisioner, run the Cloud Shell block, and paste the provider name it prints |
| `credential_rejected` | Google's Security Token Service refused Crewlet's token, or Crewlet cannot act as the provisioner | Run the Cloud Shell block again: it restores the provider's key and the Workload Identity User binding |
| `level_unavailable` | A project cannot grant a level an agent uses: a condition or `crewletOperator` is missing or differs | Run [Prepare a project for Crewlet](#prepare-a-project-for-crewlet) again for that project |
| `grant_excess` | The identity project holds service accounts besides the provisioner and Crewlet's | Move them to another project |
| `registration_orphaned` | The identity project holds accounts named for this company's agents that Crewlet has no record of | Delete the ones nobody uses |

**If a grant is refused with a message about permitted customers or domains,** your organization enforces domain restricted sharing (`iam.allowedPolicyMemberDomains`, on by default for organizations created since May 2024), and it refuses the agent's service account. Add your organization's principal set to the constraint's allowed values, or ask your organization administrator to.
