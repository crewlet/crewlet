# Cloudflare

Cloudflare gives each agent its **own account API token**, scoped to the zones the company chooses and to the permissions its level allows, and the **Cloudflare API MCP server** to use it through. An agent reads zone analytics, cache status, DNS records, rules and settings as itself and, where you allow it, purges the cache, edits cache rules, changes the zones' caching and CDN settings and their transform rules, and changes their WAF rules and the account's IP lists.

The engine mints, narrows, rolls and deletes those tokens itself, through one token you create for it. Nothing here receives events from Cloudflare yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Read zone analytics, account analytics, cache rules and status, transform rules, DNS records, firewall rules, WAF rules (custom rules included), SSL and certificate settings and every zone setting, in the company's zones, and the account's IP lists | Change anything |
| `operate` | Everything `read` can, plus purge the cache and create, edit and delete cache rules | Edit DNS records, WAF rules, transform rules or any zone setting |
| `configure` | Everything `operate` can, plus change every zone setting (cache level, browser cache TTL, development mode, Always Online, Brotli, HTTP/2 and HTTP/3, early hints, tiered cache and cache reserve among them) and create, edit and delete compression rules and transform rules (URL rewrites, and request and response header changes such as adding a `Permissions-Policy` response header) | Edit DNS records, WAF rules, IP lists or certificates |
| `secure` | Everything `configure` can, plus create, edit and delete the zones' WAF rules (custom rules and rate limiting rules) and the account's IP lists and the addresses on them | Edit DNS records or certificates |

Cloudflare grants every zone setting as one permission group, so an agent on `configure` can also change a zone's SSL/TLS encryption mode, minimum TLS version and security level. It grants every transform rule as one group as well, so the agent that can add a response header can also rewrite the URL and the request headers your origin receives. Give it to an agent you trust with those.

`secure` decides who reaches your zones: an allow or skip rule, or an address an agent adds to an IP list, keeps working after the agent's token is gone. Raising an agent to it asks for a fresh sign-in (step-up), and `secure` is never the default. Review the zones' WAF rules and the account's IP lists when you take it away. IP lists belong to the account rather than to a zone, so `provisioning.zones` does not narrow them: an agent on any level reads every list in the account, and one on `secure` can change any of them.

Every agent's token is its own, so Cloudflare's audit log names the agent behind each change, and turning one agent off never touches another.

## Setting it up from the dashboard

Company › Integrations › Cloudflare asks two things and checks them before it saves:

1. **Account ID**: the 32-character ID on the account's overview page in the Cloudflare dashboard.
2. **Provisioner token**: the one token Crewlet uses to make each agent's own token. The help under the field lists every permission it needs as the token builder names them, the account's on one line and the zones' on the next. See [Creating the provisioner token](#creating-the-provisioner-token).

The form seals the token as `CLOUDFLARE_API_TOKEN`, points the config at it and activates. The next pass mints a token for every agent seat that is on.

Every agent's token reaches **every zone in the account** unless the company document narrows it. In this release the zone list is set in the document only (`integrations.cloudflare.provisioning.zones`, see [Configuration](#configuration)); the form does not ask for it. Everything below describes what that writes, and is what you edit by hand if you would rather.

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

The form offers **Test key** under the provisioner token. It runs read-only calls in order and stops at the first that fails, so the result names what to fix. Nothing is stored by a test.

Before any call, the value is held to what a token looks like: a space, a line break, an invisible character or anything but letters, digits, hyphens and underscores is refused beside the field, naming the character's position, and nothing is sent. Saving refuses the same values. A token Cloudflare refuses as malformed or invalid (its error codes 1000, 6003 and 6111) reads "This is not a valid Cloudflare API token.", with "Paste only the token's value, from Copy on the token page." as what to do, never as a wrong address.

| Stage | Call | What a failure means |
|---|---|---|
| The token is active | `GET /accounts/{account_id}/tokens/verify` | The token is wrong, disabled or expired, or is not a token of this account |
| It can manage tokens | `GET /accounts/{account_id}/tokens/permission_groups` | It lacks Account API Tokens Write |
| It can see the zones | `GET /zones?account.id={account_id}` | It lacks Zone Read, or a zone in `provisioning.zones` is not in the account |
| Every level resolves | the permission groups read above | A permission group a level names is not offered to this account |
| It holds what it grants | `GET /accounts/{account_id}/tokens/{token_id}`, the token's own policy | It lacks a permission group it needs itself, or one the `read` level grants, so no agent can be given a level. A group only a higher level grants leaves the token working, and the result names the level it leaves unavailable |

The last stage is skipped only when Cloudflare will not show the token its own policy. When every stage passes, the result reads "Works. The token holds every permission an agent can be given.", with the number of zones and the token's expiry below it, and a warning to replace the token when it expires within the warning window. A token that holds every level's permissions but `secure`'s works too, and a warning says so with what to do: "The Secure level is unavailable until the provisioner token holds Zone WAF: Edit and Account Filter Lists: Edit." and "Edit the provisioner token at Cloudflare to add them over the account and its zones." A token missing one that `read` grants is refused, "The token works, but no agent can be given a level yet.", with the same warning for the Read level: "The Read level is unavailable until the provisioner token holds Zone WAF: Read and Account Filter Lists: Read." and "Edit the provisioner token at Cloudflare to add them over the account and its zones." Once connected, each pass makes the same check. A group the level an agent is on needs is reported as `grant_short`, and that agent's token is not minted or narrowed until it is added; a group only a level no agent is on needs is a note, `level_unavailable`, and the integration stays ready.

## Giving each agent its own Cloudflare token

Each agent seat is turned on or off, and given its level, on the integration's **Agents** tab (or the agent's own page, which uses the same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.cloudflare` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a Cloudflare token at all. Required in every block. A seat with no block is off, and a new agent seat is off until somebody turns it on. |
| `level` | `read`, `operate`, `configure` or `secure`. Unset is `read`. |

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

### What each change does at Cloudflare

| You do | Crewlet does at Cloudflare |
|---|---|
| Turn a seat on | Creates an account API token named `crewlet-<company>-<handle>` (the company name in lowercase, with anything that is not a letter or digit turned into `-`) with the level's permission groups on the company's zones and an expiry 90 days out (`POST /accounts/{account_id}/tokens`), then seals its value. |
| Change its level | Rewrites the token's policies to the new level's groups (`PUT /accounts/{account_id}/tokens/{token_id}`). The value does not change, so the agent's tool keeps working. |
| Change the zones | Rewrites every agent's token to the new zones on the next pass, the same way. |
| Turn it off | Disables the token, so the agent stops at once, then deletes it (`DELETE /accounts/{account_id}/tokens/{token_id}`) and clears the sealed value. If the delete fails, the token stays disabled and the next pass deletes it. |
| Turn it back on | Mints a new token. No person is needed. |
| Remove it (only while off) | Nothing at Cloudflare: the token is already deleted. Crewlet stops tracking it. |
| Rotate its token | Rolls the token to a new value (`PUT /accounts/{account_id}/tokens/{token_id}/value`), seals it and moves its expiry 90 days out. The old value stops working at once. |

Crewlet only ever changes tokens it created. It records the ID of every token it mints and never touches another token in the account, even one under a `crewlet-` name.

### The token is the engine's, and named for the seat

The token's value is sealed under one name the engine derives for the seat, `CLOUDFLARE_API_TOKEN_<HANDLE>` (for example `CLOUDFLARE_API_TOKEN_SRE`). The company document never names it, so nothing written to the document can point a seat's token at another variable or at the provisioner's. Cloudflare shows a token's value exactly once, so a token that was minted and **could not be sealed is deleted again** at once rather than left live and held by nobody.

### Expiry and rotation

Every agent token expires **90 days** after it is minted. When fewer than **30 days** remain, the next pass rolls it to a new value, seals that value and moves the expiry 90 days out. A token that has already expired (because the integration was paused, say) is replaced in the same pass and the expired one deleted. An agent's token therefore never lapses for long while the integration is connected, and a leaked value is useful for at most 90 days.

**Rotate credential** on the Agents tab does the same roll on demand, for a value you believe has leaked.

### The provisioner token's own expiry

Nothing rotates the provisioner token: a person created it, and only a person can replace it. Where it has an expiry, each pass reads it, and inside **14 days** of it reports a `credential_expiring` finding naming `integrations.cloudflare.provisioning.api_token` and the date. The integration stays connected, since the token works today, and the card reads **Credential expiring** with **Replace key** as its one action. On the day it lapses every pass is refused, and no agent's token can be minted, narrowed or rolled. Tokens already minted keep working until their own expiry.

### The agent's Cloudflare tool

A token is half of an agent's access; the other half is the tool it uses Cloudflare through. Declare the Cloudflare API MCP server once, as a per-seat server, with no credential of its own:

```yaml
mcp_servers:
  - name: cloudflare
    transport: http
    shared: false                       # one instance per agent seat that is on
    url: https://mcp.cloudflare.com/mcp
```

The engine adds each seat's own token to that seat's instance as `Authorization: Bearer <token>`, so no seat block names it. The server's tools (`search`, `execute`, `docs` and `whoami`) can call any part of the Cloudflare API, and the server has no read-only mode of its own: what an agent can do is exactly what its token's permissions allow.

The Agents tab reports per seat whether the tool is live, waiting for its token, missing because the company declares no per-seat server at `mcp.cloudflare.com`, or not running because the server is turned off (`enabled: false`). Adding the server from Company › Tools › MCP servers offers it from the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry).

### Every pass reads each token back

Each pass reads every token this engine manages, including the token of a seat that has since left the document.

- **A token's permissions or zones changed at Cloudflare** by somebody else are restored to the seat's level and recorded as a `drift_restored` event, with the before and after.
- **A token deleted at Cloudflare** is dropped from the engine's records, and the same pass mints a fresh one if the seat is on.
- **A token somebody disabled** at Cloudflare is never turned back on by a pass, in case it was incident response. The tab offers **Turn back on** or **Keep off**.
- **A seat that leaves the document** is treated as off: its token is disabled, then deleted.

One known limitation: when you edit `provisioning.zones`, the next pass re-narrows every token to the new list and records each as `drift_restored`, although nobody changed anything at Cloudflare. The same happens once to every agent's token when a Crewlet version changes the permission groups its level grants: the next pass rewrites the token's policy in place, keeping its value, once the provisioner token holds the new groups. Until it does, the agent keeps its token as it was, and its row on the Agents tab names the groups to add.

A refusal names the step that failed, Cloudflare's own reason, what to do about it and who does it, with a link to the account's API tokens page. The step is retried on every pass and on **Retry**.

### Disconnecting

Disconnecting deletes every token this engine minted and clears their sealed values. A Cloudflare token has no history to keep, and one left live after a disconnect would be a credential nobody manages. The provisioner token is yours, so it stays at Cloudflare and in the secret store until you delete it.

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
| `provisioning` | yes | The account and the token the engine mints each agent's token with. Required when enabled: the integration receives nothing from Cloudflare, so an enabled block without it would do nothing at all. |
| `provisioning.account_id` | yes | The Cloudflare account the agents' tokens are minted in: 32 lowercase hexadecimal characters, or a `${VAR}`. |
| `provisioning.api_token` | yes | The [provisioner token](#creating-the-provisioner-token). Keep it a `${VAR}` reference into the secret store; the dashboard seals it as `CLOUDFLARE_API_TOKEN`. |
| `provisioning.zones` | no | Set in the document only; the connect form does not ask for it in this release. The zone names agents may reach, as Cloudflare lists them (`example.com`, with no scheme or path), each once. Each must be a zone in the account. Empty, or absent, scopes every token to all zones in the account, including zones added later. |

## What the atlas reads

The engine reads the account for the company's [atlas](../concepts/atlas.md#what-integrations-read) with the provisioner token, under the Read groups it already holds on every zone, once an hour unless the company's `atlas.refresh_interval_seconds` says otherwise: the account's name, every zone, every A, AAAA and CNAME record (what each name points at, and whether Cloudflare proxies it) and every certificate pack (what it covers, its type and status, who issued it and when it lapses). MX and TXT records add nothing, a TXT record's content is never decoded, and record comments and tags are never read. The read changes nothing at Cloudflare and needs no permission beyond the provisioner token's own. Load balancers are not read.

A read of ten thousand records in a hundred zones is about three hundred calls, a quarter of one five-minute window of the token's limit, which it shares with the reconcile loop. A read that fails part way records nothing, and a name that two complete reads at least a day apart both miss is retired from the atlas.

### See more in Atlas

The atlas also shows the account's Workers, Pages projects, R2 buckets and Tunnels where the provisioner token may read them. It is optional: the integration, every agent's token and the rest of the atlas work the same without it.

**To turn it on**, edit the provisioner token at Cloudflare (**Manage account**, **Account API tokens**, the token, **Edit**) and add any of the four account permissions in the [optional table above](#creating-the-provisioner-token): Workers Scripts Read, Cloudflare Pages Read, Workers R2 Storage Read and Cloudflare Tunnel Read. That is the whole step. The token's value does not change, so there is nothing to paste again, and the next hourly read shows what each permission allows. Each is taken on its own: add one and the atlas shows that product.

**To undo it**, remove the permissions from the token again. What the atlas already recorded stays, and the Cloudflare source on Company › Atlas says which permission to add back to keep it current; nothing is retired by a permission that was taken away.

| Part | Call | Permission group | What the atlas gains |
|---|---|---|---|
| Workers | `GET /accounts/{account}/workers/scripts` | Workers Scripts Read | each script as a workload that runs on the account, by its name |
| Pages | `GET /accounts/{account}/pages/projects`, 10 a page | Pages Read | each project as a workload that runs on the account, by its name |
| R2 | `GET /accounts/{account}/r2/buckets` | Workers R2 Storage Read | each bucket, by its name |
| Tunnels | `GET /accounts/{account}/cfd_tunnel`, deleted ones left out | Cloudflare Tunnel Read | each Tunnel as a workload that runs on the account, by its name |

**A missing permission is a suggestion, never a fault.** The Test button says the token works and adds one line, "See more in Atlas", naming the permissions it could gain. The card carries the same line as a note on a ready integration, and no level, no agent and no other read waits on it. The source's standing on the atlas names a part only where an earlier read recorded something from it, which is a permission somebody took away.

**What is never read.** The engine asks for no Worker's content, settings, bindings or secrets, no Pages deployment, no Tunnel's token, configuration or connections, and no R2 object. From the four listings it decodes a name or an id and nothing else: a Pages project's `deployment_configs`, where its environment variables are, its build settings, its source repository and its domains are never decoded, and neither are a Worker's routes or a Tunnel's connections. A name shaped like a credential is refused and counted.

**What the permissions could reach, so you can judge them.** Each permission covers every read of its product, more than the one listing the engine makes: Workers Scripts Read can read a script's code and the names of its bindings, Pages Read a project's plain environment variables, and Workers R2 Storage Read the objects in the buckets. The engine calls none of those, and no agent's token is ever minted with these permissions. If that is more than you want the provisioner token to hold, leave them off.

## Cloudflare's limits worth knowing

These are Cloudflare's, not Crewlet's, and an agent meets them as a refusal from the API.

| Limit | Value | What it means for an agent |
|---|---|---|
| Edge cache TTL minimum | 2 hours on Free, 1 hour on Pro, 1 second on Business and Enterprise | A cache rule an agent writes on a Free zone cannot set an edge TTL shorter than 2 hours |
| Cache rules per zone | 10 on Free | An agent cannot add an eleventh; it has to edit or replace one |
| Cache purge requests | 5 a minute on Free, in bursts of up to 25 | Purge several URLs in one request rather than one request each, and expect a `429` past the burst |
| API requests | 1,200 per 5 minutes for each token | Each agent has its own token, so one agent's burst never slows another, or the provisioner |
| Account API tokens | 500 per account | One for the provisioner, one per agent that is on, and every token people made by hand |

A `429` from Cloudflare carries `retry-after`. The engine's own calls honour it, waiting up to 30 seconds and up to 3 times; a longer wait is left to the next pass.

## What is not supported yet

- **Inbound alerts.** Cloudflare Notifications cannot wake an agent yet: there is no webhook route for them, so an alert reaches nobody. Note that Cloudflare offers several alert types only on paid plans (Health Checks need Pro, and the Origin Error Rate alert needs Enterprise).
- **A CLI command.** The integration is set up from the dashboard or the company document; there is no `crewlet cloudflare` command.
- **Write access to DNS records or certificates.** No level grants it; see [Each agent's access is a level](#each-agents-access-is-a-level).

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The `provisioning` block is missing, `integrations.cloudflare.provisioning.api_token` resolved to nothing, or `integrations.cloudflare.provisioning.account_id` does not resolve to an account ID | Add the block, seal the provisioner token as `CLOUDFLARE_API_TOKEN` (or point `api_token` at the variable that holds it), or correct the account ID |
| `credential_rejected` | Cloudflare refused the provisioner token: it is wrong, disabled or expired, belongs to another account, or lacks Account API Tokens Write, Account Settings Read or Zone Read | Create a new token with the [permissions above](#creating-the-provisioner-token) and replace it with **Replace key** |
| `credential_expiring` | The provisioner token expires within 14 days | Create a new token and replace it before the date the finding names |
| `grant_short` | The provisioner token lacks a permission group it needs itself (subject `integrations.cloudflare.provisioning.api_token`), or one the level an agent is on grants (subject `cloudflare level read`, `cloudflare level operate`, `cloudflare level configure` or `cloudflare level secure`), so that agent's token is not minted or narrowed | Add the group the finding names to the provisioner token, at Cloudflare, or move the agents at that level to one it holds |
| `unknown_tier` | A zone in `provisioning.zones` is not in the account (subject `integrations.cloudflare.provisioning.zones`), or a permission group the level an agent is on names is not offered to the account (subject `cloudflare level read`, `cloudflare level operate`, `cloudflare level configure` or `cloudflare level secure`) | Correct the zone name, or remove it from the list. For a permission group, check the account can use that product |
| `level_unavailable` | A note, not a fault: the provisioner token lacks a permission group that a level no agent is on grants, or the account is not offered one. The integration stays ready, and the Agents tab offers that level disabled with this reason | Add the groups the note names to the provisioner token at Cloudflare to offer the level, or leave it unavailable |
| `coverage_partial` | A note, not a fault (subject `cloudflare atlas permissions`): the provisioner token does not hold one or more of the optional read permissions the atlas would [see more](#see-more-in-atlas) with. The integration is ready, and nothing else waits on them | Add the permissions the note names to the provisioner token, or leave it as it is |
| `registration_orphaned` | Cloudflare holds active account tokens named like this company's agent tokens (`crewlet-<company>-...`) that the engine has no record of | Delete the ones nobody uses at Cloudflare. Crewlet never deletes a token it did not create |
| `identity_failed` | An agent's token could not be minted, narrowed or rolled; its level names a permission group the account does not offer, so it is not minted; somebody disabled it at Cloudflare; or Cloudflare would not read it back. Cloudflare's reason, or the missing group, is in the detail | Fix what the detail names, then **Retry** on the agent's row. For a token disabled at Cloudflare, choose **Turn back on** or **Keep off** |
| `identity_missing` | For up to about a minute after you first connect, "access migration has not finished": the one-time step every per-agent integration passes through | Nothing; it clears by itself |

An agent's own failures are also reported on that agent's row on the **Agents** tab, with the control that answers them.

If an agent says its tool refuses every call, check the token has no IP address filter (the MCP server refuses one) and that Account Settings Read is on the provisioner, so every level can grant it.

If an agent is refused reading a WAF custom rule (`403`, "request is not authorized"), its token was minted without Zone WAF Read. Add Zone WAF: Read and Account Filter Lists: Read to the provisioner token at Cloudflare (its value does not change), and the next pass brings every agent's token up to its level. An agent that has to change the rule needs `secure`.

If an agent is refused reading or changing a transform rule, such as a response header rule, its token predates the Crewlet version that added Zone Transform Rules to the levels, or the provisioner token does not hold them yet. Add Transform Rules: Read and Transform Rules: Edit (under Zone) to the provisioner token at Cloudflare, and the next pass brings every agent's token up to its level, keeping its value. Changing a transform rule needs `configure` or `secure`.
