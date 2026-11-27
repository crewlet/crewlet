# Meta Ads Manager

Agents read and work on your Facebook and Instagram ads through Crewlet's own tools: ad accounts, campaigns, ad sets, ads and creatives, audiences, pixels and datasets, catalogs, lead forms, Instant Experiences, the Page and Instagram calls an ad needs, insights and the business portfolio. Every operation of Meta's Marketing API v27.0 an ads integration reaches is listed ([the full list](../reference/meta-ads-operations.md)), each in one class, and no agent ever holds a token: Crewlet keeps one system user's tokens from your own Meta app and makes every call ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md)), as the agent's own system user where you give it one ([below](#give-an-agent-its-own-identity)).

Organic Pages and Instagram publishing, messaging and insights, the WhatsApp Business Platform, Commerce and Rights Manager are other Meta products with their own permissions, review and rate limits: they would be separate integrations, and this one leaves them out.

## Connect your Meta app

1. In your business portfolio, create a **Business** app (or use yours) and add the **Marketing API** product. In the app's **Advanced** settings, turn on **Require App Secret**, so a token is useless without the app secret.
2. In **Business Settings**, **System users**, add an **Employee** system user (never an admin one) and assign it the ad accounts agents may use: **View performance** while agents only read, **Manage campaigns** once they may change things. Assign it the Pages, pixels, catalogs and apps an ad uses where agents should reach them.
3. Generate the system user's tokens from your app:
   - the **read token**: `ads_read`, and only permissions that read (`pages_show_list`, `pages_read_engagement`, `instagram_basic`, `leads_retrieval`);
   - optionally the **operate token**: `ads_management`, `pages_read_engagement` and `pages_show_list`, and the write permissions your agents need (`pages_manage_ads`, `catalog_management`), never `business_management`;
   - optionally the **admin token**: `business_management`, used only for an account change an administrator allows.
4. On the integration's page, paste the app's ID and secret and the tokens.

```yaml
integrations:
  meta_ads:
    enabled: true
    approvers: [founder]                       # optional: asked first; only an administrator answers
    app_id: "987654321012345"
    app_secret: ${META_ADS_APP_SECRET}
    read_token: ${META_ADS_READ_TOKEN}
    operate_token: ${META_ADS_OPERATE_TOKEN}   # optional: changes and asks
    admin_token: ${META_ADS_ADMIN_TOKEN}       # optional: account changes an administrator allows
    admin_system_user_token: ${META_ADS_ADMIN_SYSTEM_USER_TOKEN}  # optional: makes each agent's own system user
    business_id: "1234567890"                  # optional; needed with admin_system_user_token
    ad_accounts: ["act_1112223333"]            # optional: the ad accounts agents may be given
    spend:                                     # optional: without both caps every spend act is refused
      accounts:
        - {account: "act_1112223333", currency: EUR, per_change: "50.00", daily: "200.00"}
roles:
  - handle: growth-lead
    integrations:
      meta_ads: {enabled: true, level: operate, ad_accounts: ["act_1112223333"], identity: own}
```

A token's or the app secret's variable may be named at its own field alone: the company document refuses it anywhere else. Meta's own Ads MCP server (`mcp.facebook.com/ads`) is refused in `mcp_servers` while the integration is on: it would reach the same ad accounts around the caps, the decisions and each agent's ad accounts.

## Tokens

Each call goes through the least privileged token holding every permission its operation needs: a read through the read token, a change through the operate token, an account change an administrator allowed through the operate token or, where only it holds the permission, the admin token. The admin token is never used for a read or a change made at once, so a read that needs `business_management` is refused, and a person reads it in Business Settings. The pass refuses a read token holding a permission that writes, an operate token holding `business_management`, a token of another app or user, and a permission no operation needs.

Every call carries the bearer in its `Authorization` header and `appsecret_proof`, the token's HMAC-SHA256 keyed by the app secret. No token, secret or proof reaches a log, an error, a record or an answer; Meta's paging links, which carry the token, are dropped from answers.

A token that expires is renewed when ten days of its sixty are left, once a day until a renewal holds, under one lease for the whole fleet. The renewal is sealed in the engine's own store beside the SHA-256 of the pasted token it came from, and used only while the document names that token: paste a new one and it wins, clear it and its renewal goes. A renewal that keeps failing is a `credential_expiring` finding with the date.

## Give an agent its own identity

By default every agent calls Meta as the company's system user, so Meta's activity history shows one actor for all of them. With `identity: own` on its seat, an agent calls as its own **Employee** system user instead: Meta records its changes under it, and its token can be cut off alone. On the integration's **Agents** tab, an agent set to the company's reads **Needs you** until its own is ready; press **Set up identity** in its box.

- **Crewlet makes it**, once you connect the business's **Admin** system user: in Business Settings, **System users**, add an Admin system user (Meta allows one), generate its token from your app with `business_management`, and paste it on the integration's page as the admin system user token, with the business portfolio ID. Crewlet uses it for nothing else. **Make it for me** then makes `crewlet-<handle>` an Employee system user, assigns it the agent's ad accounts with its level's tasks (**View performance** to read, **Manage campaigns** to operate, **Manage ad account** to administer), and mints a 60-day token holding the level's permissions (`business_management` only at administer) within what your own tokens hold.
- **You make it** without that token: the sheet gives the name to copy, the tasks to turn on and the permissions to tick, and you paste the token it generates.

Crewlet checks the token before it keeps it and on every pass: your app's, an Employee system user (never an Admin one, which reaches every asset), the level's permissions, and each of the agent's ad accounts assigned with its tasks. It is sealed as `META_ADS_TOKEN_<HANDLE>` and reaches no box. The engine's own reads that hold a call to its rules (an object's ad account, a target's state, an account's spend) stay the company's; every call the agent asks for goes as its own.

- **Refused at Meta** (revoked, expired, its tasks taken away): the agent's calls stop, with the reason in its box, until it has a working identity or is set back to the company's. Crewlet never puts the company's system user in its place.
- **Meta's cap**: a business may have 10 system users at Full access and 1 at Limited, shared with every integration there. Past it, the agent stays on the company's system user and its box says so.
- **Level or ad accounts changed**: Crewlet brings a system user it made to the new level and accounts on the next pass, with a fresh token. One you made reads **Wrong role** until you change it in Business Settings.
- **Renewal**: a token with ten days left is renewed (minted anew, or exchanged where no admin system user token is connected) and the old one revoked once the new one is sealed.
- **Turned off, or back to the company's**: Crewlet takes the system user's ad accounts (where it holds the admin system user token) and revokes its tokens. Meta deletes no system user, so it stays, and is reused if the agent comes back.

## Levels

| Level | The agent may |
|---|---|
| `read` (default) | Read everything its ad accounts hold, and their insights |
| `operate` | Also make, at once, a change that raises no spend and shows nobody anything new, and ask for any spend or anything customers see |
| `administer` | Also ask for account changes, production events and archives or deletes, which Meta cannot undo |

A seat names the ad accounts it may touch (`ad_accounts`, required while on). Every object a call names, in its path, its parameters or a filter, is read for the ad account Meta answers it belongs to and refused outside the seat's; a Page, app, pixel or Instagram account a change names must be connected to that ad account. What no ad account owns (the business portfolio, Pages, pixels, catalogs, apps, billing) is reached only by a seat given `business_resources: true`. A list that can name another ad account's objects answers the seat's alone, and a field expansion (`fields=adsets{...}`) is refused.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `meta_ads_operations` | read | Lists every operation, with the level that offers it (one above the agent's marked), what each does, its class and field rules |
| `meta_ads_read` | read | Any read of the API on the agent's ad accounts |
| `meta_ads_accounts` | read | The agent's ad accounts: currency, zone, status and why, spend, minimum daily budget, access tier |
| `meta_ads_campaigns`, `meta_ads_ad_sets`, `meta_ads_ads` | read | One ad account's campaigns, ad sets or ads, with budgets, bids, targeting and review feedback |
| `meta_ads_insights` | read | Insights at account, campaign, ad set or ad level, in the account's zone, with Meta's unified attribution |
| `meta_ads_operate` | operate | A change that raises no spend and shows nobody anything new; refused where it comes to more |
| `meta_ads_pause` | operate | Pauses campaigns, ad sets or ads: the brake |
| `meta_ads_lower_budget`, `meta_ads_lower_bid` | operate | A lower, at the level holding it; a raise is refused |
| `meta_ads_create_app_campaign` | operate | An app promotion campaign, its ad set and ads, every one created PAUSED |
| `meta_ads_spend` | spend | Asks for any change that starts or raises spend |
| `meta_ads_enable` | spend | Asks to set campaigns, ad sets or ads ACTIVE |
| `meta_ads_raise_budget`, `meta_ads_raise_bid` | spend | Asks for a raise; a lower sent here is made at once |
| `meta_ads_change_targeting` | spend | Asks to change an ad set's countries, ages and placements |
| `meta_ads_create_ad` | spend | Asks to create an ad from a creative, or from an image with new text shown whole |
| `meta_ads_publish` | public | Asks for a change customers see: a Page post, a lead form, a reply, catalog items, a creative on a live ad |
| `meta_ads_admin` | admin | Asks for an account change, a production event or an archive or delete |

An update is classed by what it changes on its target as it reads just before. A pause, a lower, a rename and a label are `operate`; archiving and deleting are `admin`, since Meta cannot undo them (pause instead); an enable, a raise and any field no rule names are `spend`. A lowering Crewlet cannot prove from the two values (narrower targeting, a stricter rule) keeps the higher class. A create made PAUSED, a creative swapped on a paused ad, an audience no active ad set uses, an automated rule that only pauses or notifies, a test event, a draft Instant Experience and an unpublished Page post are `operate`; production events Meta cannot recall are `admin`. A dry run (`execution_options: ["validate_only"]`) changes nothing and is made at once. An act only an admin-level task may make (an ad account's **Manage ad account**, a business admin, a catalog's **Manage catalog**) is always held; a read only such a task may make is not offered.

Not offered in this release: the programs Meta grants separately (Messenger marketing messages, Managed Partner Ads, reach and frequency buying, Marketplace partner catalogs, child businesses), and operations whose spec names no node or gives no parameters, held until a call confirms them.

## Spend and decisions

A spend, public or admin call is held for an administrator, who answers **Allow once** or **Deny** at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). Before anybody is asked, a spend act passes the spend guard: an ad account's daily capacity is, over campaigns whose own status is ACTIVE and that have not ended, the campaign's daily budget where it holds the budget, otherwise its ACTIVE ad sets' daily budgets, a lifetime budget counted as its remainder over its remaining days. A change may add at most `per_change` to it and leave it at most `daily`. Enabling an ad, raising a bid or changing targeting adds nothing to capacity and is still asked. Caps limit the budgets agents may set: Meta may spend more than a daily budget on a given day.

An agent holds only its level's tools, and each tool that takes a method's ID says what lies above it. A method above the agent's level, named on a tool it holds, is refused before Meta is called, naming the method's class, the tool and level that make it, whether an administrator approves each call, and that an administrator raises the level on the Agents tab, asked in words (`request_access` asks for no level). A method Crewlet does not offer is refused as one no level reaches.

The administrator reads the exact requests, the targets as they are now and the capacity before and after; Allow once records the answer first, runs the guard again under the ad account's lease and sends nothing if a target changed. An unanswered ask lapses after a day. Every call, ask, answer and outcome is in the access log.

## Limits

- Meta says each ad account's business use case usage on every answer. Agents' reads stop at nine tenths of any limit, which is kept for approved acts and the pass, and wait while Meta asks for time to regain access; only a read is retried.
- An ad set's budget changes at most four times an hour: a fifth is refused before anybody is asked.
- A new app starts on Meta's **Limited** access (about 300 change calls an hour per ad account, plus 40 per active ad); Meta moves it to **Full** access after 500 successful calls in 15 days with an error rate under 15%, requested on the app's Marketing API page. The integration's page says while it is Limited.
- Amounts are in the ad account's currency, in Meta's own units: cents, or whole units for the eleven currencies Meta writes that way (JPY, KRW, HUF and others).
- Insights at ad level, with a breakdown or over more than 31 days run as an async report; recent days may still change, and such an answer is marked estimated.
- Crewlet calls Marketing API v27.0. A deprecation warning from Meta raises a notice to update Crewlet.
