# AdSense

AdSense lets agents **read and change your websites' AdSense monetization through engine tools**: accounts, sites, ad clients, ad units, custom and URL channels, alerts, policy issues, payments, ad code, saved and custom reports, and for AdSense for Platforms partners their sub-accounts, sites, events, platforms and revenue groups. Every method of the AdSense Management API (v2) and the AdSense Platform API (v1 and v1alpha) is reachable; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Google refuses service accounts at AdSense, so a person who manages your AdSense account signs in with Google once, as your own OAuth client. Crewlet keeps that sign-in sealed, renews it, and makes every agent's call through it ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md), [ADR-0178](../../adr/0178-an-integrations-consent-grant-is-a-named-grant-of-the-same-machinery.md)).
- **Crewlet holds each agent to its level and accounts.** One sign-in serves every agent, so nothing at Google tells two agents apart: an agent's level and its publisher accounts are Crewlet's to enforce, and a call outside them is refused before Google is asked.
- **What visitors see, and what cannot be undone, is asked first,** one change at a time, of an administrator at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). Nothing in AdSense spends money: AdSense pays you.

Nothing here receives events from AdSense.

## What agents get

An agent is on at one level across every publisher account it is granted.

| An agent at | Can |
|---|---|
| `read` (the default) | Accounts, child accounts, sites, ad clients, ad units, custom and URL channels, alerts, policy issues, payments, ad code and the ad blocking recovery tag, custom, saved and CSV reports, and the Platform API's reads |
| `operate` | Everything `read` can, plus changes made at once and recorded: new display ad units and custom channels, renaming them, archiving or reactivating a unit, turning a channel's data collection on or off, and a platform group's description. It also asks an administrator to approve each change visitors see: a live ad unit's size or type, and asking Google to review a platform site |
| `administer` | Everything `operate` can, plus asking an administrator to approve each change that cannot be undone or sets up accounts: deleting a custom channel, creating or closing a platform's sub-account, adding or removing its sites, recording a sign-up or log-in event, and moving a site to another revenue group |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up).

### The roles AdSense gives

AdSense reports no role through its API, and both of its roles reach every method; Crewlet turns a refusal into the scope, the enabling or the access to ask for.

| Role | What it reaches through the API |
|---|---|
| Administrator | Every method. The administrator tasks (users, terms, payment settings, closing the account) have no API method. |
| Standard user | Every method, payments among them. |
| AdMob Administrator, on a linked account | Administrator on the linked AdSense account. |
| AdMob Manager, on a linked account | Standard user on the linked AdSense account. |
| AdMob custom role | No AdSense access. |

### Methods Google enables per project

Google answers five Management API writes (creating and changing ad units, and creating, changing and deleting custom channels) and every Platform API method only for Google Cloud projects it enables, usually AdSense for Platforms partners', which the account manager asks for. The Platform API also answers only a platform's parent account, and Google turns it on for the project. Crewlet offers them all the same, marked in the table below, and its answer to Google's 403 says so. The Platform API is called as the company's sign-in, so the person who signs in manages the platform's AdSense account.

## Connecting Crewlet

1. In a Google Cloud project you control, turn on the AdSense Management API and make the company's OAuth client ([how](google-cloud.md#the-companys-own-oauth-client)): a **Web application** client whose one authorized redirect URI is `<api.public.external_url>/webhooks/mcp-oauth`, exactly as the form shows it, with no JavaScript origins, and an Internal audience or an External one published or listing whoever signs in as a test user. The engine needs a public address a browser reaches, since Google sends the person back to it.
2. Paste the client's ID and secret on the AdSense page. The secret is sealed and shown back as `__redacted__`. AdMob may sign in as the same client and name the same secret.
3. Sign in with Google there, as an Administrator or Standard user of the AdSense account. While every agent reads, the sign-in asks for `adsense.readonly`; once an agent is raised to `operate`, sign in again to add `adsense`, which every change needs.
4. Turn agents on from the Agents tab, each with its publisher accounts.

## Give an agent its own identity

Not offered: every agent's calls go as the company's consent grant. AdSense refuses service accounts, so an identity per agent would be a Google Account per agent, which Google Workspace's acceptable use policy forbids for accounts assigned to a function rather than a person. On the **Agents** tab the company's identity is the normal state, In sync, with nothing to set up ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)); the access log names the agent for each call.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `adsense_accounts_list` | read | The agent's publisher accounts with each one's name, state, time zone and sign-up tasks left, and any the sign-in cannot reach |
| `adsense_ad_clients_list` | read | An account's ad clients: product and state |
| `adsense_ad_units_list` | read | An ad client's ad units: name, state, size and type |
| `adsense_custom_channels_list` | read | An ad client's custom channels |
| `adsense_sites_list` | read | An account's sites: domain, state and auto ads |
| `adsense_alerts_list` | read | An account's alerts with their severity |
| `adsense_policy_issues_list` | read | Policy issues with Google's action and the date it escalates |
| `adsense_payments_list` | read | Unpaid earnings and past payments |
| `adsense_ad_code` | read | An ad client's Auto ads code, or one ad unit's code |
| `adsense_report` | read | A report over your dimensions, metrics, filters and sorting |
| `adsense_saved_report` | read | The saved reports, or one of them run over your dates |
| `adsense_ad_unit_upsert` | operate | Creates a display ad unit, or renames, archives or reactivates one |
| `adsense_custom_channel_upsert` | operate | Creates a custom channel, or renames one or turns its data collection on or off |
| `adsense_read` | read | Any method that only reads, by its ID |
| `adsense_operate` | operate | Any change made at once, by its method's ID |
| `adsense_publish` | public | A live ad unit's size or type, or a platform site's review: asked first |
| `adsense_admin` | admin | Deleting a custom channel, a platform's sub-accounts, their sites and events, and moving a site to another revenue group: asked first |

A change is classed per call against its target, read first: an ad unit's name and state are `operate` (an archived unit still serves; archiving only hides it in AdSense), and its size or type is `public` when it differs from the live unit's, whether the update mask names `contentAdsSettings` whole or one of its fields. A change names its fields in `updateMask`, each a path Google documents for the method, in snake_case or camelCase: an ad unit's `display_name`, `state` and `content_ads_settings` (or `.size`, `.type`), a custom channel's `display_name` and `active`, a platform group's `description`, and a platform child site's `platform_group`. A change without one, any other path, a body field spelled as its proto name (`content_ads_settings`), a query parameter the method does not take (`update_mask`) or given twice (only a report's `dimensions`, `metrics`, `filters` and `orderBy` repeat), a body naming another resource, and a site moved to another platform's group are refused before Google is asked, since Google could read each of them where the engine's class would not. A call on the wrong tool for what it changes is refused with the tool that may make it. A held change binds to what it acts on as a person saw it, and sends nothing once that moved: the resource it changes, the sub-account a platform site or event is made in, and for a site moved to another revenue group, both groups' revenue shares, which the person is asked as the share before and after.

**What a call may reach:**
- An account outside the agent's list is refused before Google is asked. On the Platform API's `platforms/{platformsId}` methods, the account is the platform's own publisher ID, and the sub-account under it is the platform's.
- Every publisher ID a call names, in its path or body (an ad client, a resource name, a platform group), is its account's or the sub-account its path names.
- The account listing is cut to the agent's accounts.

**What an agent reads:** every value Google answers with is fenced as somebody else's words: account, ad unit, channel and saved report names are typed by your account's users, site domains and policy texts name pages, alert messages may carry HTML, and ad code is HTML and JavaScript to place, never to run. A platform event's email and billing address are a third party's personal data: the question an administrator is asked and the access log keep their digest alone.

**Reports:** a report's days are cut in the account's time zone, or in `America/Los_Angeles`, Google's, the one other zone AdSense reports in, and reach back at most three years. Its dimensions, metrics, filters (`DIMENSION==value`, `!=`, `=@` contains, `!@` lacks; values joined by a comma match any) and sorting are held to the API's lists before Google is asked. Money is a decimal string of its column's currency, never summed across currencies. An answer carries at most 2,000 rows and says it is cut when Google had more (the engine asks for one row past the limit), with Google's total; earnings are marked estimated, since Google restates recent days; and Google's warnings are passed through. A CSV report (`generateCsv`) is answered as its type and text, and one larger than a call reads (8 MiB) is refused, saying to narrow it.

## Configuration

```yaml
integrations:
  adsense:
    enabled: true
    oauth:
      client_id: "123456789012-abc.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}   # sealed; AdMob may name the same variable
    accounts: ["pub-1234567890123456"]               # optional: the accounts agents may be given; absent is every one the sign-in reaches
    approvers: [founder]                             # optional: who a public or admin act is routed to first; only an administrator answers
roles:
  - handle: finance
    integrations:
      adsense: {enabled: true, level: read, accounts: ["pub-1234567890123456"]}
```

The block needs no Google Cloud connection: it signs in as its own OAuth client. The refresh token is never in the document. `adsense_accounts_list` reads each of the agent's accounts by its ID at the call, so an account the sign-in no longer reaches is named rather than left out, and a state is the one Google answers now.

## Every method, and its class

Each row is one method the engine offers, with its call, its class and the scope it needs. The table is held to the engine's catalog by a test, both ways. The Platform API's eleven stable methods are called on v1, its eight others on v1alpha.

| Method | Call | Class | Scope and notes |
|---|---|---|---|
| `adsense.accounts.list` | `GET /v2/accounts` | read | adsense.readonly |
| `adsense.accounts.get` | `GET /v2/accounts/{accountsId}` | read | adsense.readonly |
| `adsense.accounts.listChildAccounts` | `GET /v2/accounts/{accountsId}:listChildAccounts` | read | adsense.readonly |
| `adsense.accounts.getAdBlockingRecoveryTag` | `GET /v2/accounts/{accountsId}/adBlockingRecoveryTag` | read | adsense.readonly |
| `adsense.accounts.adclients.list` | `GET /v2/accounts/{accountsId}/adclients` | read | adsense.readonly |
| `adsense.accounts.adclients.get` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}` | read | adsense.readonly |
| `adsense.accounts.adclients.getAdcode` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/adcode` | read | adsense.readonly |
| `adsense.accounts.adclients.adunits.list` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits` | read | adsense.readonly |
| `adsense.accounts.adclients.adunits.get` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits/{adunitsId}` | read | adsense.readonly |
| `adsense.accounts.adclients.adunits.getAdcode` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits/{adunitsId}/adcode` | read | adsense.readonly |
| `adsense.accounts.adclients.adunits.listLinkedCustomChannels` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits/{adunitsId}:listLinkedCustomChannels` | read | adsense.readonly |
| `adsense.accounts.adclients.adunits.create` | `POST /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits` | operate | adsense; enabled projects only |
| `adsense.accounts.adclients.adunits.patch` | `PATCH /v2/accounts/{accountsId}/adclients/{adclientsId}/adunits/{adunitsId}` | operate, public by field | adsense; enabled projects only |
| `adsense.accounts.adclients.customchannels.list` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels` | read | adsense.readonly |
| `adsense.accounts.adclients.customchannels.get` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels/{customchannelsId}` | read | adsense.readonly |
| `adsense.accounts.adclients.customchannels.listLinkedAdUnits` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels/{customchannelsId}:listLinkedAdUnits` | read | adsense.readonly |
| `adsense.accounts.adclients.customchannels.create` | `POST /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels` | operate | adsense; enabled projects only |
| `adsense.accounts.adclients.customchannels.patch` | `PATCH /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels/{customchannelsId}` | operate | adsense; enabled projects only |
| `adsense.accounts.adclients.customchannels.delete` | `DELETE /v2/accounts/{accountsId}/adclients/{adclientsId}/customchannels/{customchannelsId}` | admin | adsense; enabled projects only; cannot be undone |
| `adsense.accounts.adclients.urlchannels.list` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/urlchannels` | read | adsense.readonly |
| `adsense.accounts.adclients.urlchannels.get` | `GET /v2/accounts/{accountsId}/adclients/{adclientsId}/urlchannels/{urlchannelsId}` | read | adsense.readonly |
| `adsense.accounts.alerts.list` | `GET /v2/accounts/{accountsId}/alerts` | read | adsense.readonly |
| `adsense.accounts.payments.list` | `GET /v2/accounts/{accountsId}/payments` | read | adsense.readonly |
| `adsense.accounts.policyIssues.list` | `GET /v2/accounts/{accountsId}/policyIssues` | read | adsense.readonly |
| `adsense.accounts.policyIssues.get` | `GET /v2/accounts/{accountsId}/policyIssues/{policyIssuesId}` | read | adsense.readonly |
| `adsense.accounts.reports.generate` | `GET /v2/accounts/{accountsId}/reports:generate` | read | adsense.readonly |
| `adsense.accounts.reports.generateCsv` | `GET /v2/accounts/{accountsId}/reports:generateCsv` | read | adsense.readonly |
| `adsense.accounts.reports.getSaved` | `GET /v2/accounts/{accountsId}/reports/{reportsId}/saved` | read | adsense.readonly |
| `adsense.accounts.reports.saved.list` | `GET /v2/accounts/{accountsId}/reports/saved` | read | adsense.readonly |
| `adsense.accounts.reports.saved.generate` | `GET /v2/accounts/{accountsId}/reports/{reportsId}/saved:generate` | read | adsense.readonly |
| `adsense.accounts.reports.saved.generateCsv` | `GET /v2/accounts/{accountsId}/reports/{reportsId}/saved:generateCsv` | read | adsense.readonly |
| `adsense.accounts.sites.list` | `GET /v2/accounts/{accountsId}/sites` | read | adsense.readonly |
| `adsense.accounts.sites.get` | `GET /v2/accounts/{accountsId}/sites/{sitesId}` | read | adsense.readonly |
| `adsenseplatform.platforms.accounts.list` | `GET /v1/platforms/{platformsId}/accounts` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.get` | `GET /v1/platforms/{platformsId}/accounts/{accountsId}` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.lookup` | `GET /v1/platforms/{platformsId}/accounts:lookup` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.create` | `POST /v1/platforms/{platformsId}/accounts` | admin | adsense; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.close` | `POST /v1/platforms/{platformsId}/accounts/{accountsId}:close` | admin | adsense; AdSense for Platforms; enabled projects only; cannot be undone |
| `adsenseplatform.platforms.accounts.events.create` | `POST /v1/platforms/{platformsId}/accounts/{accountsId}/events` | admin | adsense; AdSense for Platforms; enabled projects only; cannot be undone |
| `adsenseplatform.platforms.accounts.sites.list` | `GET /v1/platforms/{platformsId}/accounts/{accountsId}/sites` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.sites.get` | `GET /v1/platforms/{platformsId}/accounts/{accountsId}/sites/{sitesId}` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.sites.create` | `POST /v1/platforms/{platformsId}/accounts/{accountsId}/sites` | admin | adsense; AdSense for Platforms; enabled projects only |
| `adsenseplatform.platforms.accounts.sites.delete` | `DELETE /v1/platforms/{platformsId}/accounts/{accountsId}/sites/{sitesId}` | admin | adsense; AdSense for Platforms; enabled projects only; cannot be undone |
| `adsenseplatform.platforms.accounts.sites.requestReview` | `POST /v1/platforms/{platformsId}/accounts/{accountsId}/sites/{sitesId}:requestReview` | public | adsense; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.list` | `GET /v1alpha/accounts/{accountsId}/platforms` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.get` | `GET /v1alpha/accounts/{accountsId}/platforms/{platformsId}` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.groups.list` | `GET /v1alpha/accounts/{accountsId}/platforms/{platformsId}/groups` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.groups.get` | `GET /v1alpha/accounts/{accountsId}/platforms/{platformsId}/groups/{groupsId}` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.groups.patch` | `PATCH /v1alpha/accounts/{accountsId}/platforms/{platformsId}/groups/{groupsId}` | operate | adsense; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.childAccounts.sites.list` | `GET /v1alpha/accounts/{accountsId}/platforms/{platformsId}/childAccounts/{childAccountsId}/sites` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.childAccounts.sites.get` | `GET /v1alpha/accounts/{accountsId}/platforms/{platformsId}/childAccounts/{childAccountsId}/sites/{sitesId}` | read | adsense.readonly; AdSense for Platforms; enabled projects only |
| `adsenseplatform.accounts.platforms.childAccounts.sites.patch` | `PATCH /v1alpha/accounts/{accountsId}/platforms/{platformsId}/childAccounts/{childAccountsId}/sites/{sitesId}` | admin | adsense; AdSense for Platforms; enabled projects only |

**Notes on the classing:**
- No method spends money: AdSense pays the publisher.
- A live ad unit's size or type changes the ad slot every visitor of a page carrying its code sees, so it is `public`; a new unit shows nothing until a page carries its code, so creating one is `operate`.
- Asking Google to review a platform site starts ads on it once approved, so it is `public`.
- Deleting a custom channel cannot be undone (a channel made again gets a new reporting ID), and a closed sub-account, a removed site and a sent event cannot be taken back, so each is `admin` and asks to confirm. Creating a sub-account or its site, and moving a site to another revenue group (which changes the revenue share), set up accounts and money, so they are `admin` too.
- A platform group's description is a label, so changing it is `operate`.

**What no tool reaches, because AdSense offers no API for it:** users and access levels, terms, closing a non-platform account, payment settings, site and URL channel changes, and saved report definitions. A person does these in AdSense.

## What the atlas shows

Each publisher account the company narrows agents to (`integrations.adsense.accounts`) is an account with the `monetization` capability, keyed by its ID; a company that names none shows AdSense as one account ([the atlas](../concepts/atlas.md)).

## AdSense's limits worth knowing

- Google allows 100 requests a minute per user, 500 a minute per project and 10,000 a day per project, resetting around midnight Pacific time, and a separate, unpublished quota of report rows. A read refused for quota is waited out briefly; a change never is. The answer says when quotas free up.
- A report holds at most 100,000 rows at Google (a CSV report 1,000,000), and data three years back.
- Recent earnings are estimates Google restates for a few days.
