# Cloudflare

Cloudflare gives each agent its **own account API token**, scoped to the zones the company chooses and to the permissions its level allows, and the **Cloudflare API MCP server** to use it through. An agent reads zone analytics, cache status, DNS records, rules and settings as itself and, where you allow it, purges the cache and edits cache rules.

The engine mints, narrows, rolls and deletes those tokens itself, through one token you create for it. Nothing here receives events from Cloudflare yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Read zone analytics, account analytics, cache settings and status, DNS records, firewall and WAF rules, SSL and certificate settings and every zone setting, in the company's zones | Change anything |
| `operate` | Everything `read` can, plus purge the cache and create, edit and delete cache rules | Edit DNS records, WAF rules or any other zone setting |

Every agent's token is its own, so Cloudflare's audit log names the agent behind each change, and turning one agent off never touches another.

## Setting it up from the dashboard

Company › Integrations › Cloudflare asks two things and checks them before it saves:

1. **Account ID**: the 32-character ID on the account's overview page in the Cloudflare dashboard.
2. **Provisioner token**: the one token Crewlet uses to make each agent's own token. See [Creating the provisioner token](#creating-the-provisioner-token).

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

Then add **every permission a level can grant**, so the provisioner holds everything it may hand to an agent. Cloudflare does not document whether a token can grant a permission it does not hold itself, so Crewlet does not rely on it:

| Permission group (API name) | In the dashboard | Granted to |
|---|---|---|
| Zone Settings Read | Zone, Zone Settings, Read | `read` and `operate` |
| Analytics Read | Zone, Analytics, Read | `read` and `operate` |
| DNS Read | Zone, DNS, Read | `read` and `operate` |
| Cache Settings Read | Zone, Cache Settings, Read | `read` and `operate` |
| Firewall Services Read | Zone, Firewall Services, Read | `read` and `operate` |
| SSL and Certificates Read | Zone, SSL and Certificates, Read | `read` and `operate` |
| Account Analytics Read | Account, Account Analytics, Read | `read` and `operate` |
| Cache Purge | Zone, Cache Purge, Purge | `operate` |
| Cache Settings Write | Zone, Cache Settings, Edit | `operate` |

Zone Read and Account Settings Read are granted to both levels as well, which the first table already covers.

Under **Zone resources**, include all zones from the account, or the same zones `provisioning.zones` lists. Leave **Client IP address filtering** empty: the Cloudflare API MCP server refuses a token with IP filtering, and Crewlet never sets it on an agent's token either. An expiry date is your choice; see [The provisioner token's own expiry](#the-provisioner-tokens-own-expiry).

Cloudflare shows the token's value once. Paste it into the form, or seal it yourself with `crewlet secrets set CLOUDFLARE_API_TOKEN`.

### Testing the token before you save it

The form offers **Test key** under the provisioner token. It runs read-only calls in order and stops at the first that fails, so the result names what to fix. Nothing is stored by a test.

| Stage | Call | What a failure means |
|---|---|---|
| The token is active | `GET /accounts/{account_id}/tokens/verify` | The token is wrong, disabled or expired, or is not a token of this account |
| It can manage tokens | `GET /accounts/{account_id}/tokens/permission_groups` | It lacks Account API Tokens Write |
| It can see the zones | `GET /zones?account.id={account_id}` | It lacks Zone Read, or a zone in `provisioning.zones` is not in the account |
| Every level resolves | the permission groups read above | A permission group a level names is not offered to this account |
| It holds what it grants | `GET /accounts/{account_id}/tokens/{token_id}`, the token's own policy | It lacks a permission group a level grants, and the result names it |

The last stage is skipped only when Cloudflare will not show the token its own policy. When every stage passes, the result reads "Works. The token is active, reads 3 zones in the account and holds every permission an agent level grants.", followed by the token's expiry where it has one. Once connected, each pass makes the same check, and a group the provisioner has lost is reported as `grant_short`.

## Giving each agent its own Cloudflare token

Each agent seat is turned on or off, and given its level, on the integration's **Agents** tab (or the agent's own page, which uses the same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.cloudflare` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a Cloudflare token at all. Required in every block. A seat with no block is off, and a new agent seat is off until somebody turns it on. |
| `level` | `read` or `operate`. Unset is `read`. |

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
| `read` | Zone Read, Zone Settings Read, Analytics Read, DNS Read, Cache Settings Read, Firewall Services Read, SSL and Certificates Read, Account Analytics Read, Account Settings Read |
| `operate` | Everything in `read`, plus Cache Purge and Cache Settings Write |

Account Settings Read is in both because the Cloudflare API MCP server needs to find the account the token belongs to before it answers anything.

Crewlet looks each group up by its exact name in the account's own list (`GET /accounts/{account_id}/tokens/permission_groups`) on every pass and uses the ID Cloudflare returns, so nothing depends on IDs that could differ between accounts. A name the account does not offer is **a finding, never a fallback**: the token is not minted rather than minted with less, or more, than its level says.

DNS Write, WAF Write and Zone Settings Write are deliberately in no level. An agent that should change DNS or security rules is a decision for a later, separate level.

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

One known limitation: when you edit `provisioning.zones`, the next pass re-narrows every token to the new list and records each as `drift_restored`, although nobody changed anything at Cloudflare.

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
- **Write access to DNS, WAF or zone settings.** No level grants it; see [Each agent's access is a level](#each-agents-access-is-a-level).

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | The `provisioning` block is missing, `integrations.cloudflare.provisioning.api_token` resolved to nothing, or `integrations.cloudflare.provisioning.account_id` does not resolve to an account ID | Add the block, seal the provisioner token as `CLOUDFLARE_API_TOKEN` (or point `api_token` at the variable that holds it), or correct the account ID |
| `credential_rejected` | Cloudflare refused the provisioner token: it is wrong, disabled or expired, belongs to another account, or lacks Account API Tokens Write, Account Settings Read or Zone Read | Create a new token with the [permissions above](#creating-the-provisioner-token) and replace it with **Replace key** |
| `credential_expiring` | The provisioner token expires within 14 days | Create a new token and replace it before the date the finding names |
| `grant_short` | The provisioner token lacks a permission group a level grants | Add the group the finding names to the provisioner token, at Cloudflare |
| `unknown_tier` | A zone in `provisioning.zones` is not in the account (subject `integrations.cloudflare.provisioning.zones`), or a permission group a level names is not offered to the account (subject `cloudflare level read` or `cloudflare level operate`) | Correct the zone name, or remove it from the list. For a permission group, check the account can use that product |
| `registration_orphaned` | Cloudflare holds active account tokens named like this company's agent tokens (`crewlet-<company>-...`) that the engine has no record of | Delete the ones nobody uses at Cloudflare. Crewlet never deletes a token it did not create |
| `identity_failed` | An agent's token could not be minted, narrowed or rolled; its level names a permission group the account does not offer, so it is not minted; somebody disabled it at Cloudflare; or Cloudflare would not read it back. Cloudflare's reason, or the missing group, is in the detail | Fix what the detail names, then **Retry** on the agent's row. For a token disabled at Cloudflare, choose **Turn back on** or **Keep off** |
| `identity_missing` | For up to about a minute after you first connect, "access migration has not finished": the one-time step every per-agent integration passes through | Nothing; it clears by itself |

An agent's own failures are also reported on that agent's row on the **Agents** tab, with the control that answers them.

If an agent says its tool refuses every call, check the token has no IP address filter (the MCP server refuses one) and that Account Settings Read is on the provisioner, so every level can grant it.
