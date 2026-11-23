# App Store Connect

Agents read and work on your App Store apps through Crewlet's own tools: listings, ratings and reviews and replies to them, TestFlight, custom product pages, in-app events, in-app purchases and subscriptions, prices and availability, sales and analytics, and the team's users, devices, certificates and profiles. Every operation of Apple's App Store Connect API is offered ([the full list](../reference/app-store-connect-operations.md)), each in one class, and no agent ever holds a key: Crewlet keeps your team's API keys, and each agent's own key where it has one, and makes every call ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md)).

## Connect a team

1. The Account Holder requests App Store Connect API access once, in **Users and Access**, **Integrations**.
2. For each Apple role you want agents to use, create a **team key** there (**Team Keys**, **+**) with that role. Apple shows each key's `.p8` file once and keeps no copy.
3. On the integration's page, add the team: its issuer ID (shown above the keys), then under **API keys**, **Add a key** for each key: its role, its key ID and its `.p8` file (choose the file, or paste it whole). Its vendor number for sales (on **Payments and Financial Reports**) is under **More settings**.

A company may connect several teams, each under its own connection id; a call that names no app goes to the only team, or names one. A team needs at least one key; start with a Marketing key, and add one key per role so each call is made with the least access that can make it.

```yaml
integrations:
  app_store_connect:
    enabled: true
    approvers: [founder]        # optional: asked first; only an administrator answers
    apps: ["1234567890"]        # optional: the apps agents may be given
    connections:
      main:                     # the connection's id
        issuer_id: 57246542-96fe-1a63-e053-0824d011072a
        vendor_number: "85012345"
        keys:
          marketing:        {key_id: 2X9R4HXF34, private_key: ${APP_STORE_CONNECT_MAIN_MARKETING_KEY}}
          customer_support: {key_id: K1L2M3N4P5, private_key: ${APP_STORE_CONNECT_MAIN_CUSTOMER_SUPPORT_KEY}}
          finance:          {key_id: 9A8B7C6D5E, private_key: ${APP_STORE_CONNECT_MAIN_FINANCE_KEY}}
roles:
  - handle: growth-lead
    integrations:
      app_store_connect: {enabled: true, level: operate, apps: ["1234567890"]}
```

A key's variable may be named at its own field alone: the company document refuses it anywhere else.

## Roles and keys

Each call goes through the connected key with the least access whose role may make it, and a key Apple refuses is never retried with another. The Admin key can manage the team's users and keys, so it makes an act an administrator allowed and nothing else: never a read, a check of what a call names or an operate change, even where it is the only key that could. A change Apple lets only Admin make is an admin act, held for an administrator; a target only Admin may read is not read first, and the administrator allows the exact request. Every call through it is on the access log, and a pass checks that it reads as a key without calling Apple with it. Every slot is empty until a person fills it, and removing a key from the company document takes effect on the next call.

| Slot | Apple role | What agents can do with it |
|---|---|---|
| `access_to_reports` | Access to Reports | Sales and trends reports |
| `sales` | Sales | Sales and analytics reports |
| `finance` | Finance | Sales, finance and analytics reports; prices and agreements, read |
| `customer_support` | Customer Support | Reviews and replies to them; App Store details |
| `marketing` | Marketing | Listings, custom product pages, in-app events, internal TestFlight groups |
| `developer` | Developer | Builds and TestFlight; with access to certificates, identifiers and profiles, signing and devices |
| `app_manager` | App Manager | Versions, submissions, prices and availability, in-app purchases, external TestFlight |
| `admin` | Admin | Users, invitations and every account change, each allowed by an administrator; nothing else |

The Account Holder has no key: an act only it may take is done in App Store Connect by that person.

## Give an agent its own identity

By default every agent's calls go as the company's team keys, so Apple's history shows one actor for every agent. Give an agent a team key of its own and Apple records its submissions under that key, its calls get their own hourly budget, and you can revoke it alone ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)). Until it has one, the agent's row on the **Agents** tab reads **Needs you**.

1. On the integration's **Agents** tab, open the agent and press **Set up identity**.
2. In App Store Connect, **Users and Access**, **Integrations**, **Team Keys**, press **Generate API Key** (or **+**). Only the Account Holder or an Admin can.
3. Name it as the sheet shows (`crewlet-<handle>`, from the one rule Crewlet names every agent identity by) and give it the role the agent's level needs. Never Admin.
4. Press **Generate**, then **Download API Key** on its row: Apple offers the `AuthKey_<Key ID>.p8` file once.
5. Back on the sheet, choose the file. Crewlet reads the key ID from its name, checks the key, and keeps it only if it works.

| Level | Role of the agent's key |
|---|---|
| `read` | Marketing (App Manager or Developer work, with a warning: they reach more) |
| `operate` | App Manager |
| `administer` | App Manager |

**The check** makes reads only: it signs with each connected team's issuer to find the key's team, makes a read every role may make (a key that fails it is revoked or another team's), reads the key's role from seven list reads whose roles Apple's role matrix fixes (Apple has no call that names a key's role), and confirms the agent's apps are in that team. An Admin key, a key at a role that does not fit the level, another team's key, a key another agent keeps and one of the company's own keys are refused and kept nowhere. A kept key is sealed under `APP_STORE_CONNECT_KEY_<HANDLE>`, reaches no box, and is checked again on every pass. The document records the choice on the seat:

```yaml
roles:
  - handle: growth
    integrations:
      app_store_connect: {enabled: true, level: read, apps: ["1234567890"], identity: own}
```

**Which key signs a call.** A call the agent's key may make goes through it. A read its role may not make (a sales report, for a Marketing key) goes through the company's least-privileged key that can, and the access log records it as the company's. An account change only Admin may make goes through the company's Admin key once an administrator allows it. Every other change goes through the agent's key or is refused before Apple is asked. Each record on the access log names whose identity signed it (`signer`: `own` or `company`).

**When Apple refuses it.** A key revoked at Apple, or one whose team or role no longer fits, stops the agent's calls: they are refused with the reason, never sent as the company's, until you give it a new key or press **Use the company's for now**.

**Replacing and giving one up.** Apple cannot change a key's role or revoke one by API. To replace a key, set up a new one: Crewlet switches once it works and asks you to revoke the old one at Apple. Turning the agent off, or back to the company's keys, clears its sealed key and asks for the same revoke. Either way the row says which key to revoke until a pass finds Apple refusing it.

## Levels

| Level | The agent may |
|---|---|
| `read` (View, the default) | Read everything its apps hold |
| `operate` (Manage) | Also make changes nobody outside the team sees at once (a draft version's text, a draft custom product page or in-app event, internal TestFlight), and ask for anything customers or external testers see |
| `administer` (Administer) | Also ask for account changes: users, invitations, devices, certificates, bundle IDs, profiles, sandbox testers, webhooks, and deletes that cannot be undone |

A seat names the apps it may touch (`apps`, required while on). The company's `apps` narrows what a seat may name; without it, a seat may name any app the team's keys reach. On the integration's **Agents** tab, an agent's app chooser lists the apps the keys reach by name, each with its Apple ID and bundle ID, to tick and search; an app it does not list can still be added by its Apple ID. Crewlet reads the list with the least-privileged key that may list apps (never the Admin key, so a team with only an Admin key lists none, and its apps show by Apple ID), keeps it for ten minutes, and reads it again on every pass and on **Check now**. The integration's **Settings** tab lists the apps each connection reaches; where no key may list them, it, the **Agents** tab and the chooser say which key to add.

Every resource a call names that belongs to an app, in its path, its filters or the body it sends, must be in one of those apps; Crewlet finds its app before anything is sent. Resources no app owns (users, beta testers, devices, certificates, reports across apps) need `team_resources: true` on the seat; Apple's own reference data (territories, app categories) needs nothing. An answer never shows an app outside the seat's apps, and a read through a resource no app owns that would show another app's resources (a tester's beta groups) is refused unless the seat's apps are all of the team's: read them through one of its apps instead.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `app_store_connect_operations` | read | Lists the operations the agent may call now, with their class and roles |
| `app_store_connect_read` | read | Any read of the API on the agent's apps |
| `app_store_connect_operate` | operate | A change nobody outside the team sees; refused where the target is live |
| `app_store_connect_publish` | public | Asks for a change customers or external testers see |
| `app_store_connect_admin` | admin | Asks for an account change or a delete that cannot be undone |
| `app_store_connect_reviews` | read | An app's reviews, newest first, never the reviewer's name |
| `app_store_connect_reply_to_reviews` | public | Asks to post 1 to 10 replies to one app's reviews, allowed or denied together |
| `app_store_connect_sales` | read | One Pacific day's units and proceeds per app, territory and currency; an in-app purchase's or subscription's row counts for the app whose SKU it names |

A write is classed by what its target is when it is made: an edit to a version in Prepare for Submission is operate, the same edit to a live version public. A state Crewlet cannot read keeps the stricter class.

## Decisions

A public or admin call is held for an administrator, who answers **Allow once** or **Deny** at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). The administrator reads the exact request and the target as it is now; if the target changes before the answer, Allow once sends nothing and the agent is told to ask again. An unanswered ask lapses after a day. Every call, ask, answer and outcome is in the access log.

## App Store analytics

Impressions and product page views come from an ongoing analytics report request per app, which only an Admin key may create. A team that keeps no Admin key turns them on from the integration's settings: **Turn on App Store analytics**, paste an Admin key's ID and `.p8` file, at a fresh sign-in. Crewlet creates each app's request with it at once, keeps no copy, and asks you to revoke the key in App Store Connect afterwards; the access log records each request as yours. A team with a connected Finance, Sales or Access to Reports key reads the reports from then on.

## Limits

- Apple allows each key a number of requests per hour and says how many are left on every answer. Agents' reads stop at a tenth of a key's limit, which is kept for approved acts.
- Apple publishes a day's sales by about 8 a.m. Pacific time the next day.
- A reply is at most 5,970 characters; Apple publishes it within a day.
