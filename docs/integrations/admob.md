# AdMob

AdMob lets agents **read and change your apps' AdMob monetization through engine tools**: accounts, apps, ad units, ad unit mappings, ad sources, mediation groups, mediation A/B experiments, and the network, mediation and campaign reports. Every method of the AdMob API, v1 and v1beta, is reachable; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Google refuses service accounts at AdMob, so a person who manages your AdMob account signs in with Google once, as your own OAuth client. Crewlet keeps that sign-in sealed, renews it, and makes every agent's call through it ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md), [ADR-0178](../../adr/0178-an-integrations-consent-grant-is-a-named-grant-of-the-same-machinery.md)).
- **Crewlet holds each agent to its level and accounts.** One sign-in serves every agent, so nothing at Google tells two agents apart: an agent's level and its publisher accounts are Crewlet's to enforce, and a call outside them is refused before Google is asked.
- **What cannot be undone is asked first,** one change at a time, of an administrator at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). Nothing in AdMob spends money or publishes: AdMob pays you.

Nothing here receives events from AdMob.

## What agents get

An agent is on at one level across every publisher account it is granted.

| An agent at | Can | Needs, of the person who signed in |
|---|---|---|
| `read` (the default) | Accounts, apps, ad units, ad unit mappings, ad sources and their adapters, mediation groups, and the network, mediation and campaign reports | Any role reads accounts and inventory; the reports need Reports and performance data at View |
| `operate` | Everything `read` can, plus changes made at once and recorded: new apps by name, ad units, ad unit mappings, mediation groups and their lines (ad source, eCPM, mode, enabled or disabled, ad units), and starting or stopping a mediation A/B experiment | Inventory management at View and manage, or Manager; and the sign-in made again to allow changes |
| `administer` | Everything `operate` can, plus asking an administrator to approve each change that cannot be undone: linking a new app to its store listing, removing a mediation line, stopping an A/B experiment keeping the treatment's lines | The same as `operate` |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up).

### The roles AdMob gives

AdMob reports no role through its API: the role of the person who signed in decides what Google answers, and Crewlet turns a refusal into the role, scope or allowlisting to ask for.

| Role | What it reaches through the API |
|---|---|
| Administrator | Every method. The administrator tasks (users, terms, MCM relationships, closing the account) have no API method. |
| Manager | Every method. |
| Custom role: Inventory management, View | Apps, ad units, mappings, ad sources and mediation groups, read only. Every custom role holds at least this. |
| Custom role: Inventory management, View and manage | Also every change. |
| Custom role: Reports and performance data, View (or View and manage) | The network, mediation and campaign reports. |
| Custom role: Account management, View and manage | Nothing more: blocking controls, linked services, privacy and messaging have no API method. |
| Custom role: Payments, View and manage | Nothing more: payments have no API method. |
| Custom role limited to some apps | Lists and reports show only its apps, and changes to other apps are refused. |

AdMob has no Analyst or Reporting role; give such a person a custom role with Reports and performance data at View.

### Limited access

Google calls ten methods limited access (marked in the table below): they answer 403 until Google allows the account, which its AdMob account manager asks for. Crewlet offers them all the same, and its answer to that 403 says so.

## Connecting Crewlet

1. In a Google Cloud project you control, turn on the AdMob API and make the company's OAuth client ([how](google-cloud.md#the-companys-own-oauth-client)): a **Web application** client whose one authorized redirect URI is `<api.public.external_url>/webhooks/mcp-oauth`, exactly as the form shows it, with no JavaScript origins, and an Internal audience or an External one published or listing whoever signs in as a test user. The engine needs a public address a browser reaches, since Google sends the person back to it.
2. Paste the client's ID and secret on the AdMob page. The secret is sealed and shown back as `__redacted__`. AdSense may sign in as the same client and name the same secret.
3. Sign in with Google there, as a person who manages the AdMob account. While every agent reads, the sign-in asks for `admob.readonly` and `admob.report`; once an agent is raised to `operate`, sign in again to add `admob.monetization`, which every change needs.
4. Turn agents on from the Agents tab, each with its publisher accounts: the picker lists the publisher accounts the sign-in reaches, and takes a typed publisher ID (`pub-` and 16 digits) for one it does not.

## Give an agent its own identity

Not offered: every agent's calls go as the company's consent grant. AdMob refuses service accounts, so an identity per agent would be a Google Account per agent, which Google Workspace's acceptable use policy forbids for accounts assigned to a function rather than a person. On the **Agents** tab the company's identity is the normal state, In sync, with nothing to set up ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)); AdMob's change history names the signing person, and the access log names the agent for each call.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `admob_accounts_list` | read | The agent's publisher accounts with each one's currency and reporting time zone, and any the sign-in cannot reach |
| `admob_apps_list` | read | An account's apps: platform, store link or manual name, approval state |
| `admob_ad_units_list` | read | An account's ad units: app, format, media types, reward settings |
| `admob_mediation_groups_list` | read | An account's mediation groups, filtered as Google's filter takes it |
| `admob_network_report` | read | The AdMob Network report: earnings, requests, impressions, clicks and rates by your dimensions |
| `admob_mediation_report` | read | The mediation report: every ad source's earnings, eCPM and latency |
| `admob_campaign_report` | read | House and cross-promotion campaigns over at most 30 days |
| `admob_app_create` | operate | Adds an app by the name you give it, not linked to a store |
| `admob_ad_unit_create` | operate | Creates an ad unit for an app |
| `admob_ad_unit_mapping_create` | operate | Maps ad units to an ad source's adapter, one or up to 100 at once |
| `admob_mediation_group_upsert` | operate | Creates a mediation group, or changes one by its update mask |
| `admob_ab_experiment` | operate | Starts a mediation A/B experiment, or stops the running one keeping the control |
| `admob_read` | read | Any method that only reads, by its ID |
| `admob_operate` | operate | Any change, by its method's ID |
| `admob_admin` | admin | Linking a new app to its store listing, removing a mediation line, or keeping an experiment's treatment: asked first |

A change is classed per call from what it sets: creating an app is `operate` and setting its `linkedAppInfo.appStoreId` is `admin`, since Google says a store link cannot be undone; a mediation line's state set to `ENABLED` or `DISABLED` is `operate`, and `REMOVED`, or any other state, is `admin`, whether the update mask names the line's state, the whole line (a field it leaves out is cleared), or a line the body leaves out; stopping an experiment keeping the treatment (`VARIANT_CHOICE_B`) replaces the group's lines with the treatment's, so it is `admin` too. A change names its fields in `updateMask`, each a path Google documents: a line (`mediation_group_lines["<line id>"]`), one of its fields (`.state`, `.cpm_micros`, `.cpm_mode`, `.display_name`) or ad unit mappings (`.ad_unit_mappings["<ad unit id>"]`), or the group's ad units (`targeting.ad_unit_ids`). A change without one, any other path, a line whose `id` is not its key, a line's `experimentVariant`, a body naming another group, a body field spelled as its proto name (`linked_app_info`) and a query parameter the method does not take (`update_mask`) are refused before Google is asked, since Google could read each of them where the engine's class would not. A call on the wrong tool for what it changes is refused with the tool that may make it.

**What a call may reach:**
- An account outside the agent's list is refused before Google is asked, and so is a batch item naming another account's ad unit.
- The account listing is cut to the agent's accounts.

**What an agent reads:** an app's approval state (`ACTION_REQUIRED`, `IN_REVIEW`, `APPROVED`) and its store link are in the app listing; an app not yet approved serves no ads, and its fix is in AdMob. Every value Google answers with is fenced as somebody else's words: app, ad unit, mediation group and line names are typed by your account's users, store listing names by whoever owns the listing, and ad source, adapter and campaign names by third parties. A report's dimension that carries a name has a `_NAME` column beside its ID, fenced. An ad unit mapping's configuration values are a third-party network's identifiers, some of them keys, so they are withheld from every answer and the access log keeps their digest.

**Reports:** a report's days are cut in the account's reporting time zone, or in `America/Los_Angeles`, the one other zone Google accepts. Money is a decimal string of the account's currency (or the one asked for), never summed across currencies. An answer carries at most 2,000 rows and says it is cut when Google had more (the engine asks for one row past the limit), passing Google's own row count on as an estimate, which Google says may be off; a campaign report, which Google caps at no row count, is refused with a remedy when it is larger than one answer holds; earnings are marked estimated, since Google restates recent days; and Google's warnings (data delayed, a zone or currency changed) are passed through.

## Configuration

```yaml
integrations:
  admob:
    enabled: true
    oauth:
      client_id: "123456789012-abc.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}   # sealed; AdSense may name the same variable
    accounts: ["pub-1234567890123456"]               # optional: the accounts agents may be given; absent is every one the sign-in reaches
    approvers: [founder]                             # optional: who an admin act is routed to first; only an administrator answers
roles:
  - handle: finance
    integrations:
      admob: {enabled: true, level: read, accounts: ["pub-1234567890123456"]}
```

The block needs no Google Cloud connection: it signs in as its own OAuth client. The refresh token is never in the document. A company whose AdMob accounts are reached by different Google accounts connects the accounts one person reaches; `accounts.list` names only the account last opened in AdMob, so each agent's accounts are read by their IDs.

## Every method, and its class

Each row is one method the engine offers, with its class, the least role Google asks of the person who signed in, and the scope it needs. The table is held to the engine's catalog by a test, both ways. The six stable reads are called on v1, every other method on v1beta.

| Method | Resource | Call | Class | Least role | Scope |
|---|---|---|---|---|---|
| `admob.accounts.list` | accounts | `GET /v1/accounts` | read | any | admob.readonly or admob.report |
| `admob.accounts.get` | accounts | `GET /v1/accounts/{accountsId}` | read | any | admob.readonly or admob.report |
| `admob.accounts.apps.list` | accounts.apps | `GET /v1/accounts/{accountsId}/apps` | read | Inventory management, View | admob.readonly |
| `admob.accounts.apps.create` | accounts.apps | `POST /v1beta/accounts/{accountsId}/apps` | operate, admin by field | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.adUnits.list` | accounts.adUnits | `GET /v1/accounts/{accountsId}/adUnits` | read | Inventory management, View | admob.readonly |
| `admob.accounts.adUnits.create` | accounts.adUnits | `POST /v1beta/accounts/{accountsId}/adUnits` | operate | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.adUnits.adUnitMappings.list` | accounts.adUnits.adUnitMappings | `GET /v1beta/accounts/{accountsId}/adUnits/{adUnitsId}/adUnitMappings` | read | Inventory management, View | admob.readonly (limited access) |
| `admob.accounts.adUnits.adUnitMappings.create` | accounts.adUnits.adUnitMappings | `POST /v1beta/accounts/{accountsId}/adUnits/{adUnitsId}/adUnitMappings` | operate | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.adUnitMappings.batchCreate` | accounts.adUnitMappings | `POST /v1beta/accounts/{accountsId}/adUnitMappings:batchCreate` | operate | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.adSources.list` | accounts.adSources | `GET /v1beta/accounts/{accountsId}/adSources` | read | Inventory management, View | admob.readonly or admob.report |
| `admob.accounts.adSources.adapters.list` | accounts.adSources.adapters | `GET /v1beta/accounts/{accountsId}/adSources/{adSourcesId}/adapters` | read | Inventory management, View | admob.readonly |
| `admob.accounts.mediationGroups.list` | accounts.mediationGroups | `GET /v1beta/accounts/{accountsId}/mediationGroups` | read | Inventory management, View | admob.readonly (limited access) |
| `admob.accounts.mediationGroups.create` | accounts.mediationGroups | `POST /v1beta/accounts/{accountsId}/mediationGroups` | operate | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.mediationGroups.patch` | accounts.mediationGroups | `PATCH /v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}` | operate, admin by field | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.mediationGroups.mediationAbExperiments.create` | accounts.mediationGroups.mediationAbExperiments | `POST /v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}/mediationAbExperiments` | operate | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.mediationGroups.mediationAbExperiments.stop` | accounts.mediationGroups.mediationAbExperiments | `POST /v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}/mediationAbExperiments:stop` | operate, admin by field | Inventory management, View and manage | admob.monetization (limited access) |
| `admob.accounts.networkReport.generate` | accounts.networkReport | `POST /v1/accounts/{accountsId}/networkReport:generate` | read | Reports and performance data, View | admob.readonly or admob.report |
| `admob.accounts.mediationReport.generate` | accounts.mediationReport | `POST /v1/accounts/{accountsId}/mediationReport:generate` | read | Reports and performance data, View | admob.readonly or admob.report |
| `admob.accounts.campaignReport.generate` | accounts.campaignReport | `POST /v1beta/accounts/{accountsId}/campaignReport:generate` | read | Reports and performance data, View | admob.readonly or admob.report |

**Notes on the classing:**
- No method spends money or publishes: AdMob pays the publisher, and every write configures monetization. Enabling a mediation line or raising its eCPM changes which ad source serves, not money paid out, so it is `operate`.
- No method deletes. An app, an ad unit or a mapping created through the API stays; ad units are archived in AdMob, permanently.
- Stopping an A/B experiment keeping the control changes no line, so it is `operate`; keeping the treatment removes every control line the treatment lacks, so it is `admin`. The API offers no read of an experiment before it stops, so its leader (`variantLeader`) is in the stop's answer, not before it.

**What no tool reaches, because AdMob offers no API for it:** users and roles, MCM relationships, terms, closing the account, payments, blocking controls, privacy and messaging, app-ads.txt, archiving apps and ad units, and managing campaigns. A person does these in AdMob.

## What the atlas shows

Each publisher account the company narrows agents to (`integrations.admob.accounts`) is an account with the `monetization` capability, keyed by its ID; a company that names none shows AdMob as one account ([the atlas](../concepts/atlas.md)).

## AdMob's limits worth knowing

- Google allows 900 account and report requests a minute per project, and 120 inventory reads a minute (172,800 a day). A read refused for quota is waited out briefly; a change never is. The answer says when quotas free up.
- A report holds at most 100,000 rows at Google; a campaign report spans at most 30 days.
- Recent earnings are estimates Google restates for a few days.
