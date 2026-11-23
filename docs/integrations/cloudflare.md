# Cloudflare

Cloudflare gives each agent its **own account API token**, scoped to the zones the company chooses and to the permissions its level allows, and the **Cloudflare API MCP server** to use it through. An agent reads zone analytics, cache status, DNS records, rules and settings as itself and, where you allow it, purges the cache, edits cache rules, changes zone caching and CDN settings and transform rules, and changes WAF rules and the account's IP lists.

The engine mints, narrows, rolls and deletes the tokens itself, through one token you create for it. Nothing here receives events from Cloudflare yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Read zone analytics, account analytics, cache rules and status, transform rules, DNS records, firewall rules, WAF rules (custom included), SSL and certificate settings and every zone setting, in the company's zones, and the account's IP lists | Change anything |
| `operate` | `read`, plus purge the cache and create, edit and delete cache rules | Edit DNS records, WAF rules, transform rules or any zone setting |
| `configure` | `operate`, plus change every zone setting (cache level, browser cache TTL, development mode, Always Online, Brotli, HTTP/2 and HTTP/3, early hints, tiered cache and cache reserve among them) and create, edit and delete compression rules and transform rules (URL rewrites, request and response header changes such as adding a `Permissions-Policy` response header) | Edit DNS records, WAF rules, IP lists or certificates |
| `secure` | `configure`, plus create, edit and delete WAF rules (custom and rate limiting) and the account's IP lists and the addresses on them | Edit DNS records or certificates |

Cloudflare grants every zone setting as one permission group, so `configure` can also change a zone's SSL/TLS encryption mode, minimum TLS version and security level. It grants all transform rules as one group too, so an agent that can add a response header can also rewrite the URL and the request headers your origin receives. Give it only to an agent you trust with those.

`secure` decides who reaches your zones: an allow or skip rule, or an address an agent adds to an IP list, keeps working after the agent's token is gone. Raising an agent to it asks for a fresh sign-in (step-up); it is never the default. Review the zones' WAF rules and the account's IP lists when you take it away. IP lists belong to the account, not a zone, so `provisioning.zones` does not narrow them: an agent on any level reads every list in the account, and one on `secure` can change any of them.

Every token is the agent's own, so Cloudflare's audit log names the agent behind each change, and turning one agent off never touches another.

A level is the safe default. When one agent needs one permission no level holds (origin rules, DNS edits, Workers), give it that permission group alone as a **grant** beside its level, for a few hours or for good; see [Giving one agent one more permission](#giving-one-agent-one-more-permission). The agent can ask for one itself when Cloudflare refuses it.

## Setting it up from the dashboard

Company › Integrations › Cloudflare asks two things and checks them before it saves:

1. **Account ID**: the 32-character ID on the account's overview page in the Cloudflare dashboard.
2. **Provisioner token**: the one token Crewlet uses to make each agent's own token. The help under the field lists every permission it needs as the token builder names them, the account's on one line and the zones' on the next. See [Creating the provisioner token](#creating-the-provisioner-token).

The form seals the token as `CLOUDFLARE_API_TOKEN`, points the config at it and activates. The next pass mints a token for every agent seat that is on.

Every agent's token reaches **every zone in the account** unless the company document narrows it. The zone list is set in the document only (`integrations.cloudflare.provisioning.zones`, see [Configuration](#configuration)); the form does not ask for it.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.
### Creating the provisioner token

The provisioner token is an **account** API token, owned by the account rather than by a person, so it keeps working when whoever created it leaves. Crewlet uses it for one job: making, narrowing, rolling and deleting the agents' own tokens. No agent ever holds it.

In the Cloudflare dashboard, open **Manage account**, then **Account API tokens**, then **Create token**, and start a **Custom token**. Give it these permissions:

| Permission group (API name) | In the dashboard | Why Crewlet needs it |
|---|---|---|
| Account API Tokens Write | Account, Account API Tokens, Edit | Mint, narrow, roll and delete the agents' tokens |
| Account Settings Read | Account, Account Settings, Read | Find the account, and test the token |
| Zone Read | Zone, Zone, Read | List the zones, and check each configured zone exists |

Then add **every permission a level can grant**, so the provisioner holds everything it may hand to an agent. Cloudflare does not document whether a token can grant a permission it does not hold itself, so Crewlet does not rely on it. A level whose permissions the token lacks is unavailable rather than broken: the Agents tab offers it disabled, naming what to add, until the token holds them:

| Permission group (API name) | In the dashboard | Granted to |
|---|---|---|
| Zone Settings Read | Zone, Zone Settings, Read | `read`, `operate`, `configure` and `secure` |
| Analytics Read | Zone, Analytics, Read | `read`, `operate`, `configure` and `secure` |
| DNS Read | Zone, DNS, Read | `read`, `operate`, `configure` and `secure` |
| Cache Settings Read | Zone, Cache Rules, Read | `read`, `operate`, `configure` and `secure` |
| Zone Transform Rules Read | Zone, Transform Rules, Read | `read`, `operate`, `configure` and `secure` |
| Firewall Services Read | Zone, Firewall Services, Read | `read`, `operate`, `configure` and `secure` |
| Zone WAF Read | Zone, Zone WAF, Read | `read`, `operate`, `configure` and `secure` |
| SSL and Certificates Read | Zone, SSL and Certificates, Read | `read`, `operate`, `configure` and `secure` |
| Account Analytics Read | Account, Account Analytics, Read | `read`, `operate`, `configure` and `secure` |
| Account Rule Lists Read | Account, Account Filter Lists, Read | `read`, `operate`, `configure` and `secure` |
| Cache Purge | Zone, Cache Purge, Purge | `operate`, `configure` and `secure` |
| Cache Settings Write | Zone, Cache Rules, Edit | `operate`, `configure` and `secure` |
| Zone Settings Write | Zone, Zone Settings, Edit | `configure` and `secure` |
| Response Compression Write | Zone, Response Compression, Edit | `configure` and `secure` |
| Zone Transform Rules Write | Zone, Transform Rules, Edit | `configure` and `secure` |
| Zone WAF Write | Zone, Zone WAF, Edit | `secure` |
| Account Rule Lists Write | Account, Account Filter Lists, Edit | `secure` |

Zone Read and Account Settings Read are granted to every level as well, which the first table already covers. The dashboard names some groups differently from the API (Cache Settings is Cache Rules there, Zone Transform Rules is Transform Rules, and Account Rule Lists is Account Filter Lists), as the Dashboard and API tabs of Cloudflare's [API token permissions](https://developers.cloudflare.com/fundamentals/api/reference/permissions/) reference show.

Under **Zone resources**, include all zones from the account, or the same zones `provisioning.zones` lists. Leave **Client IP address filtering** empty: the Cloudflare API MCP server refuses a token with IP filtering, and Crewlet never sets it on an agent's token either. An expiry date is your choice; see [The provisioner token's own expiry](#the-provisioner-tokens-own-expiry).

**Optional, to see more in Atlas.** Four more read permissions let the [atlas](#see-more-in-atlas) show the account's Workers, Pages projects, R2 buckets and Tunnels. Nothing else uses them, no level grants them to an agent, and a token without them is whole:

| Permission group (API name) | In the dashboard | What the atlas shows with it |
|---|---|---|
| Workers Scripts Read | Account, Workers Scripts, Read | each Workers script, by its name |
| Pages Read | Account, Cloudflare Pages, Read | each Pages project, by its name |
| Workers R2 Storage Read | Account, Workers R2 Storage, Read | each R2 bucket, by its name |
| Cloudflare Tunnel Read | Account, Cloudflare Tunnel, Read | each Tunnel, by its name |

Cloudflare shows the token's value once. Paste it into the form, or seal it yourself with `crewlet secrets set CLOUDFLARE_API_TOKEN`.

### Testing the token before you save it

**Test key** under the provisioner token runs read-only calls in order and stops at the first failure, which names what to fix. A test stores nothing.

First the value is checked: a space, a line break, an invisible character or anything but letters, digits, hyphens and underscores is refused beside the field, naming the character's position, and nothing is sent. Saving refuses the same values. A token Cloudflare refuses as malformed or invalid (error codes 1000, 6003, 6111) reads "This is not a valid Cloudflare API token.", with "Paste only the token's value, from Copy on the token page." as what to do, never as a wrong address.

| Stage | Call | What a failure means |
|---|---|---|
| The token is active | `GET /accounts/{account_id}/tokens/verify` | Wrong, disabled or expired, or not a token of this account |
| It can manage tokens | `GET /accounts/{account_id}/tokens/permission_groups` | Lacks Account API Tokens Write |
| It can see the zones | `GET /zones?account.id={account_id}` | Lacks Zone Read, or a zone in `provisioning.zones` is not in the account |
| Every level resolves | the permission groups read above | A permission group a level names is not offered to this account |
| It holds what it grants | `GET /accounts/{account_id}/tokens/{token_id}`, the token's own policy | Lacks a group it needs itself, or one the `read` level grants, so no agent can be given a level. A group only a higher level grants leaves the token working, and the result names the level left unavailable |

The last stage is skipped only when Cloudflare will not show the token its own policy. When every stage passes, the result reads "Works. The token holds every permission an agent can be given.", with the zone count and the token's expiry, and a warning to replace the token when it expires within the warning window.

A token holding every level's permissions but `secure`'s works too, with a warning: "The Secure level is unavailable until the provisioner token holds Zone WAF: Edit and Account Filter Lists: Edit." and "Edit the provisioner token at Cloudflare to add them over the account and its zones."

A token missing one that `read` grants is refused, "The token works, but no agent can be given a level yet.", with the same warning for the Read level: "The Read level is unavailable until the provisioner token holds Zone WAF: Read and Account Filter Lists: Read." and the same "Edit the provisioner token…" line.

Once connected, each pass makes the same check. A group needed by the level an agent is on is reported as `grant_short`, and that agent's token is not minted or narrowed until it is added. A group needed only by a level no agent is on is a note, `level_unavailable`, and the integration stays ready.

## Giving each agent its own Cloudflare token

Turn each agent seat on or off, and set its level, on the integration's **Agents** tab (or the agent's own page, same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.cloudflare` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a Cloudflare token. Required in every block. A seat with no block is off, and a new agent seat is off until turned on. |
| `level` | `read`, `operate`, `configure` or `secure`. Unset is `read`. |
| `grants` | Permission groups the token holds beside its level, each `{id, permission, scope, expires_at, granted_by, granted_at, reason, request}`. Written by the Agents tab and by approving an agent's request, never typed: the engine writes `id`, `granted_by` and `granted_at`. See [Giving one agent one more permission](#giving-one-agent-one-more-permission). |

```yaml
roles:
  - handle: sre
    integrations:
      cloudflare:
        enabled: true
        level: operate
```
### Each agent's access is a level

A level is a fixed set of Cloudflare permission groups, the same in every company:

| Level | Permission groups (API names) |
|---|---|
| `read` | Zone Read, Zone Settings Read, Analytics Read, DNS Read, Cache Settings Read, Zone Transform Rules Read, Firewall Services Read, Zone WAF Read, SSL and Certificates Read, Account Analytics Read, Account Rule Lists Read and Account Settings Read |
| `operate` | Everything in `read`, plus Cache Purge and Cache Settings Write |
| `configure` | Everything in `operate`, plus Zone Settings Write, Response Compression Write and Zone Transform Rules Write |
| `secure` | Everything in `configure`, plus Zone WAF Write and Account Rule Lists Write |

Account Settings Read is in every level because the Cloudflare API MCP server needs to find the account the token belongs to before it answers anything.

Crewlet looks each group up by its exact name in the account's own list (`GET /accounts/{account_id}/tokens/permission_groups`) on every pass and uses the ID Cloudflare returns, so nothing depends on IDs that could differ between accounts. A name the account does not offer is **a finding, never a fallback**: the token is not minted rather than minted with less, or more, than its level says.

`configure` adds what Cloudflare's API reference asks for to change caching and CDN settings: every zone setting, and the tiered cache, cache reserve, cache variants and Argo settings, take Zone Settings Write, and compression rules take Response Compression Write. It adds Zone Transform Rules Write too, which Cloudflare asks for to change a zone's transform rules (its URL rewrite, request header and response header rules, in the `http_request_transform`, `http_request_late_transform` and `http_response_headers_transform` phases of `/zones/{zone_id}/rulesets`); `read` holds Zone Transform Rules Read, so every level reads them. Page Rules are left out because Cloudflare does not let an account API token use them.

`read` reaches WAF custom rules through Zone WAF Read. Firewall Services Read covers only Cloudflare's older firewall resources, such as IP access rules, and Cloudflare refuses a zone's custom rules (`GET /zones/{zone_id}/rulesets/phases/http_request_firewall_custom/entrypoint`) to a token without Zone WAF. Account Rule Lists Read reads the IP lists a rule matches on, such as `ip.src in $office` (`GET /accounts/{account_id}/rules/lists`).

`secure` adds the writes to both: Zone WAF Write changes a zone's WAF rules, which Cloudflare grants as one group for its custom rules and rate limiting rules alike, and Account Rule Lists Write changes the account's lists and their items. It is a level of its own above `configure`, so an agent on `configure` never gains them without somebody choosing `secure` for it.

DNS Write and SSL and Certificates Write are deliberately in no level.

### Giving one agent one more permission

A grant is one Cloudflare permission group on one agent's token beside its level. Example: the SRE on `secure` that must create an origin rule (ruleset phase `http_request_origin`) gets **Origin Rules: Edit** on `example.com` for four hours; nobody else's token changes.

On the **Agents** tab, each agent's row lists its grants (an agent that is off lists none): the permission as the token builder names it, where it applies, time left (or Permanent), and on hover who granted it and why. **Add grant** opens a picker over the account's permission groups, read live from Cloudflare, with the zones it applies to (none chosen is every zone the company's agents reach), how long (4 hours, 1 day, 7 days or Permanent) and why, which the grant keeps. The remove on a grant's tag takes it off. Both change the tab's draft; **Save and sync** writes one revision of the company document, under the seat's `grants`, and narrows the token on the next sync, in place, keeping its value.

- **Only what can be minted is offered.** The picker lists every permission group a token can be granted at the account or its zones, by its token-builder name where Cloudflare renames it (Origin Write is **Origin Rules: Edit**), marked **Edit** unless read-only. A group the provisioner token does not hold is offered disabled, saying what to add at Cloudflare: Crewlet grants nothing the token does not hold itself.
- **Never granted:** a group that manages API tokens, the account's members (Cloudflare files them under Account Settings Write) or billing, or Access's organization, identity providers, groups or service tokens. Each would outlive the agent's token or let it give itself more.
- **A write asks what `secure` asks.** Adding a grant of any non-read-only group asks the person saving for a fresh sign-in; a machine token cannot save one. Removing or shortening a grant never does.
- **A grant never outlives its expiry.** It leaves the token on the pass a minute before it lapses; the reconcile loop runs that pass at the lapse rather than on its ten-minute settled interval. Once no token holds it, it is removed from the company document in a revision of its own.
- **A grant the provisioner cannot give** (it lost the group at Cloudflare, say) is reported as `grant_short`, naming the group to add, and left off the token; the rest of the token still matches its level and other grants.
- **Grants a level already holds add nothing**: the picker says which level holds each, and an agent asking for one is told it already has it.

A grant is also part of a seat's own access for the turn: its "Your access" section lists each grant its token holds, with where and until when, and the next turn after a change is told.

#### An agent asks for one itself

When Cloudflare refuses an agent's call for a permission its token lacks (`403`, "request is not authorized"), the agent asks with `request_access`, naming the permission group as Cloudflare's API names it, its zones and how many hours the change needs (1 to 168, 4 by default), on the work item that needs it and of an administrator. It never asks a person to edit its token at Cloudflare: the next pass would undo that as drift.

The engine checks the request against the account's live catalogue, stores it and puts it in that person's Inbox as a decision: **Grant for this change (expires in N hours)** (recommended), **Add to this agent permanently**, or **Not now**, with the agent's reason. A group the provisioner does not hold yet is still asked, saying what to add at Cloudflare first. Choosing a grant shows the grant built from the stored request; **Grant** answers with that option, which is the approval itself: the engine saves the grant first and only then posts the answer, so the agent is told yes only once its token holds the permission. It needs an administrator signed in within the step-up window (and in person, for a permission that changes things), the same from the Inbox, Ask Crewlet and the API; an agent can never choose it. **Not now** answers at once. A request nobody answers is dropped after 30 days.

Every grant asked for, granted, declined, removed, lapsed or refused is a `seat_grant_changed` event, naming the agent, the permission and its zones, and the person or engine behind it.

### What each change does at Cloudflare

| You do | Crewlet does at Cloudflare |
|---|---|
| Turn a seat on | Creates an account API token named `crewlet-<company>-<handle>` (company name lowercased, non-letter/digit characters turned into `-`) with the level's permission groups on the company's zones and an expiry 90 days out (`POST /accounts/{account_id}/tokens`), then seals its value. |
| Change its level | Rewrites the token's policies to the new level's groups (`PUT /accounts/{account_id}/tokens/{token_id}`). The value is unchanged, so the agent's tool keeps working. |
| Add or remove a grant, or one lapses | Rewrites the policies to the level's groups plus live grants, each on its own zones or the account, the same way. |
| Change the zones | Rewrites every agent's token to the new zones on the next pass, the same way. |
| Turn it off | Disables the token (the agent stops at once), then deletes it (`DELETE /accounts/{account_id}/tokens/{token_id}`) and clears the sealed value. If the delete fails, the token stays disabled and the next pass deletes it. |
| Turn it back on | Mints a new token. No person is needed. |
| Remove it (only while off) | Nothing at Cloudflare: the token is already deleted. Crewlet stops tracking it. |
| Rotate its token | Rolls the token to a new value (`PUT /accounts/{account_id}/tokens/{token_id}/value`), seals it and moves its expiry 90 days out. The old value stops working at once. |

Crewlet only changes tokens it created. It records the ID of every token it mints and never touches another, even one under a `crewlet-` name.

### The token is the engine's, and named for the seat

The token's value is sealed under one name the engine derives for the seat, `CLOUDFLARE_API_TOKEN_<HANDLE>` (for example `CLOUDFLARE_API_TOKEN_SRE`). The company document never names it, so nothing written there can point a seat's token at another variable or the provisioner's. Cloudflare shows a token's value once, so a token minted but **not sealed is deleted again** at once rather than left live and held by nobody.

### Expiry and rotation

Every agent token expires **90 days** after minting. With fewer than **30 days** left, the next pass rolls it to a new value, seals it and moves the expiry 90 days out. An already expired token (the integration was paused, say) is replaced in the same pass and the expired one deleted. So a token never lapses for long while the integration is connected, and a leaked value is useful for at most 90 days.

**Rotate credential** on the Agents tab does the same roll on demand, for a value you believe leaked.

### The provisioner token's own expiry

Nothing rotates the provisioner token: only a person can replace it. Where it has an expiry, each pass reads it, and inside **14 days** reports a `credential_expiring` finding naming `integrations.cloudflare.provisioning.api_token` and the date. The integration stays connected, and the card reads **Credential expiring** with **Replace key** as its one action. On the day it lapses every pass is refused, and no agent's token can be minted, narrowed or rolled; tokens already minted work until their own expiry.

### The agent's Cloudflare tool

A token is half of an agent's access; the other half is the tool. Declare the Cloudflare API MCP server once, per seat, with no credential of its own:

```yaml
mcp_servers:
  - name: cloudflare
    transport: http
    shared: false                       # one instance per agent seat that is on
    url: https://mcp.cloudflare.com/mcp
```

The engine adds each seat's own token to that seat's instance as `Authorization: Bearer <token>`, so no seat block names it. The server's tools (`search`, `execute`, `docs` and `whoami`) can call any part of the Cloudflare API, and it has no read-only mode: what an agent can do is exactly what its token allows.

The Agents tab reports per seat whether the tool is live, waiting for its token, missing (the company declares no per-seat server at `mcp.cloudflare.com`), or not running (`enabled: false`). Adding the server from Company › Tools › MCP servers offers it from the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry).

### Every pass reads each token back

Each pass reads every token this engine manages, including that of a seat that has left the document.

- **A token's permissions or zones changed at Cloudflare** by somebody else are restored to the seat's level and grants and recorded as a `drift_restored` event, with before and after.
- **A token deleted at Cloudflare** is dropped from the engine's records, and the same pass mints a fresh one if the seat is on.
- **A token somebody disabled** at Cloudflare is never turned back on by a pass, in case it was incident response. The tab offers **Turn back on** or **Keep off**.
- **A seat that leaves the document** is treated as off: its token is disabled, then deleted.

Known limitation: editing `provisioning.zones` makes the next pass re-narrow every token and record each as `drift_restored`, though nothing changed at Cloudflare. The same happens once to every token when a Crewlet version changes the groups a level grants: the next pass rewrites the policy in place, keeping its value, once the provisioner token holds the new groups. Until then the agent keeps its token as it was, and its Agents-tab row names the groups to add.

A refusal names the failed step, Cloudflare's reason, what to do and who does it, with a link to the account's API tokens page. The step is retried every pass and on **Retry**.

### Disconnecting

Disconnecting deletes every token this engine minted and clears their sealed values, since a token left live would be a credential nobody manages. The provisioner token is yours: it stays at Cloudflare and in the secret store until you delete it.

## Configuration

```yaml
integrations:
  cloudflare:
    enabled: true
    provisioning:
      account_id: 023e105f4ecef8ad9ca31a8372d0c353
      api_token: "${CLOUDFLARE_API_TOKEN}"   # the provisioner token
      zones: [example.com, example.net]      # optional: empty means every zone in the account
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning` | yes | The account and token the engine mints each agent's token with. Required when enabled: the integration receives nothing from Cloudflare, so without it it would do nothing. |
| `provisioning.account_id` | yes | The account the agents' tokens are minted in: 32 lowercase hex characters, or a `${VAR}`. |
| `provisioning.api_token` | yes | The [provisioner token](#creating-the-provisioner-token). Keep it a `${VAR}` reference into the secret store; the dashboard seals it as `CLOUDFLARE_API_TOKEN`. |
| `provisioning.zones` | no | Document only; the connect form does not ask for it in this release. Zone names agents may reach, as Cloudflare lists them (`example.com`, no scheme or path), each once and each in the account. Empty or absent scopes every token to all zones, including ones added later. |

## What the atlas reads

With the provisioner token, under the Read groups it already holds on every zone, the engine reads the account for the company's [atlas](../concepts/atlas.md#what-integrations-read) once an hour unless `atlas.refresh_interval_seconds` says otherwise: the account's name, every zone, every A, AAAA and CNAME record (what each name points at, and whether Cloudflare proxies it) and every certificate pack (coverage, type, status, issuer and lapse date). MX and TXT records add nothing, a TXT record's content is never decoded, and record comments and tags are never read. The read changes nothing at Cloudflare and needs no permission beyond the provisioner token's own. Load balancers are not read.

Ten thousand records in a hundred zones is about three hundred calls, a quarter of one five-minute window of the token's limit, shared with the reconcile loop. A read that fails part way records nothing; a name that two complete reads at least a day apart both miss is retired from the atlas.

### See more in Atlas

The atlas also shows the account's Workers, Pages projects, R2 buckets and Tunnels where the provisioner token may read them. It is optional: the integration, every agent's token and the rest of the atlas work the same without it.

**To turn it on**, edit the provisioner token at Cloudflare (**Manage account**, **Account API tokens**, the token, **Edit**) and add any of the four account permissions in the [optional table above](#creating-the-provisioner-token): Workers Scripts Read, Cloudflare Pages Read, Workers R2 Storage Read and Cloudflare Tunnel Read. The token's value does not change, so nothing is pasted again, and the next hourly read shows what each allows. Each is taken on its own.

**To undo it**, remove the permissions again. What the atlas already recorded stays, and the Cloudflare source on Company › Atlas says which permission to add back to keep it current; nothing is retired by a removed permission.

| Part | Call | Permission group | What the atlas gains |
|---|---|---|---|
| Workers | `GET /accounts/{account}/workers/scripts` | Workers Scripts Read | each script as a workload that runs on the account, by name |
| Pages | `GET /accounts/{account}/pages/projects`, 10 a page | Pages Read | each project as a workload that runs on the account, by name |
| R2 | `GET /accounts/{account}/r2/buckets` | Workers R2 Storage Read | each bucket, by name |
| Tunnels | `GET /accounts/{account}/cfd_tunnel`, deleted ones left out | Cloudflare Tunnel Read | each Tunnel as a workload that runs on the account, by name |

**A missing permission is a suggestion, never a fault.** The Test button says the token works and adds one line, "See more in Atlas", naming the permissions it could gain. The card carries the same line as a note on a ready integration, and nothing waits on it. The source's standing on the atlas names a part only where an earlier read recorded something from it, i.e. a permission somebody took away.

**What is never read.** No Worker's content, settings, bindings or secrets, no Pages deployment, no Tunnel's token, configuration or connections, no R2 object. From the four listings the engine decodes a name or an id only: a Pages project's `deployment_configs`, environment variables, build settings, source repository and domains, a Worker's routes and a Tunnel's connections are never decoded. A name shaped like a credential is refused and counted.

**What the permissions could reach.** Each permission covers every read of its product, more than the one listing the engine makes: Workers Scripts Read can read a script's code and its bindings' names, Pages Read a project's plain environment variables, Workers R2 Storage Read the objects in the buckets. The engine calls none of those, and no agent's token is ever minted with these permissions. If that is more than you want the provisioner token to hold, leave them off.

## Cloudflare's limits worth knowing

These are Cloudflare's, not Crewlet's; an agent meets them as an API refusal.

| Limit | Value | What it means for an agent |
|---|---|---|
| Edge cache TTL minimum | 2 hours on Free, 1 hour on Pro, 1 second on Business and Enterprise | A cache rule on a Free zone cannot set an edge TTL under 2 hours |
| Cache rules per zone | 10 on Free | An agent cannot add an eleventh; it edits or replaces one |
| Cache purge requests | 5 a minute on Free, in bursts of up to 25 | Purge several URLs in one request, and expect a `429` past the burst |
| API requests | 1,200 per 5 minutes for each token | Each agent has its own token, so one's burst never slows another or the provisioner |
| Account API tokens | 500 per account | One for the provisioner, one per agent that is on, and every token made by hand |

A `429` carries `retry-after`. The engine's own calls honour it, waiting up to 30 seconds and up to 3 times; a longer wait is left to the next pass.

## What is not supported yet

- **Inbound alerts.** Cloudflare Notifications cannot wake an agent: there is no webhook route, so an alert reaches nobody. Several alert types need paid plans (Health Checks need Pro, the Origin Error Rate alert needs Enterprise).
- **A CLI command.** Set up from the dashboard or the company document; there is no `crewlet cloudflare` command.
- **Write access to DNS records or certificates at a level.** No level grants it; give the one agent that needs it DNS Write or SSL and Certificates Write as a grant ([Giving one agent one more permission](#giving-one-agent-one-more-permission)).

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The `provisioning` block is missing, `integrations.cloudflare.provisioning.api_token` resolved to nothing, or `integrations.cloudflare.provisioning.account_id` does not resolve to an account ID | Add the block, seal the provisioner token as `CLOUDFLARE_API_TOKEN` (or point `api_token` at the variable holding it), or correct the account ID |
| `credential_rejected` | Cloudflare refused the provisioner token: wrong, disabled, expired, another account's, or lacking Account API Tokens Write, Account Settings Read or Zone Read | Create a new token with the [permissions above](#creating-the-provisioner-token) and replace it with **Replace key** |
| `credential_expiring` | The provisioner token expires within 14 days | Create a new token and replace it before the date named |
| `grant_short` | The provisioner token lacks a group it needs itself (subject `integrations.cloudflare.provisioning.api_token`), or one the level an agent is on grants (subject `cloudflare level read`, `cloudflare level operate`, `cloudflare level configure` or `cloudflare level secure`), so that agent's token is not minted or narrowed; or one an agent's grant names (subject the agent's handle), so that grant is left off its token | Add the named group to the provisioner token at Cloudflare, or move the agents at that level to one it holds, or remove the grant |
| `unknown_tier` | A zone in `provisioning.zones` is not in the account (subject `integrations.cloudflare.provisioning.zones`), or a group the agent's level names is not offered to the account (subject `cloudflare level read`, `cloudflare level operate`, `cloudflare level configure` or `cloudflare level secure`) | Correct or remove the zone. For a group, check the account can use that product |
| `level_unavailable` | A note, not a fault: the provisioner token lacks a group a level no agent is on grants, or the account is not offered one. The integration stays ready, and the Agents tab offers that level disabled with this reason | Add the named groups to the provisioner token at Cloudflare to offer the level, or leave it unavailable |
| `coverage_partial` | A note, not a fault (subject `cloudflare atlas permissions`): the provisioner token lacks one or more optional read permissions the atlas would [see more](#see-more-in-atlas) with. The integration is ready, nothing waits on them | Add the named permissions, or leave it |
| `registration_orphaned` | Cloudflare holds active account tokens named like this company's agent tokens (`crewlet-<company>-...`) that the engine has no record of | Delete the unused ones at Cloudflare. Crewlet never deletes a token it did not create |
| `identity_failed` | An agent's token could not be minted, narrowed or rolled; its level names a group the account does not offer; somebody disabled it at Cloudflare; or Cloudflare would not read it back. The detail holds Cloudflare's reason or the missing group | Fix what the detail names, then **Retry** on the agent's row. For a token disabled at Cloudflare, choose **Turn back on** or **Keep off** |
| `identity_missing` | For up to about a minute after you first connect, "access migration has not finished": the one-time step every per-agent integration passes through | Nothing; it clears by itself |

An agent's own failures are also on its row on the **Agents** tab, with the control that answers them.

If an agent says its tool refuses every call, check the token has no IP address filter (the MCP server refuses one) and that Account Settings Read is on the provisioner, so every level can grant it.

If an agent is refused reading a WAF custom rule (`403`, "request is not authorized"), its token was minted without Zone WAF Read. Add Zone WAF: Read and Account Filter Lists: Read to the provisioner token at Cloudflare (its value does not change); the next pass brings every agent's token up to its level. Changing the rule needs `secure`.

If an agent is refused creating or changing an origin rule (`403`, "request is not authorized", on the `http_request_origin` phase), no level grants origin rules: give that agent **Origin Rules: Edit** as a grant on the zone, or approve its `request_access`. The provisioner token must hold Origin Rules: Edit (under Zone) first.

If an agent is refused reading or changing a transform rule, such as a response header rule, its token predates the Crewlet version that added Zone Transform Rules to the levels, or the provisioner token does not hold them yet. Add Transform Rules: Read and Transform Rules: Edit (under Zone) to the provisioner token at Cloudflare; the next pass brings every agent's token up to its level, keeping its value. Changing a transform rule needs `configure` or `secure`.
