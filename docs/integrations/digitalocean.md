# DigitalOcean

Each agent gets its **own personal access token** in the company's DigitalOcean team, at the level you choose. Through `doctl` in its container and DigitalOcean's MCP server, an agent reads Droplets, volumes, networking, DNS, databases, Kubernetes clusters, apps and monitoring and, where its level allows, creates, changes and deletes them.

No DigitalOcean API creates a token, so **you create each token in the control panel and paste it**. Crewlet proves its scopes, seals it, and reads everything with it. Crewlet holds no DigitalOcean token for provisioning, and revokes an agent's token when you turn the agent off or replace the token. Nothing here receives events from DigitalOcean; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | What it can do |
|---|---|
| `read` (the default) | Read Droplets, volumes, snapshots, networking, DNS, databases, Kubernetes clusters, apps, monitoring and projects. It changes nothing. |
| `operate` | `read`, plus create, change and delete Droplets, images, volumes and snapshots, firewalls, load balancers, VPCs, reserved IPs, DNS records and certificates, databases, Kubernetes clusters, apps, and alerts and uptime checks. |
| `administer` | `operate`, plus download Kubernetes cluster credentials (for `kubectl`), read database passwords, and open consoles on Droplets and apps. |

**Administer can create access that outlives the agent's token.** A kubeconfig, database password or console session keeps working after its token is revoked. `administer` asks for a fresh sign-in and is never the default; when you take it away, rotate the credentials the agent could reach.

No level holds billing, team or role management, stored secrets, Spaces access keys, SSH keys, registry credentials, or any scope that creates a token. Turning one agent off never touches another's token.

## Setting it up from the dashboard

Company › Integrations › DigitalOcean asks for the **team**, by its name in the control panel's team menu, and optionally the **Atlas read token** ([let the atlas read the team](#let-the-atlas-read-the-team)). The team is not tested here: the first agent's token proves it, and its Team ID is shown on acceptance, to compare with Account › Settings › Team.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

## Giving each agent its own token

Turn a seat on or off and set its level on the integration's **Agents** tab (or the agent's page). The tab writes one revision of the company document, under the seat's `roles[].integrations.digitalocean` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a DigitalOcean token. Required in every block. A seat with no block, and a new seat, is off. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. |

```yaml
roles:
  - handle: sre
    integrations:
      digitalocean:
        enabled: true
        level: operate
```

Crewlet itself puts the token in the agent's box as `DIGITALOCEAN_ACCESS_TOKEN` ([The agent's DigitalOcean tools](#the-agents-digitalocean-tools)).

### Each agent's access is a level

A level is a fixed set of DigitalOcean scopes, the same in every company. Each level holds the one before it.

| Level | Scopes it adds |
|---|---|
| `read` | `account:read`, `actions:read`, `regions:read`, `sizes:read`, `image:read`, `snapshot:read`, `droplet:read`, `block_storage:read`, `block_storage_action:read`, `block_storage_snapshot:read`, `firewall:read`, `load_balancer:read`, `vpc:read`, `vpc_peering:read`, `nat_gateway:read`, `reserved_ip:read`, `domain:read`, `certificate:read`, `cdn:read`, `spaces:read`, `database:read`, `kubernetes:read`, `app:read`, `monitoring:read`, `uptime:read`, `project:read`, `tag:read`, `ssh_key:read` |
| `operate` | the `create`, `update` and `delete` scopes of `droplet`, `image`, `kubernetes`, `app`, `block_storage`, `block_storage_snapshot`, `database`, `firewall`, `load_balancer`, `vpc`, `vpc_peering`, `nat_gateway`, `reserved_ip`, `domain`, `monitoring` and `uptime`; and `snapshot:delete`, `block_storage_action:create`, `spaces:update`, `certificate:create`, `certificate:delete`, `tag:create`, `tag:delete`, `project:assign_resource` |
| `administer` | `droplet:admin`, `kubernetes:access_cluster`, `database:view_credentials`, `app:access_console` |

`account:read` (token owner's name, email, limits, status and the team) is needed to see whose token it is; no scope changes an account.

**Crewlet refuses a token holding any of these, whatever else it holds:** the Full Access and Read Only presets, `billing:read`, `security:read` (DigitalOcean's stored secrets and their values), every `registry` scope (registry credentials), every `spaces_key` scope, `ssh_key:create`, `ssh_key:update` and `ssh_key:delete`, every `dedicated_inference_token` scope, `team_membership:read`, `role_management:read`, `organization:create_team`, `organization_membership:read_teams`, `function:admin`, `functions:admin` and `partner_network_connect:view_credentials`.

### Setting a token up

The agent's row shows **Needs you** and a **Set up** button until these are done:

1. **You:** switch to the company's team in the control panel's team menu, then open **API › Tokens › Generate New Token** (`cloud.digitalocean.com/account/api/tokens/new`):
   - name it as the sheet shows, `crewlet-<handle>-<level>` (from the one rule Crewlet names every agent identity by, at most 30 characters; a long handle is cut before the level);
   - choose the longest expiry your policy allows (30, 60 or 90 days, 1 year, or none). At expiry the row shows **Needs you**; paste a new token;
   - choose **Custom Scopes**, never Read Only or Full Access. Use **Quick bulk scope select › Select all Reads**, then untick the refused reads above. For `operate` and `administer`, tick the level's write scopes per resource (a section's checkbox ticks all its scopes; untick any the level lacks). Never use the bulk Creates, Updates or Deletes, which select refused scopes;
   - choose **Generate Token**. If **Required scopes** appears, choose **Add scopes and continue**: they are reads the level holds. The token is shown once.
2. **You:** copy the token, which starts with `dop_v1_`.
3. **You paste it, and Crewlet checks** that:
   - it is a personal access token, not OAuth, and not another agent's;
   - DigitalOcean accepts it, the account is not locked, and it is in the company's team;
   - it holds no refused scope, every scope of the agent's level, and none of a higher level's.

   A passing token is sealed as `DIGITALOCEAN_TOKEN_<HANDLE>`. A failing one is refused beside the field with what to do, and nothing is stored. Scopes cannot change, so the remedy is always a new token: delete the refused one. Pasting needs the `secrets.write` authority and a recent sign-in; the audit records the token's expected name, never its value.

**How the scopes are proven.** A token cannot read its own scopes, so Crewlet sends requests that change nothing: a one-row list read per read scope, and for the writes separating the levels a request DigitalOcean refuses with 403 when the scope is missing and rejects for its body or target otherwise (malformed or empty body, or an impossible ID). Scopes never change, so the proof holds for the token's life. A scope with no public endpoint (`spaces:read`, `spaces:update`, `droplet:admin`, `team_membership:read`, `role_management:read`) cannot be proven, so tick exactly what the sheet lists.

### What each change does

| You do | What happens |
|---|---|
| Turn a seat on | The three steps above. |
| Change its level | The row asks for a new token named for the new level. Crewlet checks and seals it and revokes the previous one. |
| Turn it off | Crewlet clears `DIGITALOCEAN_TOKEN_<HANDLE>` at once, so the agent loses access immediately, and revokes the token. Nothing is left for you to delete. |
| Rotate its token | Choose **Regenerate Token** on its row in the control panel (keeps name, scopes and expiry; the old value stops at once), or create a new token with the same scopes, and paste it. Crewlet seals it and revokes the previous one if it still works. |

Until a replaced or turned-off token is revoked, Crewlet keeps it as `DIGITALOCEAN_RETIRED_TOKEN_<HANDLE>`, only to revoke it. No agent gets it: the company document is refused if a seat names it. It is cleared when the revoke succeeds, normally the same pass; if DigitalOcean is unreachable, each pass retries. If DigitalOcean refuses the revoke while the token works, the row asks you to delete it in the control panel.

There is no **Keep at DigitalOcean** choice: a kept token is live access nobody watches.

### The token is named for the seat

The token is sealed under the name the engine derives, `DIGITALOCEAN_TOKEN_<HANDLE>` (for example `DIGITALOCEAN_TOKEN_SRE`). The company document never names it, so it cannot point a seat at another variable. A keyed fingerprint tells seats apart, so one token pasted for two agents is refused.

### The agent's DigitalOcean tools

Two ways, each bounded by the token's scopes.

**doctl, as itself.** While an agent is on, Crewlet sets `DIGITALOCEAN_ACCESS_TOKEN` in its box to its own sealed token, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)). An agent turned off gets none from its next run. Until a token is pasted, a run starts without it and says it is waiting for the DigitalOcean integration (`sandbox_env_unresolved`, at info; a warning where the integration cannot say whether it holds the agent up, unless this node is only still reading its findings: [`unsure`](../concepts/code-sandbox.md#failure-modes)). A line of the seat's own under that name is ignored and reported as an advisory at that line. A DigitalOcean token the run prints, its own or another (`dop_v1_`, `doo_v1_` or `dor_v1_` and its hexadecimal digits), is redacted from its session and everything it hands back, live output included ([Security model](../concepts/code-sandbox.md#security-model)).

The token is doctl's, never the model sign-in. OpenCode reads `DIGITALOCEAN_ACCESS_TOKEN` as the key of its DigitalOcean model provider, so an agent whose executor runs on an OpenCode entry signing in to DigitalOcean's models by that name (`DIGITALOCEAN_ACCESS_TOKEN` in the entry's `env`) cannot also have doctl's token: model calls would use the infrastructure token. Crewlet withholds the token from that box, logs `sandbox_credential_withheld` at each launch, and reports an advisory at the seat's `integrations` block. Give that agent the MCP server below, or run its executor on an entry signing in by another name. On any other entry the token is doctl's alone and never counted as the run's sign-in.

doctl must be in the box. Install the release Crewlet pins, checked against DigitalOcean's checksums file, with a setup step like [Recipe 1](../concepts/code-sandbox.md#bring-your-own-image) does for `gh`:

```yaml
providers:
  sandbox:
    setup:
      - name: doctl
        files:
          .profile: |
            export PATH="$HOME/.local/bin:$PATH"
        commands:
          - |
            set -eu
            doctl_version=2.1.0; a=$(dpkg --print-architecture)
            t="doctl-${doctl_version}-linux-${a}.tar.gz"
            base="https://github.com/digitalocean/doctl/releases/download/v${doctl_version}"
            cd "$(mktemp -d)"
            curl -fsSL --retry 5 -o "$t" "$base/$t"
            curl -fsSL --retry 5 -o sums.txt "$base/doctl-${doctl_version}-checksums.sha256"
            grep " ${t}\$" sums.txt | sha256sum -c -
            tar -xzf "$t" doctl
            mkdir -p "$HOME/.local/bin" && mv doctl "$HOME/.local/bin/doctl"
```

Then an agent runs `doctl compute droplet list`, `doctl kubernetes cluster list` or `doctl databases list` with no `doctl auth init`. The Agents tab reports per seat whether a tool is live or waiting for its token, and on a seat with neither tool names the two ways to add one.

**DigitalOcean's MCP router.** `https://mcp.digitalocean.com/mcp` fronts every DigitalOcean MCP service (Droplets, Kubernetes, databases, networking, volumes, apps, insights and more) through three tools: `find_tools`, `retrieve_schema` and `invoke_tool`. Add it from Company › Tools › MCP servers, where the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) offers it as `digitalocean`, a per-agent server: each agent's token is bound as a bearer `Authorization` header. A service's own endpoint (`https://droplets.mcp.digitalocean.com/mcp` and the like) is bound the same way.

### Every pass proves each token

Each pass reads every agent's token with itself and re-proves its level with the paste's requests; DigitalOcean is never called with another token. About forty requests per agent every ten minutes by default (the integrations' check interval), well inside DigitalOcean's 5,000 an hour per token.

- **A token revoked, deleted, expired or regenerated elsewhere** answers 401. Crewlet clears it, never sends it again, and the row asks for a new one.
- **A token that no longer proves its level** usually means its creator was moved to a narrower team role, which takes scopes from every token they made. The row says so: restore the role, or paste a new token.
- **A seat that leaves the document** is treated as off.

### Disconnecting

Disconnecting turns every agent off, revokes their tokens and clears them. If a revoke fails, the row names the token to delete in the control panel.

## Let the atlas read the team

Optional. With only the team's name, the company's [atlas](../concepts/atlas.md#what-integrations-read) shows DigitalOcean as one account. To see what runs there, give it a read-only token of its own. No agent needs it, nothing waits on it, and no agent's token is used for it.

**The one step.** In the control panel, switch to the team, open **API › Tokens › Generate New Token**, name it `crewlet-atlas`, choose **Custom Scopes**, and tick these eight scopes and no other:

| Scope | What the atlas reads with it |
|---|---|
| `account:read` | which team the token acts in, so a token of another team is refused. User and email are not read |
| `droplet:read` | each Droplet as a server: name, region, addresses (a private one marked internal) |
| `kubernetes:read` | each cluster as a server: name and region. Endpoint, node pools and credentials are not read |
| `database:read` | each managed database as a database (Kafka as a queue): name, engine, version, region. Connection details, users and passwords are not read |
| `load_balancer:read` | each load balancer as a server: name, region, addresses |
| `domain:read` | each domain, and each name its A, AAAA and CNAME records give with its target. Only those three types are requested, so a TXT record is never sent |
| `app:read` | each App Platform app as a service: name, addresses, the GitHub and GitLab repositories it is built from, the managed databases attached. Environment variables are not read |
| `project:read` | each Spaces bucket by name, from the resources its project lists (buckets themselves are listed only to a Spaces access key, which the atlas never holds) |

Paste the token under **Company › Integrations › DigitalOcean › Atlas read token** and press **Test**. Crewlet seals it as `DIGITALOCEAN_ATLAS_TOKEN` and reads the team within a minute, then every hour (the company's `atlas.refresh_interval_seconds`).

**A token that can change anything is refused.** On Test and before every read, Crewlet proves the scopes with the requests that prove an agent's token: each is refused by DigitalOcean with a `403` before it can have an effect. A token that can create, change or delete a resource, read billing, stored secrets, registry credentials or Spaces keys, download cluster credentials, read database passwords or open a console reads nothing, and the atlas's Sources page names the scope to remove (so Full Access and Read Only are refused). The proof covers only scopes the checks can see, so a write scope no check reaches goes unnoticed: tick only the eight above.

**A scope you leave out is a part left unread.** The read completes with the rest, the Sources page names the scope to tick, and what an earlier read recorded of that part stays. Widening a token means creating it again.

**What DigitalOcean shows.** The token acts as its creator, like every personal access token; its name, `crewlet-atlas`, tells its reads from an agent's in the team's token list.

**To undo it.** Delete the token `crewlet-atlas` in the control panel and clear the field (or remove `integrations.digitalocean.atlas` from the company configuration). The atlas stops reading; what it recorded stays, each entry showing when last seen, until a person retires it.

**Cost.** One read is about thirty proving requests, one listing per kind (a hundred rows a page, twenty for apps), three per domain and one per project, far inside 5,000 an hour. A `429` is waited out; a listing that fails part way stops the read, which records nothing and retries five minutes later.

## Configuration

```yaml
integrations:
  digitalocean:
    enabled: true
    provisioning:
      team: Example Team
    atlas:
      token: ${DIGITALOCEAN_ATLAS_TOKEN}
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning` | yes | The team the agents' tokens act in. Required when enabled. |
| `provisioning.team` | yes | The team's name as the control panel's team menu shows it, on one line with no leading or trailing space, or a `${VAR}`. Every token must be made in this team; after the first, later tokens must match its Team ID, so a second team with the same name is refused. |
| `atlas` | no | The atlas's own read of the team. Left out, the atlas shows the account alone. |
| `atlas.token` | no | A `${VAR}` naming the sealed personal access token made for the atlas, with the eight read scopes of [Let the atlas read the team](#let-the-atlas-read-the-team). The dashboard seals it as `DIGITALOCEAN_ATLAS_TOKEN`. A token that can change anything, or any agent's token, is refused. |

The block holds no agent credential; each is pasted on the Agents tab.

## What DigitalOcean records

A personal access token acts as **the person who created it**. Audit History (Account › Security) records security events such as token creation under that person's name, email and IP address, not the token's name; resource changes such as a tag created and deleted were not shown. DigitalOcean cannot tell agents apart: Crewlet's run history records which agent took each action. Per-agent tokens give per-agent revocation.

## What is not supported yet

- **Inbound events.** Monitoring alerts can post to a webhook, but Crewlet has no route for them, so an alert wakes nobody. An agent reads alerts and metrics with doctl or the MCP router.
- **Spaces object contents.** Objects need Spaces access keys, which no level can create; agents manage buckets through the API but cannot read or write objects.
- **Billing.** No level reads billing.
- **A CLI command.** Set up from the dashboard or the company document; there is no `crewlet digitalocean` command.

## Troubleshooting

A pasted token is refused beside the field, with what to do:

| The sheet says | Why | What to do |
|---|---|---|
| This is an OAuth token, which expires within hours or days | It starts `doo_v1_` or `dor_v1_` | Generate a personal access token and paste it |
| This is not a DigitalOcean personal access token | It does not start `dop_v1_` followed by 64 characters | Copy the token shown once when you generated it |
| This token is already …'s | Another agent has it | Generate a token for this agent |
| DigitalOcean did not accept this token | Wrong, expired, deleted or regenerated | Generate a new token and paste it |
| This token lacks account:read | `account:read` was unticked | Delete it and generate one with `account:read` |
| This DigitalOcean account is locked | DigitalOcean locked it | Resolve it at DigitalOcean |
| This token was made in team …, and Crewlet is connected to team … | Generated in another team | Switch teams in the team menu, generate it there, and delete this one |
| This token can … (…). Agents never hold it | A refused scope, or Full Access or Read Only | Delete it and generate one with Custom Scopes and that scope unticked |
| This token lacks …, which … needs | A scope of the level was not ticked | Delete it and generate one with every scope the sheet lists |
| This token can …, and you chose Read | A Read token holds a write | Generate one with the Read scopes only, or choose Operate |
| This token can …, which only Administer grants | An Operate token holds an Administer scope | Generate one without it, or choose Administer |
| The token has a space, line break or invisible character | Wrapped or copied with a hidden character | Copy it again in one piece |

The card reports `credential_missing` when `integrations.digitalocean.provisioning.team` is missing or resolves to nothing; fill it in. An agent's state is on its row on the **Agents** tab, with the control that answers it.

When an agent reports an error from doctl or the MCP router:

| It says | Why | What to do |
|---|---|---|
| `Unable to authenticate you` or 401 | Token revoked, expired or regenerated | The row asks for a new token; paste one |
| `You are not authorized to perform this operation` or 403 | The agent's level lacks the action | Raise the level on the Agents tab and paste a token for it |
| doctl asks for `doctl auth init` | The agent is off, no token is pasted yet, or its executor signs in by `DIGITALOCEAN_ACCESS_TOKEN` (`sandbox_credential_withheld`) | Turn the agent on and paste the token, or run its executor on another entry, as in [The agent's DigitalOcean tools](#the-agents-digitalocean-tools) |
| `doctl: command not found` | doctl is not installed in the box | Add the setup step above |
