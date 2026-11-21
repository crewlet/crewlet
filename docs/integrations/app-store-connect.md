# App Store Connect

Agents read and work on your App Store apps through Crewlet's own tools: listings, ratings and reviews and replies to them, TestFlight, custom product pages, in-app events, in-app purchases and subscriptions, prices and availability, sales and analytics, and the team's users, devices, certificates and profiles. Every operation of Apple's App Store Connect API is offered ([the full list](../reference/app-store-connect-operations.md)), each in one class, and no agent ever holds a key: Crewlet keeps your team's API keys and makes every call ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md)).

## Connect a team

1. The Account Holder requests App Store Connect API access once, in **Users and Access**, **Integrations**.
2. For each Apple role you want agents to use, create a **team key** there (**Team Keys**, **+**) with that role. Apple shows each key's `.p8` file once and keeps no copy.
3. On the integration's page, add the team: its issuer ID (shown above the keys), its vendor number for sales (on **Payments and Financial Reports**), and each key's ID and `.p8` file in the slot of its role.

A company may connect several teams, each under its own connection id; a call that names no app goes to the only team, or names one. Every slot is optional; at least one is needed.

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

Each call goes through the connected key with the least access whose role may make it, and a key Apple refuses is never retried with another. The Admin key can manage the team's users and keys, so it makes an account change an administrator allowed, or a change Apple lets only Admin make, and nothing else: never a read or an operate change, even where it is the only key that could. Every call through it is on the access log, and a pass checks that it reads as a key without calling Apple with it. Every slot is empty until a person fills it, and removing a key from the company document takes effect on the next call.

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

## Levels

| Level | The agent may |
|---|---|
| `read` (default) | Read everything its apps hold |
| `operate` | Also make changes nobody outside the team sees at once (a draft version's text, a draft custom product page or in-app event, internal TestFlight), and ask for anything customers or external testers see |
| `administer` | Also ask for account changes: users, invitations, devices, certificates, bundle IDs, profiles, sandbox testers, webhooks, and deletes that cannot be undone |

A seat names the apps it may touch (`apps`, required while on). Resources no app owns (users, devices, certificates, profiles, reports across apps) need `team_resources: true` on the seat.

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
