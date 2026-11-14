# ADR-0138: Atlassian is read under the organization key, and a product is what its account does

- **Status:** accepted
- **Authority:** `internal/atlassian`
- **Enforced-by:** `internal/atlassian.TestTheAtlasReadsTheOrganizationsSitesAndProducts`, `internal/atlassian.TestTheAtlassianReadAndTheSettingsNameTheSameAccounts`, `internal/atlassian.TestAListingAtlassianRefusesIsAFindingAndRetiresNothing`, `internal/atlassian.TestAnAtlassianSiteTwoReadsMissIsRetired`, `internal/atlassian.TestNoCredentialShapeReachesTheAtlasFromAtlassian`, `internal/atlassian.TestAListingThatFailsOrIsRateLimitedFailsTheAtlassianRead`, `internal/atlassian.TestTheAtlassianListingIsWalkedToItsEnd`
- **Cost-when-tried:** not tried. The reconcile loop already lists an organization's workspaces on every pass and keeps one site of the answer. The atlas keeps the rest, which is every Atlassian product the company has rather than the two the engine works in.
- **Tag-status:** unreleased

## The decision

**The organization is read under the organization API key the company already gave the engine**, with the one listing the reconcile loop already makes (`POST /admin/v2/orgs/{org}/workspaces`, a query Atlassian models as a `POST`, which changes nothing). It takes no new key, scope or setting. A company on Data Center has no organization, and is not read.

**A site is a vendor account, keyed as the settings key it.** A workspace on a site names the site's cloud id in its identifier and the site's address beside it. The site is an entry of kind `vendor_account` keyed `atlassian/` and the address's host in lowercase, which is the key the settings give the site Jira or Confluence works in, so the account a company connected and the one the listing reports are one entry. A site whose row carries no address is keyed by its cloud id, as the settings key one whose address they do not carry. The read says the site's vendor and cloud id. It names a site, and says its address, only where the settings name no site at that host: two names for one account would be a disagreement for a person to settle. An address is kept as its scheme and host alone.

**A product is what its account does, never an entry.** Each workspace is read as one of the atlas's capabilities through a fixed table, by the first word of the product's key: every Jira is `tracker`, Confluence `knowledge`, Bitbucket `code_hosting`, Opsgenie `alerting`. A product on a site is a capability of that site's account, and one on no site (its identifier names no site) a capability of the organization's. A product the table does not hold says nothing, and a product's key or name is never a value.

**Nobody is read.** A row names who created a workspace and who owns it, and carries a name, labels and avatars. The decoder maps the identifier, the product's key and type, and the address, and nothing else.

**The listing is the one part.** A `403` is a key Atlassian knows and refuses the listing, which is a key created with scopes that leave it out: a finding that names the setting and carries everything an earlier read recorded (ADR-0130). A `401` is a key Atlassian does not know and fails the read as a rejected credential. Atlassian's rate limit past what a call waits, and every other failure, fail the read whole.

**The client reads the listing to its end and waits out a rate limit.** The listing takes its cursor in the request's body, and its next link is the cursor or an address carrying it as `cursor`; either is followed, and a link that carries neither is an error, never an end. The reconcile loop's discovery of the site the agents work in reads through the same listing, so a site past the first page is found. A call answered `429` is sent again after the wait Atlassian names, or five seconds where it names none, by the rule every vendor client shares (`httpx.VendorRateLimit`); a `429` is the limiter refusing a request before it is processed, so no method repeats anything.

## Why the obvious alternative is wrong

**An entry per product** makes a card of Jira Software, Jira Service Management and Confluence on every site, three entries for what a person calls "our Atlassian site", and breaks the one entry the settings already make of Jira and Confluence on one site.

**Keying a site by its cloud id** names a second entry beside the one the settings key by host, for every company.

**Naming every site from the listing** puts the site the engine works in into Review for every company, since the settings name it.

**Keeping a site's whole address** keeps the `/wiki` path Confluence's row carries, so one site has two addresses, and keeps whatever a copy of an address carries after its host.

**Reading the first page alone**, as the discovery did, is a whole read only while an organization's products fit on a page; past that, what the page did not hold would be retired a day later.

**Reading a `403` as a rejected credential** tells a company to rotate a key that works for everything else the engine does with it.

## What this does not decide

Whether a site belongs to its organization, which needs a relation from an account to an account (ADR-0137). Jira's projects and Confluence's spaces, which the tracker and the knowledge base read as the seats that work in them. How a refused part is shown, which the dashboard decides.
