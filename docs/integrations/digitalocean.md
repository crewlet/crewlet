# DigitalOcean

DigitalOcean gives each agent its **own personal access token** in the company's DigitalOcean team, at the level you choose. An agent reads Droplets, volumes, networking, DNS, databases, Kubernetes clusters, apps and monitoring and, where its level allows it, creates, changes and deletes them, through `doctl` in its own container and DigitalOcean's own MCP server.

DigitalOcean lets no API create a token, so **you create each agent's token in the control panel in a few clicks and paste it**. Crewlet proves the token's scopes, seals it, and from then on reads everything with that token. Crewlet holds no DigitalOcean token for provisioning, and revokes an agent's token itself when you turn the agent off or replace its token. Nothing here receives events from DigitalOcean yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | What it can do |
|---|---|
| `read` (the default) | Read Droplets, volumes, snapshots, networking, DNS, databases, Kubernetes clusters, apps, monitoring and projects. It changes nothing. |
| `operate` | Everything `read` does, plus create, change and delete Droplets, images, volumes and snapshots, firewalls, load balancers, VPCs, reserved IPs, DNS records and certificates, databases, Kubernetes clusters, apps, and alerts and uptime checks. |
| `administer` | Everything `operate` does, plus download Kubernetes cluster credentials (for `kubectl`), read database passwords, and open consoles on Droplets and apps. |

**Administer can create access that outlives the agent's token.** A kubeconfig, a database password or a console session the agent obtained keeps working after its token is revoked. Giving an agent `administer` asks for a fresh sign-in, it is never the default, and when you take it away you should rotate the credentials it could reach.

No level holds billing, team or role management, the team's stored secrets, its Spaces access keys, SSH keys or registry credentials, or any scope that creates a token. Each agent's token is its own, so turning one agent off never touches another.

## Setting it up from the dashboard

Company › Integrations › DigitalOcean asks one thing: the **team**, by its name as the team menu at the top of the control panel shows it. Its one other field, the **Atlas read token**, is optional ([let the atlas read the team](#let-the-atlas-read-the-team)). There is nothing to test about the team at this point. The first agent's token proves the team, and its Team ID is shown when the token is accepted, to compare with Account › Settings › Team.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

## Giving each agent its own token

Each agent seat is turned on or off, and given its level, on the integration's **Agents** tab (or the agent's own page, which uses the same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.digitalocean` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a DigitalOcean token at all. Required in every block. A seat with no block is off, and a new agent seat is off until somebody turns it on. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. |

```yaml
roles:
  - handle: sre
    integrations:
      digitalocean:
        enabled: true
        level: operate
```

Nothing else is written for the agent: Crewlet puts its token in its box as doctl's `DIGITALOCEAN_ACCESS_TOKEN` itself ([The agent's DigitalOcean tools](#the-agents-digitalocean-tools)).

### Each agent's access is a level

A level is a fixed set of DigitalOcean's own scopes, the same in every company. Each level holds the one before it.

| Level | Scopes it adds |
|---|---|
| `read` | `account:read`, `actions:read`, `regions:read`, `sizes:read`, `image:read`, `snapshot:read`, `droplet:read`, `block_storage:read`, `block_storage_action:read`, `block_storage_snapshot:read`, `firewall:read`, `load_balancer:read`, `vpc:read`, `vpc_peering:read`, `nat_gateway:read`, `reserved_ip:read`, `domain:read`, `certificate:read`, `cdn:read`, `spaces:read`, `database:read`, `kubernetes:read`, `app:read`, `monitoring:read`, `uptime:read`, `project:read`, `tag:read`, `ssh_key:read` |
| `operate` | the `create`, `update` and `delete` scopes of `droplet`, `image`, `kubernetes`, `app`, `block_storage`, `block_storage_snapshot`, `database`, `firewall`, `load_balancer`, `vpc`, `vpc_peering`, `nat_gateway`, `reserved_ip`, `domain`, `monitoring` and `uptime`; and `snapshot:delete`, `block_storage_action:create`, `spaces:update`, `certificate:create`, `certificate:delete`, `tag:create`, `tag:delete`, `project:assign_resource` |
| `administer` | `droplet:admin`, `kubernetes:access_cluster`, `database:view_credentials`, `app:access_console` |

`account:read` reads the token owner's name, email, limits and status, and the team; DigitalOcean has no scope that changes an account. Crewlet needs it to see whose token it is.

**Crewlet refuses a token holding any of these, whatever else it holds:** the Full Access and Read Only presets, `billing:read`, `security:read` (DigitalOcean's stored secrets and their values), every `registry` scope (registry credentials), every `spaces_key` scope, `ssh_key:create`, `ssh_key:update` and `ssh_key:delete`, every `dedicated_inference_token` scope, `team_membership:read`, `role_management:read`, `organization:create_team`, `organization_membership:read_teams`, `function:admin`, `functions:admin` and `partner_network_connect:view_credentials`.

### Setting a token up

Turning an agent on draws three steps, two of them yours. The agent's row shows **Needs you** and a **Set up** button until they are done:

1. **You:** in the team menu at the top of the control panel, switch to the company's team. Then open **API › Tokens › Generate New Token** (`cloud.digitalocean.com/account/api/tokens/new`):
   - name it as the sheet shows, `crewlet-<company>-<handle>-<level>`;
   - choose an expiry: the longest your policy allows (DigitalOcean offers 30, 60 or 90 days, 1 year, or no expiry). When it runs out, the agent's row shows **Needs you** and you paste a new token;
   - choose **Custom Scopes**, never Read Only or Full Access. Use **Quick bulk scope select › Select all Reads**, then untick the refused reads above. For `operate` and `administer`, tick the level's write scopes resource by resource; a resource section's own checkbox ticks all of its scopes, after which untick any the level does not grant. Do not use the bulk Creates, Updates or Deletes, which select refused scopes;
   - choose **Generate Token**. If DigitalOcean shows **Required scopes**, choose **Add scopes and continue**: they are reads the level already holds. DigitalOcean shows the token once.
2. **You:** copy the token, which starts with `dop_v1_`.
3. **You paste it, and Crewlet checks it** before it seals anything:
   - it is a personal access token, not an OAuth token;
   - it is not another agent's token;
   - DigitalOcean accepts it, the account is not locked, and the token is in the company's team;
   - it holds no refused scope;
   - it holds every scope of the agent's level, and none of a higher level's.

   A token that passes is sealed as `DIGITALOCEAN_TOKEN_<HANDLE>`. A token that fails is refused beside the field with what to do, and nothing is stored. DigitalOcean cannot change a token's scopes, so the remedy for a wrong scope is always a new token: delete the refused one in the control panel. Pasting a token is a credential write: it needs the `secrets.write` authority and a recent sign-in, and the audit records the token's expected name, never its value.

**How the scopes are proven.** A DigitalOcean token cannot read its own scopes, so Crewlet proves them with requests that cannot change anything: a one-row list read for each read scope, and for the writes that tell the levels apart a request DigitalOcean refuses with 403 when the scope is missing and rejects for its body or its target otherwise (a malformed or empty body, or an ID that cannot exist). Each was proven against a real team with a token that holds the scope and one that does not. A custom-scoped token's scopes never change, so the proof holds for the token's life; a scope with no public endpoint (`spaces:read`, `spaces:update`, `droplet:admin`, `team_membership:read`, `role_management:read`) cannot be proven, so tick exactly what the sheet lists.

### What each change does

| You do | What happens |
|---|---|
| Turn a seat on | The three steps above. |
| Change its level | DigitalOcean cannot change a token's scopes, so the row asks you to create a new token named for the new level and paste it. Crewlet checks it, seals it and revokes the previous token itself. |
| Turn it off | Crewlet clears `DIGITALOCEAN_TOKEN_<HANDLE>` at once, so the agent loses DigitalOcean straight away, and revokes the token at DigitalOcean. Nothing is left for you to delete. |
| Rotate its token | Choose **Regenerate Token** on the token's row in the control panel (it keeps the name, scopes and expiry, and the old value stops working at once), or create a new token with the same scopes, and paste it. Crewlet seals it and revokes the previous token if it still works. |

Until a replaced or turned-off token is revoked, Crewlet keeps it as `DIGITALOCEAN_RETIRED_TOKEN_<HANDLE>`, only to revoke it. Nothing hands it to an agent: the company document is refused if a seat names it. It is cleared as soon as the revoke succeeds, which is normally the same pass; if DigitalOcean is unreachable, each pass tries again. If DigitalOcean refuses the revoke while the token still works, the row asks you to delete the token in the control panel.

There is no **Keep at DigitalOcean** choice: a kept token is live access that no agent uses and nobody watches.

### The token is named for the seat

The token's value is sealed under one name the engine derives for the seat, `DIGITALOCEAN_TOKEN_<HANDLE>` (for example `DIGITALOCEAN_TOKEN_SRE`). The company document never names it, so nothing written to the document can point a seat's token at another variable. Seats are told apart by a keyed fingerprint of each token, so the same token pasted for two agents is refused.

### The agent's DigitalOcean tools

An agent works with DigitalOcean two ways, each bounded by its token's scopes.

**doctl, as itself.** doctl reads a token from `DIGITALOCEAN_ACCESS_TOKEN` in place of a login. While an agent is on at DigitalOcean, Crewlet sets `DIGITALOCEAN_ACCESS_TOKEN` in its box to the agent's own sealed token, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)). An agent turned off is given none from its next run. Until a token is pasted, a run starts without it and its start says it is waiting for the DigitalOcean integration (`sandbox_env_unresolved`, at info). A line of the seat's own under that name is ignored for the agent's token, and reported as an advisory at that line. A DigitalOcean token the run prints, its own or another (`dop_v1_`, `doo_v1_` or `dor_v1_` and its hexadecimal digits), is redacted from its session and from everything else it hands back, its live output included ([Security model](../concepts/code-sandbox.md#security-model)).

The token is doctl's and never the agent's model sign-in. OpenCode reads `DIGITALOCEAN_ACCESS_TOKEN` as the key of its DigitalOcean model provider, so an agent whose executor runs on an OpenCode entry that signs in to DigitalOcean's models by that name (`DIGITALOCEAN_ACCESS_TOKEN` in the entry's `env`) cannot also have doctl's token: in one box the model calls would be made with the agent's infrastructure token. Crewlet withholds the agent's token from that box, logs `sandbox_credential_withheld` at each launch, and reports an advisory at the seat's `integrations` block. Give that agent the MCP server below instead, or run its executor on an entry that signs in by another name. On any other entry the token is doctl's alone: the run's model is the entry's, and Crewlet never counts the agent's token as the run's sign-in.

doctl must be in the box. Install the one release Crewlet pins, checked against the checksums file DigitalOcean publishes with it, with a setup step like [Recipe 1](../concepts/code-sandbox.md#bring-your-own-image) does for `gh`:

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

With both in place, an agent runs commands such as `doctl compute droplet list`, `doctl kubernetes cluster list` or `doctl databases list` with no `doctl auth init`. The Agents tab reports per seat whether a tool is live or waiting for its token, and on a seat with neither tool it says the two ways to add one.

**DigitalOcean's MCP router.** DigitalOcean's remote MCP router at `https://mcp.digitalocean.com/mcp` fronts every DigitalOcean MCP service (Droplets, Kubernetes, databases, networking, volumes, apps, insights and more) through three tools: `find_tools`, `retrieve_schema` and `invoke_tool`. Add it from Company › Tools › MCP servers, where the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) offers it as `digitalocean`, as a per-agent server: each agent's own token is bound to it as a bearer `Authorization` header. A service's own endpoint (`https://droplets.mcp.digitalocean.com/mcp` and the like) is bound the same way.

### Every pass proves each token

Each pass reads every agent's token with the token itself, and proves its level again with the same requests the paste used. DigitalOcean is never called with any other token. A pass makes about forty requests per agent, every ten minutes by default (the integrations' check interval), well inside the 5,000 an hour DigitalOcean allows each token.

- **A token revoked, deleted, expired or regenerated elsewhere** answers 401. Crewlet clears it, never sends it again, and the row asks for a new token.
- **A token that no longer proves its level** usually means the person who created it was moved to a narrower team role, which takes scopes away from every token they made. The row says so: restore the role, or paste a new token.
- **A seat that leaves the document** is treated as off.

### Disconnecting

Disconnecting turns every agent off, revokes their tokens at DigitalOcean and clears them. If a revoke fails then, the row says which token to delete in the control panel.

## Let the atlas read the team

Optional. With nothing more than the team's name, the company's [atlas](../concepts/atlas.md#what-integrations-read) shows DigitalOcean as one account. To see what runs there, give the atlas a read-only token of its own. No agent needs it, nothing waits on it, and no agent's token is ever used for it.

**The one step.** In the control panel, switch to the team, open **API › Tokens › Generate New Token**, name the token `crewlet-atlas`, choose **Custom Scopes**, and tick these eight scopes and no other:

| Scope | What the atlas reads with it |
|---|---|
| `account:read` | which team the token acts in, so a token of another team is refused. The account's user and email are not read |
| `droplet:read` | each Droplet as a server: its name, region and addresses (a private address is marked internal) |
| `kubernetes:read` | each Kubernetes cluster as a server: its name and region. Its endpoint, its node pools and its credentials are not read |
| `database:read` | each managed database as a database (a Kafka cluster as a queue): its name, engine, version and region. Its connection details, its users and its passwords are not read |
| `load_balancer:read` | each load balancer as a server: its name, region and addresses |
| `domain:read` | each domain, and each name its A, AAAA and CNAME records give with what it points at. Records are asked for by those three types, so a TXT record is never sent |
| `app:read` | each App Platform app as a service: its name, its addresses, the GitHub and GitLab repositories it is built from, and the managed databases it is attached to. Its environment variables are not read |
| `project:read` | each Spaces bucket by its name, from the resources its project lists. DigitalOcean lists buckets themselves to a Spaces access key alone, which the atlas never holds |

Then paste the token under **Company › Integrations › DigitalOcean › Atlas read token** and press **Test**. Crewlet seals it as `DIGITALOCEAN_ATLAS_TOKEN` and reads the team within a minute, then every hour (the company's `atlas.refresh_interval_seconds`).

**A token that can change anything is refused.** A token cannot read its own scopes, so Crewlet proves them, when you press Test and again before every read, with the same requests that prove an agent's token: each is refused by DigitalOcean with a `403` before it can have an effect. A token that can create, change or delete a resource, read billing, the team's stored secrets, registry credentials or Spaces keys, download cluster credentials, read database passwords or open a console reads nothing, and the atlas's Sources page says which scope to remove. A Full Access or Read Only token is refused for that reason. The proof covers the scopes the checks can see, the same ones as for an agent, so a write scope no check reaches is not noticed: tick only the eight above.

**A scope you leave out is a part left unread.** The read completes with the rest, the Sources page names the scope to tick, and what an earlier read recorded of that part stays. DigitalOcean cannot change a token's scopes, so widening it means creating the token again.

**What DigitalOcean shows.** The token acts as the person who created it, as every personal access token does, and its name, `crewlet-atlas`, is what tells its reads from an agent's in the team's token list.

**To undo it.** Delete the token `crewlet-atlas` in the control panel and clear the field (or remove `integrations.digitalocean.atlas` from the company configuration). The atlas stops reading the team. What it recorded stays, each entry showing when it was last seen, until a person retires it.

**Cost.** One read is about thirty requests that prove the token, one listing per kind (a hundred rows a page, twenty for apps), three per domain and one per project, far inside the 5,000 an hour DigitalOcean allows a token. A `429` is waited out; a listing that fails part way stops the read, which records nothing and is made again five minutes later.

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
| `provisioning.team` | yes | The team's name as the control panel's team menu shows it, on one line with no leading or trailing space, or a `${VAR}`. Every agent's token must be made in this team. After the first token, later tokens must also be in the same team by its Team ID, so a second team with the same name is refused. |

| `atlas` | no | The atlas's own read of the team. Leave it out and the atlas shows the DigitalOcean account alone. |
| `atlas.token` | no | A `${VAR}` naming the sealed personal access token made for the atlas, with the eight read scopes of [Let the atlas read the team](#let-the-atlas-read-the-team). The dashboard seals it as `DIGITALOCEAN_ATLAS_TOKEN`. A token that can change anything, and any agent's own token, is refused. |

The block holds no agent's credential. Each agent's token is its own, pasted on the Agents tab.

## What DigitalOcean records

A personal access token acts as **the person who created it**. DigitalOcean's Audit History (Account › Security) records security events such as token creation under that person's name, email and IP address, and does not name the token. Resource changes such as a tag created and deleted were not shown there. So DigitalOcean cannot tell one agent from another: Crewlet's own run history records which agent took each action. What per-agent tokens do give you is per-agent revocation.

## What is not supported yet

- **Inbound events.** DigitalOcean's monitoring alerts can post to a webhook, but Crewlet has no route for them yet, so an alert wakes nobody. An agent reads alerts and metrics with doctl or the MCP router.
- **Spaces object contents.** Buckets are read and written with Spaces access keys, which no level can create, so agents manage buckets through the API but cannot read or write their objects.
- **Billing.** No level reads billing.
- **A CLI command.** The integration is set up from the dashboard or the company document; there is no `crewlet digitalocean` command.

## Troubleshooting

A token you paste is refused beside the field, with what to do:

| The sheet says | Why | What to do |
|---|---|---|
| This is an OAuth token, which expires within hours or days | It starts `doo_v1_` or `dor_v1_` | Generate a personal access token and paste it |
| This is not a DigitalOcean personal access token | It does not start `dop_v1_` followed by 64 characters | Copy the token DigitalOcean showed once when you generated it |
| This token is already …'s | Another agent has this token | Generate a token for this agent |
| DigitalOcean did not accept this token | It is wrong, expired, deleted or regenerated | Generate a new token and paste it |
| This token lacks account:read | `account:read` was unticked | Delete it and generate one with `account:read` |
| This DigitalOcean account is locked | DigitalOcean locked the account | Resolve it at DigitalOcean |
| This token was made in team …, and Crewlet is connected to team … | It was generated in another team | Switch teams in the team menu, generate it there, and delete this one |
| This token can … (…). Agents never hold it | It holds a refused scope, or it is Full Access or Read Only | Delete it and generate one with Custom Scopes and that scope unticked |
| This token lacks …, which … needs | A scope of the level was not ticked | Delete it and generate one with every scope the sheet lists |
| This token can …, and you chose Read | A Read token holds a write | Generate one with the Read scopes only, or choose Operate |
| This token can …, which only Administer grants | An Operate token holds an Administer scope | Generate one without it, or choose Administer |
| The token has a space, line break or invisible character | It was wrapped or copied with a hidden character | Copy it again in one piece |

The card reports `credential_missing` when `integrations.digitalocean.provisioning.team` is missing or resolves to nothing; fill in the team. An agent's own state is reported on its row on the **Agents** tab, with the control that answers it.

When an agent reports an error from doctl or the MCP router:

| It says | Why | What to do |
|---|---|---|
| `Unable to authenticate you` or 401 | The token was revoked, expired or regenerated | The row asks for a new token; paste one |
| `You are not authorized to perform this operation` or 403 | The agent's level does not include the action | Raise the agent's level on the Agents tab and paste a token for it |
| doctl asks for `doctl auth init` | The agent is off at DigitalOcean, no token is pasted yet, or its executor signs in by `DIGITALOCEAN_ACCESS_TOKEN` (`sandbox_credential_withheld`) | Turn the agent on and paste the token, or run its executor on another entry, as in [The agent's DigitalOcean tools](#the-agents-digitalocean-tools) |
| `doctl: command not found` | doctl is not installed in the box | Add the setup step above |
