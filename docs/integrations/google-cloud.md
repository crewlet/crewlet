# Google Cloud

Google Cloud gives each agent its **own service account**, at the level you choose on the projects you choose, and **Google's managed MCP servers** to use it through. An agent reads VMs, networks, GKE, Cloud Run, Cloud SQL, Cloud DNS, buckets, metrics and logs as itself; where you allow it, restarts VMs and pods, scales workloads and runs jobs; at the opt-in top level, changes infrastructure.

Crewlet manages those accounts through one provisioner service account you create. **No service account key is ever made or downloaded:** Crewlet signs in as the provisioner with Workload Identity Federation, using a key of its own whose public half you hand to Google, and issues each agent's token for an hour at a time, never storing it.

Nothing here receives events from Google Cloud yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `viewer` (the default) | Read the project and its allow policy, VMs, disks, networks, firewall rules and load balancers, GKE objects except Secrets, Cloud Run services and jobs, Cloud SQL instances, Cloud DNS zones and records, bucket metadata, metrics and logs, and call Google's MCP tools | Change anything, or read an object in a bucket, a Secret, an API key or billing |
| `operator` | `viewer`'s, plus start, stop, reset, suspend and resume VMs, resize managed instance groups, delete and evict pods, scale deployments, stateful sets and replica sets, restart Cloud SQL instances, run and cancel Cloud Run jobs as defined, edit alerting, dashboards and uptime checks, and read and write bucket objects | Change a VM's metadata, a pod's spec, a database's users, passwords or certificates, or any allow policy |
| `administer` (opt-in) | `operator`'s, plus create, change and delete VMs that run as no service account, disks, images, networks, routes, load balancers, firewall rules and certificates; manage GKE clusters and node pools; manage Cloud SQL fully; manage DNS zones and records; manage buckets and their access | Act as another service account, grant anything on the project, read a Secret or an API key, or touch billing |

**Administer can create access that outlives the agent's token** (an SSH key or startup script on a VM, a database user or client certificate, another principal's access to a bucket or DNS zone), which stays after the agent is turned off. Give it only to an agent you trust with that, and review its Cloud Audit Logs.

Raising an agent to `operator` or `administer` asks for a fresh sign-in (step-up). `administer` is never the default.

## Setting it up from the dashboard

Open **Company › Integrations › Google Cloud**.

### Before you start: the identity project

1. **Create an empty project** on the console's [new project page](https://console.cloud.google.com/projectcreate), in the same organization or folder as the projects agents will work on.
2. **Leave it empty:** only the provisioner and one service account per agent.
   - **Never enable Compute Engine or another product there.** The provisioner holds Service Account Admin here, so any other service account is one it could act as. The card warns about any account that is neither the provisioner nor Crewlet's.

### Connecting Crewlet

1. **Enter the provisioner service account** as `crewlet-provisioner@<identity project>.iam.gserviceaccount.com`, and press **Save**.
   - This makes Crewlet's ES256 federation key, sealed as `GOOGLE_CLOUD_FEDERATION_KEY`; its private half never leaves the secret store. The form then shows a **Cloud Shell block**.
2. **Open Cloud Shell** in the identity project, paste the block whole, and run it. It:
   - enables the IAM, IAM Service Account Credentials, Security Token Service, Cloud Resource Manager and Service Usage APIs;
   - creates the provisioner if missing, and grants it Service Account Admin and Service Usage Viewer on the identity project;
   - creates the workload identity pool and provider holding Crewlet's public key (inline, nothing uploaded), or updates the provider's key if it exists;
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

# Optional: the roles Crewlet may grant an agent here beside its level, at most 10.
GRANTS="'roles/pubsub.editor', 'roles/cloudscheduler.admin'"
if [ -n "$GRANTS" ]; then
  cat > crewlet-grants.yaml <<YAML
title: Crewlet grants list
description: Crewlet may grant or revoke only these roles, one agent at a time, on request.
expression: "api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly([${GRANTS}])"
YAML
  gcloud projects add-iam-policy-binding "$TARGET" --member="serviceAccount:$PROV" \
    --role=roles/resourcemanager.projectIamAdmin --condition-from-file=crewlet-grants.yaml --format=none
fi

# Only where "See more in Atlas" made its account: let it search this project too.
ATLAS=crewlet-atlas@${IDENTITY}.iam.gserviceaccount.com
if gcloud iam service-accounts describe "$ATLAS" --project="$IDENTITY" >/dev/null 2>&1; then
  if gcloud iam roles describe crewletAtlasReader --project="$TARGET" >/dev/null 2>&1; then
    gcloud iam roles update crewletAtlasReader --project="$TARGET" \
      --permissions=cloudasset.assets.searchAllResources --format=none
  else
    gcloud iam roles create crewletAtlasReader --project="$TARGET" --title="Crewlet Atlas reader" \
      --description="Search resources by name and type, for Crewlet's atlas. Do not add permissions." \
      --stage=GA --permissions=cloudasset.assets.searchAllResources --format=none
  fi
  gcloud projects add-iam-policy-binding "$TARGET" --member="serviceAccount:$ATLAS" \
    --role="projects/$TARGET/roles/crewletAtlasReader" --condition=None --format=none
fi
```

**What this does:**
- Creates `crewletOperator`, a custom role with exactly the 12 restart and scale permissions above (Google has no predefined Compute or GKE operator role without credential-making permissions; see [Roles no level grants](#roles-no-level-grants-and-why)).
- Gives the provisioner **Project IAM Admin three times**, each under a condition listing one level's roles, so it can grant or revoke only those. Google limits such a list to 10 roles, hence one per level.
- Adds **Service Usage Viewer** (to check the project's APIs) and **Role Viewer** (to check `crewletOperator` and the grants list's roles).
- Where `GRANTS` names roles, gives Project IAM Admin a fourth time under the **grants list**: roles Crewlet may grant one agent on request ([granting one more role](#granting-one-more-role)). Empty for none.
- Where [See more in Atlas](#see-more-in-atlas-what-each-project-runs) made the `crewlet-atlas` account, lets it search this project by name and type.

**Afterwards:**
- The project's IAM page should show the provisioner with Project IAM Admin three times, each under its "Crewlet ... roles only" condition (four with the "Crewlet grants list"), plus Service Usage Viewer and Role Viewer, and nothing else.
- Enable the APIs of the products your agents use; each product's MCP server is enabled with its API.

**Crewlet finds your prepared projects itself.** It offers a project once all three conditions are there exactly, `crewletOperator` holds exactly its 12 permissions, and every role a grants list names is grantable (none can act as another identity, mint or read a credential or key, grant access, run as a workload's identity or reach billing). A half-prepared project, or one whose grants list names such a role, is named on the card with what to change.

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

- **An agent that is on must have at least one project.** An empty list means no project, never every project, so a new company project never silently widens every agent.
- **The tab offers the projects you prepared,** by name with project ID.

### Each agent's access is a level

Each level is a fixed set of Google's roles, granted on each of the agent's projects. The project's IAM page shows them under the agent's account:

| Level | Roles, as the IAM page names them |
|---|---|
| `viewer` | Browser, Cloud SQL Viewer, Compute Viewer, Kubernetes Engine Viewer, DNS Reader, Logs Viewer, MCP Tool User, Monitoring Viewer, Cloud Run Viewer, Storage Bucket Viewer |
| `operator` | Viewer's roles, plus Monitoring Editor, Cloud Run Jobs Executor, Storage Object User and Crewlet Operator |
| `administer` | Operator's roles, plus Cloud SQL Admin, Compute Instance Admin (v1), Compute Network Admin, Compute Security Admin, Kubernetes Engine Cluster Admin, DNS Administrator and Storage Admin |

**Caveats:**
- **Cloud Run Viewer reads a service's configuration, environment variables included.** Reference Secret Manager instead of a plain environment variable.
- **Storage Object User and Storage Admin read every object in the project's buckets.** Keep Terraform state and exported keys out of the projects agents work on.
- **Storage Bucket Viewer is a Beta role at Google.**

### What each change does at Google Cloud

| Change | At Google Cloud |
|---|---|
| Turn on | Creates the agent's service account in the identity project, lets the provisioner make tokens for that account alone, then grants the level's roles on each project |
| Change the level | Adds or removes the roles between the two levels on each project. The account and its unique ID stay |
| Change the projects | Grants the level's roles on an added project, removes them from a dropped one |
| Grant or remove a role | Binds or unbinds that one role on the grant's projects, one write per role. A level change never removes a role a live grant holds |
| Turn off | Disables the account first, so it stops everywhere at once, then removes its roles from every project. The account and its audit history stay |
| Turn back on | Enables the account and grants its roles again. An account somebody else disabled at Google is never turned back on without being asked |
| Remove (only when off) | Deletes the account. Cloud Audit Logs keep its entries |

### Granting one more role

When an agent needs a role no level holds, give it that role alone, beside its level, instead of a higher level:

- **On the Agents tab:** open the agent's row, choose **Add grant**, pick the role, choose its projects (none is the agent's own projects), and how long: 4 hours, 24 hours, 7 days, or permanent.
- **From the agent's request:** when Google refuses a call with `PERMISSION_DENIED`, the agent asks with `request_access`, naming the role that holds the permission and why. You answer in your Inbox: **Grant for this change** (it lapses after the hours shown), **Add to this agent permanently**, or **Not now**. Either grant shows the exact grant first, and **Grant** approves it: the engine saves it and only then tells the agent yes.

What Crewlet does:

- **The catalogue is your grants lists.** A role can be granted on a project only where that project's grants list names it ([Prepare a project](#prepare-a-project-for-crewlet)), since the provisioner may change only roles its conditions list. Crewlet reads each listed role's title and permissions from Google and offers it where any prepared project lists it. A grant naming a project whose list lacks the role is refused, naming the role and the condition to add it to.
- **A grant is a plain binding** of the role to the agent's account on its projects, one role per write, so exactly one condition allows each write.
- **A grant never outlives its expiry:** a pass a minute before it lapses removes the binding; the level stays.
- **A role holding any permission that is not a read needs a fresh sign-in**, as `operator` and `administer` do.

```yaml
roles:
  - handle: sre
    integrations:
      google_cloud:
        enabled: true
        level: viewer
        projects: [example-prod]
        grants:
          - id: g2h8v4d6s1q
            permission: roles/pubsub.editor
            expires_at: 2026-11-15T18:00:00Z
            granted_by: ada@example.com
            granted_at: 2026-11-15T14:00:00Z
            reason: Create the release topic
```

### The account is the engine's, and named for the seat

**The account's name:**
- Its ID is `crewlet-` plus the agent's handle (cut to 17 characters) plus 4 characters that tell two handles apart; Google limits an ID to 30 characters.
- Its display name is `Crewlet <company> <handle>`.

**The engine acts only on accounts it recorded when it made them,** by Google's unique ID, never adopting by name. An account named like Crewlet's but unrecorded is reported, never deleted.

### Tokens are issued for an hour, never stored

- An agent's credential is `GOOGLE_CLOUD_ACCESS_TOKEN_<HANDLE>`: a token Crewlet makes for the agent's account by impersonation when a tool needs it, good for one hour.
- It lives in the engine's memory and is renewed before expiry. Nothing is sealed, so nothing rotates.
- An agent that is off is issued nothing, and disabling its account stops an issued token at once.
- Until the account is made and Google has applied the grant, a tool that names the token waits and retries on its own. A refusal after that names what to fix, on the connection or the agent's row of the Agents tab.

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

**The Google Cloud CLI works in the agent's box too.** Crewlet writes the token to `.config/crewlet/gcloud-token` under the home of every Google Cloud agent's box, keeps it current while the run lasts, and sets `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` to its path in the box and `CLOUDSDK_CORE_PROJECT` to the agent's first project. A setting of the same name in the agent's own sandbox environment wins. The Agents tab lists these as provided by Crewlet. Install the CLI in your box image (Crewlet never downloads binaries); the tool reads live only for a Google Cloud MCP server, since Crewlet cannot see whether the CLI is installed. A token the run prints, `gcloud auth print-access-token` or the file's own, is kept in its session redacted, as every known credential shape is ([Code Sandbox](../concepts/code-sandbox.md#each-launchs-session-kept)). The run's live output shows the commands it runs and a failed call's first line, never a command's output, redacted by the same shapes ([Security model](../concepts/code-sandbox.md#security-model)).

### Agents in GKE clusters

**No level lets an agent write Kubernetes objects inside a cluster beyond deleting pods and scaling.** This is deliberate. Kubernetes Engine Developer and Admin:
- read every Secret;
- make Kubernetes service account tokens;
- let a workload run as any Kubernetes service account, which through Workload Identity means any Google service account bound to one: acting as another identity.

**Administer manages clusters and node pools** (Kubernetes Engine Cluster Admin), never the objects in them.

To let one agent do more inside a cluster, bind its account email to a Kubernetes Role yourself, for example:

```sh
kubectl create rolebinding sre-agent-edit --role=deployer --namespace=app \
  --user=crewlet-sre-a1b2@example-crewlet-id.iam.gserviceaccount.com
```

### Google applies changes within minutes

**Google's IAM is eventually consistent:** a new account can take a moment before a project accepts a grant to it, a grant can take minutes to apply, and a change can read back stale.

**Crewlet waits these out, never reporting a failure.** The sync ends as done once its writes are made, and the agent's tool on the Agents tab says what it waits on:
- "Waiting: Google is still applying the grant", up to 10 minutes after Crewlet grants access (turning on, raising the level, adding a project), since the first token can take a minute or two;
- "Waiting: Google is still applying the change", up to 3 minutes after any other change (lowering the level, dropping a project).

Each ends by itself. A sync that cannot see its own write after 3 minutes says so and the next pass retries: for a new account, "Google is still creating the agent's account".

### Disconnecting

**Disconnecting turns every agent off:** it disables each account this engine made and removes its roles, and deletes the accounts if you also choose to remove them. The identity project, provisioner, pool and provider are yours and stay; remove the provider's key if you will not reconnect.

## Google's growth products sign in through this identity

Google's growth products (Google Analytics, Search Console, Google Ads and Google Play) sign in through the identity project this connection sets up. No Google Cloud project needs to be prepared for them, but each needs Google Cloud connected with its provisioning block. The design is [ADR-0172](../../adr/0172-a-google-growth-product-signs-in-as-each-seats-own-growth-identity-never-its-cloud-identity.md).

**Each agent gets a second account, its growth identity,** used for those products alone:
- Its ID is `crewlet-` plus the agent's handle (cut to 15 characters) plus 6 characters, the name Crewlet gives every agent's identity at any vendor, and its display name ends in `growth`.
- It is never the agent's Google Cloud account, and its tokens never leave the engine: no agent's box holds one. You add its email in each product, at the role the agent may act with.
- Crewlet makes it when the agent's first growth product turns on and turns it off when the last turns off or the agent is removed. It keeps the account, so the roles you gave it return with the agent; delete it in the identity project if the agent will not return. Turning Google Cloud off for an agent leaves it working.
- It is recorded by Google's unique ID and never adopted by name. If somebody turns it off at Google, Crewlet reports it and leaves it off.

**The company reads its figures as its own reader,** `crewlet-gr-` plus the company's name (cut to 14 characters) plus 4 characters. When a growth product is on, the Cloud Shell block makes it, enables that product's APIs in the identity project, and lets only this company's Crewlet act as it. You add its email once in each product, at the product's read role.

### Products that refuse service accounts sign in with a person's consent

AdSense and AdMob accept no service account, so the company reaches them through one person's consent, which Crewlet keeps ([ADR-0178](../../adr/0178-an-integrations-consent-grant-is-a-named-grant-of-the-same-machinery.md)). [AdSense](adsense.md) and [AdMob](admob.md) connect this way. A product's sign-in covers reading alone until an agent may change settings; then a person signs in again for both. For each:
- **Your own OAuth client**, made as [below](#the-companys-own-oauth-client).
- **An administrator signs in** with Google from the product's page, at a fresh step-up. Crewlet asks for exactly the scopes the product's agents need, keeps the grant only if Google granted exactly those, seals the refresh token where no operator view reaches, and renews it on every node. Crewlet needs a public address for this: Google sends the browser back to it, and offers no device sign-in for these products.
- **Signing in again** replaces the grant and revokes nothing at Google. If Google refuses to renew the sign-in, the product reports `credential_rejected` and Crewlet stops asking Google until somebody signs in again.
- **Signing out or disconnecting** deletes Crewlet's copy, and revokes the access at Google only when no other product's sign-in uses a client in the same Cloud project, since Google's revocation would end that one too. When it does not revoke, the answer says how to remove Crewlet at myaccount.google.com. Separate projects per product make every sign-out a revocation.

### The company's own OAuth client

AdSense and AdMob sign in as it, and Google Analytics and Google Play use it, optionally, for **Add it for me**. Each product's connect form shows the values below; one client and one secret variable may serve every product.

1. In a Google Cloud project you control, open **Google Cloud › Credentials** and create an OAuth client ID of type **Web application**, after turning on the APIs the product's page names.
2. Under **Authorized redirect URIs**, add `<api.public.external_url>/webhooks/mcp-oauth` (`<api.external_url>/webhooks/mcp-oauth` without a public listener), exactly as the form shows it: Google refuses any address not registered exactly. Leave **Authorized JavaScript origins** empty, since the engine exchanges the code itself. When the address changes, as a temporary tunnel's can on restart, update the URI before the next sign-in. With no public address the form says so, and a sign-in is refused.
3. In **Google Auth Platform › Audience**, choose **Internal** if whoever signs in is in the project's Google Cloud organization. Otherwise choose **External** and press **Publish app** (Google's unverified-app warning is fine for the few people who sign in), or keep it in **Testing** and add each person who signs in as a test user: Google ends a test user's sign-in seven days after consent, and a refusal about then says so ([Manage App Audience](https://support.google.com/cloud/answer/15549945)).
4. Put the client's ID and secret in the product's `oauth.client_id` and `oauth.client_secret`, the secret as a `${VAR}`, or paste them on its page.

## Configuration

```yaml
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@example-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
      atlas_read: false
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
| `provisioning.atlas_read` | no | Let the atlas search what each project runs, as the account `crewlet-atlas` ([see more in Atlas](#see-more-in-atlas-what-each-project-runs)). Off by default. |
| `roles[].integrations.google_cloud.enabled` | yes | Whether the agent has its own account. Off disables it and removes its roles. |
| `roles[].integrations.google_cloud.level` | no | `viewer` (the default), `operator` or `administer`. |
| `roles[].integrations.google_cloud.projects` | when enabled | The project IDs the agent acts on, each prepared for Crewlet. At least one. |
| `roles[].integrations.google_cloud.grants` | no | Roles its account holds beside its level ([granting one more role](#granting-one-more-role)). Written by the Agents tab and by approving the agent's request, never typed. |

There is no company-wide project list: Crewlet reads the projects you prepared from the provisioner's own grants.

## Roles no level grants, and why

No level grants a role that mints or reveals a credential outliving the agent's own access, acts as another identity, grants on the project, or reads billing:

| Role | Why not |
|---|---|
| Owner, Editor | Basic roles with thousands of permissions, acting as other service accounts among them |
| Viewer (basic) | Reads every API key's string |
| Project IAM Admin, Security Admin | Grants anything on the project |
| Service Account User, Service Account Token Creator, Service Account Key Admin | Acts as, or makes credentials for, other service accounts |
| Secret Manager Secret Accessor | Reads secret payloads |
| Kubernetes Engine Developer, Kubernetes Engine Admin | Reads Secrets, mints Kubernetes tokens, runs workloads as other identities (see [Agents in GKE clusters](#agents-in-gke-clusters)) |
| Cloud Run Developer, Cloud Run Admin | Runs a job with overridden arguments as the job's own identity |
| Cloud SQL Editor (below Administer) | Creates client certificates |
| Storage HMAC Key Admin | Makes an HMAC key for any service account |
| Any billing role | No level touches billing |

Each prepared project's conditions refuse these to the provisioner too, so it cannot grant one even if asked.

## What the atlas reads

The engine reads the company's projects for its [atlas](../concepts/atlas.md#what-integrations-read) once an hour unless `atlas.refresh_interval_seconds` says otherwise, as the provisioner through the same federation, with no role beyond the setup blocks'. Every call is a `GET`; nothing at Google Cloud changes.

| Part | Calls | Role | What the atlas gains |
|---|---|---|---|
| Projects | `GET /v3/projects:search`, 100 a page | whatever lets the provisioner see the project | each active project as an account, with its id and display name. The identity project is the account the settings already name |
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

An API not in the table says nothing, including every API a new project has enabled before anybody uses it (Cloud Storage, BigQuery, Datastore, Cloud Logging, Cloud Monitoring).

**A project being deleted is not read.** Google keeps it thirty days in a non-active state. The atlas leaves it out from the first read after the deletion and retires it a day later.

**A refusal is a finding, never a failed read.** Where Google refuses a project's services (the provisioner lacks Service Usage Viewer there, as on a project the Prepare a project block never ran on), the project is still read as an account, the source's standing counts such projects and names the block, and what an earlier read said they are used for stays. If Google refuses the project listing itself, or does not trust the provisioner at federation or impersonation, the read fails as a rejected credential.

**What is never read.** A project's labels, tags, IAM policy, organization and folder. Nothing inside a project is read without **See more in Atlas**; with it, what each project runs is read by name and type alone.

**Cost and limits.** A read costs one call per hundred projects and one per active project, plus the two that mint the provisioner's token hourly. On Google's rate limit the call waits as long as Google asks (five seconds if no wait is named), up to three times and thirty seconds a wait. Past that, or when a listing fails part way, the read stops and records nothing, and the engine reads again five minutes later. A provisioner that can read more than 1,000 projects is not read: the standing says so, and nothing is retired. A project that two complete reads at least a day apart both miss is retired from the atlas.

### See more in Atlas: what each project runs

**See more in Atlas** on the connect form (`integrations.google_cloud.provisioning.atlas_read`, off by default) lets the atlas list what each project runs. It is optional and never changes what an agent can do.

**The one step.** Turn it on and save, then run the connection's Cloud Shell block again, as an Owner of the projects. The block now also:

- enables the Cloud Asset API in the identity project, and lets the new account use it there (Service Usage Consumer);
- creates the service account `crewlet-atlas` in the identity project, if missing;
- lets Crewlet's subject act as it (Workload Identity User), as for the provisioner;
- on every active project where the provisioner holds a role, creates the custom role `crewletAtlasReader`, holding `cloudasset.assets.searchAllResources` and nothing else, and grants it to `crewlet-atlas`. A project the person running the block may not change is named and skipped.

Nothing needs pasting back. A project prepared later is covered by the atlas lines of [Prepare a project](#prepare-a-project-for-crewlet), or by running the identity block again.

**Who reads.** The engine exchanges its federation token at Google's Security Token Service and impersonates `crewlet-atlas` directly, never through the provisioner. Cloud Audit Logs show each search under `crewlet-atlas@<identity project>.iam.gserviceaccount.com`, never under the provisioner or an agent.

**Why not Cloud Asset Viewer.** It also lists and exports assets whole, which include a function's environment variables or an instance's startup script. `crewletAtlasReader` holds only the search permission, and the search names its fields and never asks for a resource's configuration (`versionedResources`).

| Asset type | Read as | What the atlas gains |
|---|---|---|
| Compute Engine instance | a server | its name, internal and external addresses, and Region |
| Cloud SQL instance | a datastore | its name and Region. Its engine is in its configuration, which is not read |
| Memorystore for Redis instance | a datastore | its name, Region and the engine `redis` |
| AlloyDB cluster | a datastore | its name, Region and the engine `postgres` |
| Spanner instance | a datastore | its name and the engine `other` |
| Cloud Run service, Cloud Run function | a workload running on the project | its name and Region |
| Cloud Storage bucket | a bucket, by its global name | its name and location, and that the project uses object storage |
| Pub/Sub topic | a queue | its name |

Each entry belongs to, or runs on, its project's account. A resource Google is deleting is left out, and a stopped instance is still a server.

**What is never read.** No resource's configuration, labels, tags, description, network tags, keys, environment, metadata or data. Of an instance's attributes only its two address lists are decoded. Kubernetes Engine clusters, DNS zones and every type outside the table are not read.

**A refusal is a finding, never a failed read.** A project whose search Google refuses (the role was never granted there) is listed on the atlas's Sources tab, its earlier record stands, and other projects are read. An unreachable account, or a Cloud Asset API off in the identity project, is one finding naming the block. Any other failure stops the read, changing nothing.

**To undo it.** Turn the switch off, which stops the searches at once, and delete the account in the identity project:

```sh
gcloud iam service-accounts delete crewlet-atlas@IDENTITY.iam.gserviceaccount.com --project=IDENTITY
```

Its grants stop working with it. The leftover custom role `crewletAtlasReader` grants nothing; delete it with `gcloud iam roles delete crewletAtlasReader --project=PROJECT` if wanted. What the atlas recorded stays until a person retires it.

**Cost and limits.** A search costs one call per project per 500 resources, plus the two that mint the account's token hourly. Cloud Asset Inventory allows 400 searches a minute per billed project (the identity project); a read waits out the rate limit like any other.

## Google Cloud's limits worth knowing

These are Google's, not Crewlet's.

| Limit | Value | What it means |
|---|---|---|
| Service accounts per project | A per-project quota ("Service Account Count", commonly 100) | The identity project holds the provisioner and one account per agent that is on or off but not removed |
| Service account ID | 6 to 30 characters | Crewlet's names fit; see [The account is the engine's](#the-account-is-the-engines-and-named-for-the-seat) |
| Roles in one grant condition | 10 | Why each level has its own condition |
| Keys on a workload identity provider | 8 | Reconnecting updates the key in place |
| Token lifetime | 1 hour | Crewlet renews before expiry |
| Rate limits | A `429`, with `Retry-After` on some limits | A read waits as long as Google asks, or 5 seconds where it names no wait, up to 30 seconds and 3 times; a longer wait is left to the next pass. A write is never resent: Google answers a spent quota with the same status |

## What is not supported yet

- **Inbound alerts.** Cloud Monitoring alerts cannot wake an agent yet.
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
