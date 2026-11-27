# Google Search Console

Search Console lets agents **read and manage your Google Search Console properties through engine tools**, as their own Google identity: search performance, sitemaps, URL inspection, site verification and its owners, and Indexing API notifications. Every method of the Search Console API, the Site Verification API and the Indexing API is reachable but one Google retired; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Each agent signs in as its own Google growth identity, a service account the engine makes in your identity project, and its tokens are minted inside the engine for an hour, with the scope each call needs ([ADR-0172](../../adr/0172-a-google-growth-product-signs-in-as-each-seats-own-growth-identity-never-its-cloud-identity.md)).
- **Two ceilings, both kept.** The permission you give the agent's identity on each property is Google's ceiling; the agent's level, its sites and each method's class are Crewlet's. A call needs both.
- **Adding or removing a property, verifying a site and changing its owners are asked first,** one each, of an administrator at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)).

Nothing here receives events from Search Console.

## What agents get

An agent is on at one level across every site it is granted.

| An agent at | Can | Needs, on the property |
|---|---|---|
| `read` (the default) | Search performance, sitemaps, URL inspection, the properties in its identity's list, verification tokens, verified sites and their owners, and its Indexing API notifications | Restricted user or above. Verified sites and Indexing API notifications need Owner |
| `operate` | Everything `read` can, plus changes made at once and recorded: submitting and removing sitemaps, and Indexing API notifications for job posting and livestream pages | Full user for sitemaps; Owner for the Indexing API |
| `administer` | Everything `operate` can, plus asking an administrator to approve each property added to or removed from its identity's list, site verified, owner added or removed, or ownership relinquished | Owner to change owners; a verified owner to relinquish |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up). An agent holds only its level's tools, and each tool that takes a method's ID says what lies above it. A method above the agent's level, named on a tool it holds, is refused before Search Console is called, naming the method's class, the tool and level that make it, whether an administrator approves each call, and that an administrator raises the level on the Agents tab, asked in words (`request_access` asks for no level). A method Crewlet does not offer is refused as one no level reaches.

The permissions Search Console gives, least first: **Restricted user** (sees performance, sitemaps, URL inspection and every report), **Full user** (adds submitting and removing sitemaps; cannot add users or owners), **Owner** (adds Site Verification, the Indexing API and managing users; verified with its own token, or delegated by another owner). A **verified owner** is an owner whose own token is on the site, the only one who can relinquish its ownership. A property added to an identity's list with no permission reads as **unverified**: it can verify ownership, or take the property off its list.

## Connecting Crewlet

1. Connect [Google Cloud](google-cloud.md) first; no project needs preparing, and the form says whether it is connected. Turn Search Console on: Crewlet turns on the Search Console, Site Verification and Indexing APIs in the identity project and makes the company reader itself, with no block to run ([Google's growth products sign in through this identity](google-cloud.md#googles-growth-products-sign-in-through-this-identity)).
2. Add the company reader's email, shown on the form, to each property as a **Restricted user**: Search Console, Settings, Users and permissions, Add user. Crewlet reads the company's own figures as this identity, never as an agent's.
3. Turn agents on from the Agents tab, each with its sites, and give each its identity ([below](#give-an-agent-its-own-identity)). An agent at `administer` can also verify a property itself and become its owner, once an administrator approves.

## Give an agent its own identity

An agent's identity at Search Console is its own Google growth identity, which Crewlet made; what is left is an owner adding it to each property, since Search Console has no API that adds a user. Search Console keeps no history of who did what, so the agent's own identity is what lets you remove one agent alone. The agent's **Sites** list what the company reader sees, and take a typed property (`sc-domain:` and a domain, or a URL prefix ending in a slash) for one it cannot see yet. A new identity reads **Getting ready** for up to about two minutes, until Google lets a person add it. Then its row reads **Needs you** until the identity reaches each of its properties, each listed in the sheet with a button to its Users and permissions, and **Set up identity** in its box shows the steps, with the email ready to copy:

1. In Search Console, choose the property, then open **Settings**, **Users and permissions**.
2. Press **Add user** and paste the agent's email.
3. Pick the permission its level needs, **Restricted user** at `read`, **Full user** at `operate` and **Owner** at `administer`, and save. The Indexing API needs **Owner** at any level.

**The check, on every pass:** Crewlet signs in as the identity and lists its sites, each with the identity's own permission there: a property not listed, or listed with no permission, is not added yet; one below the level's is the wrong role; Owner on a `read` agent is a warning. The agent is ready once every property passes, and reads Needs you again if somebody removes it there; calls Google refuses meanwhile name what is missing.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `search_console_sites_list` | read | The properties the agent may reach, with its identity's permission on each, and the ones of its sites its identity is not added to yet |
| `search_console_search_analytics` | read | Clicks, impressions, CTR and position on one property, grouped by query, page, country, device, date or search appearance, in Pacific time, with the first incomplete day |
| `search_console_sitemaps_list` | read | A property's sitemaps, or an index file's children, with their errors and warnings |
| `search_console_inspect_url` | read | One page as Google has indexed it: verdict, coverage, crawl, canonicals and rich results |
| `search_console_verification_token` | read | The token that proves the agent's identity owns a property, and where to place it |
| `search_console_sitemap_submit` | operate | Submits a sitemap, or submits it again so Google fetches it again |
| `search_console_index_notify` | operate | Tells Google a job posting or livestream page was updated or removed |
| `search_console_verify_site` | admin | Verifies ownership of a property once its token is placed: asked first |
| `search_console_owners_update` | admin | Adds or removes owners of a verified property: asked first |
| `search_console_read` | read | Any method that only reads, by its ID |
| `search_console_operate` | operate | Any reversible change, by its method's ID |
| `search_console_admin` | admin | Any property, verification or owner change, by its method's ID: asked first |

A call on the wrong tool for its method's class is refused with the tool that may make it.

**What a call may reach:**
- A site is written as Search Console writes it: a URL prefix with its scheme and trailing slash (`https://www.example.com/`) or a domain (`sc-domain:example.com`).
- A site outside the agent's list is refused before Google is asked, and so is a page no site of its list holds: a page a URL prefix starts, or one whose host is a domain or below it.
- A body's site or page is read in either spelling Google takes (`siteUrl` or `site_url`); a body naming one both ways with two values is refused.
- A Site Verification resource is held to the property it verifies: `dns://example.com` is `sc-domain:example.com`, a site's URL its URL prefix. An Android app has no property here, so its verification is refused.
- A listing is cut to the agent's sites.

**The Indexing API's terms.** Google allows the Indexing API only for pages carrying `JobPosting`, or `BroadcastEvent` inside `VideoObject`, structured data in the schema.org vocabulary, and spam use can cost the company its access. Before every notification the engine sends one GET for the page itself through its public-address guard, following no redirect and reading at most 15 MB within 15 seconds, and refuses a notification the page does not allow: an update needs that markup (in JSON-LD, microdata or RDFa, the `BroadcastEvent` nested in the `VideoObject`), a removal a page answering 404 or 410 or carrying `noindex`, and a page that redirects allows neither. Nothing is sent for a refused one.

**How an approved change is made:** the administrator's answer is recorded first, and the change is made only on the call's one answer: a call that lapsed or was denied is never made. Every write holds its site's one lease while the engine checks the agent's sites and the Indexing API's terms, reads the target again and sends it, so two changes to one site never pass on the same reading, and an approved change whose target moved is not made.

**An owner cannot remove itself** through `search_console_owners_update`; relinquishing its own ownership is `siteVerification.webResource.delete` through `search_console_admin`, after its tokens are taken off the site.

**What an agent reads:** every value Google answers with is fenced as somebody else's words, since searchers write the queries a report groups by, pages carry their own URLs, canonicals, item names and the issue text Google quotes, and a verified resource lists its owners' emails. A verification token is shown whole, since the agent places it on the site.

## Configuration

```yaml
integrations:
  search_console:
    enabled: true
    sites: ["https://www.example.com/", "sc-domain:example.com"]   # optional: the properties agents may be given; absent is every one an identity reaches
    approvers: [founder]        # optional: who an admin act is routed to first; only an administrator answers
roles:
  - handle: growth-lead
    integrations:
      search_console: {enabled: true, level: read, sites: ["sc-domain:example.com"]}
```

The block needs `integrations.google_cloud` on with its provisioning block. It holds no secret. A block lists at most 1,000 sites, Search Console's own limit on one account's properties.

## Every method, and its class

Each row is one method the engine offers, with its class, the least permission Google asks of the identity, and the scope minted for it. The table is held to the engine's catalog by a test, both ways.

| Method | Resource | Call | Class | Least role | Scope |
|---|---|---|---|---|---|
| `webmasters.searchanalytics.query` | searchanalytics | `POST /webmasters/v3/sites/{siteUrl}/searchAnalytics/query` | read | Restricted user | webmasters.readonly |
| `webmasters.sitemaps.list` | sitemaps | `GET /webmasters/v3/sites/{siteUrl}/sitemaps` | read | Restricted user | webmasters.readonly |
| `webmasters.sitemaps.get` | sitemaps | `GET /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | read | Restricted user | webmasters.readonly |
| `webmasters.sitemaps.submit` | sitemaps | `PUT /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | operate | Full user | webmasters |
| `webmasters.sitemaps.delete` | sitemaps | `DELETE /webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | operate | Full user | webmasters |
| `webmasters.sites.list` | sites | `GET /webmasters/v3/sites` | read | Any Google account | webmasters.readonly |
| `webmasters.sites.get` | sites | `GET /webmasters/v3/sites/{siteUrl}` | read | Unverified | webmasters.readonly |
| `webmasters.sites.add` | sites | `PUT /webmasters/v3/sites/{siteUrl}` | admin | Any Google account | webmasters |
| `webmasters.sites.delete` | sites | `DELETE /webmasters/v3/sites/{siteUrl}` | admin | Unverified | webmasters |
| `searchconsole.urlInspection.index.inspect` | urlInspection.index | `POST /v1/urlInspection/index:inspect` | read | Restricted user | webmasters.readonly |
| `searchconsole.urlTestingTools.mobileFriendlyTest.run` | urlTestingTools.mobileFriendlyTest | `POST /v1/urlTestingTools/mobileFriendlyTest:run` | never | none | none |
| `siteVerification.webResource.getToken` | webResource | `POST /siteVerification/v1/token` | read | Any Google account | siteverification.verify_only |
| `siteVerification.webResource.insert` | webResource | `POST /siteVerification/v1/webResource` | admin | Any Google account | siteverification.verify_only |
| `siteVerification.webResource.list` | webResource | `GET /siteVerification/v1/webResource` | read | Owner | siteverification |
| `siteVerification.webResource.get` | webResource | `GET /siteVerification/v1/webResource/{id}` | read | Owner | siteverification |
| `siteVerification.webResource.update` | webResource | `PUT /siteVerification/v1/webResource/{id}` | admin | Owner | siteverification |
| `siteVerification.webResource.patch` | webResource | `PATCH /siteVerification/v1/webResource/{id}` | admin | Owner | siteverification |
| `siteVerification.webResource.delete` | webResource | `DELETE /siteVerification/v1/webResource/{id}` | admin | Verified owner | siteverification |
| `indexing.urlNotifications.publish` | urlNotifications | `POST /v3/urlNotifications:publish` | operate | Owner | indexing |
| `indexing.urlNotifications.getMetadata` | urlNotifications | `GET /v3/urlNotifications/metadata` | read | Owner | indexing |

Hosts: `searchconsole.googleapis.com` for the Search Console API, `www.googleapis.com` for Site Verification, `indexing.googleapis.com` for the Indexing API. Scopes drop their `https://www.googleapis.com/auth/` prefix.

**Notes on the classing:**
- `sitemaps.delete` is `operate`: it takes the sitemap off the Sitemaps report only, Google keeps crawling it, and submitting it again restores it.
- `sites.delete` is `admin` and marked irreversible: a user's access goes with it, and only an owner can give it back.
- `webResource.insert` is `admin`: verifying makes the identity an owner of the property in every Google product that reads verification. Adding an owner grants full control and removing one revokes it, so `update` and `patch` are `admin` either way. `delete` relinquishes the identity's own ownership.
- `indexing.urlNotifications.publish` is `operate` for a page Google's terms allow, and refused for any other ([the Indexing API's terms](#the-agents-tools)).
- Site Verification has no read-only scope and the Indexing API one scope, so their reads are minted with those; the token is spent on that read alone.
- `urlTestingTools.mobileFriendlyTest.run` is never offered: Google retired the Mobile-Friendly Test on December 1, 2023, and the method answers `403`.

## What the atlas shows

Each site the company narrows agents to (`integrations.search_console.sites`) is an account with the `analytics` capability: a domain property keyed as Search Console writes it (`sc-domain:example.com`), and each URL prefix its own account, keyed by its host and a short digest of the site, with the site itself shown as the account's site. A company that names none shows Search Console as one account ([the atlas](../concepts/atlas.md)).

## Search Console's limits worth knowing

- Search Analytics allows 1,200 queries a minute per site and per user, and has a load quota: past it, Google asks for 15 minutes' wait. Queries grouped by page and query over long ranges cost the most.
- URL Inspection allows 2,000 inspections a day and 600 a minute per site, reset at midnight Pacific.
- The Indexing API allows 200 notifications a day per identity project, updates and removals together, reset at midnight Pacific; a refusal says so. Its metadata answers only for a page notified before.
- Search Analytics data is final two to three days after the day; `data_state: all` includes fresher figures, and the answer names the first incomplete day.
- Users other than owners can only be added in Search Console itself; it has no API for them.
- Features with no API, so no tool: removals, requesting indexing from URL inspection, the live URL test, the page indexing, Core Web Vitals and rich result reports, links, manual actions, security issues, disavow, change of address and crawl settings.
