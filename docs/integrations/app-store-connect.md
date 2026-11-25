# App Store Connect

Agents read and work on your App Store apps through Crewlet's own tools: listings, ratings and reviews and replies to them, TestFlight, custom product pages, in-app events, in-app purchases and subscriptions, prices and availability, and the team's users, devices, certificates and profiles. Every operation of Apple's App Store Connect API is offered ([the full list](../reference/app-store-connect-operations.md)), each in one class, and no agent ever holds a key: Crewlet keeps each agent's own team key, checks it, and makes every call with it ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md)).

## Connect a team

1. The Account Holder requests App Store Connect API access once, in **Users and Access**, **Integrations**.
2. On the integration's page, add the team by its issuer ID, which App Store Connect shows above the team keys on that **Integrations** page.
3. On the **Agents** tab, turn agents on, each with its level and apps, and give each its own key ([below](#give-an-agent-its-own-identity)).

A company may connect several teams, each under its own connection id; an agent works in the team its key belongs to.

```yaml
integrations:
  app_store_connect:
    enabled: true
    approvers: [founder]        # optional: asked first; only an administrator answers
    connections:
      main:                     # the connection's id
        issuer_id: 57246542-96fe-1a63-e053-0824d011072a
roles:
  - handle: growth-lead
    integrations:
      app_store_connect: {enabled: true, level: operate, apps: ["1234567890"]}
```

## Roles and keys

Crewlet holds no key of the company's, of any role. Every call is signed with the calling agent's own key, and so is every read Crewlet makes for one (which team, the team's apps, which app a resource is in, a target's state before an act). An agent without a checked key makes no call: each is refused, pointing to **Set up identity**.

A call the agent's key's role may not make is refused before Apple is asked, naming the role it needs. An operation only Admin may make (an analytics report request, for one) is refused too: Crewlet holds no Admin key, and a person makes it in App Store Connect. Sales reports, which only Finance, Sales and Access to Reports may read, take an agent whose own key is a Sales key; finance and analytics reports are out of an agent's reach. The Account Holder has no key: an act only it may take is done in App Store Connect by that person.

## Give an agent its own identity

Each agent signs its calls with a team key of its own: Apple records its submissions under that key, its calls get their own hourly budget, and you can revoke it alone ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)). Until its key is checked, the agent's row on the **Agents** tab reads **Needs you**.

1. On the integration's **Agents** tab, open the agent and press **Set up identity**.
2. In App Store Connect, **Users and Access**, **Integrations**, **Team Keys**, press **Generate API Key** (or **+**). Only the Account Holder or an Admin can.
3. Name it as the sheet shows (`crewlet-<handle>`, from the one rule Crewlet names every agent identity by) and give it the role the agent's level needs. Never Admin.
4. Press **Generate**, then **Download API Key** on its row: Apple offers the `AuthKey_<Key ID>.p8` file once.
5. Back on the sheet, choose the file. Crewlet reads the key ID from its name, checks the key, and keeps it only if it works. For a Sales key, also give the team's vendor number (digits, on **Payments and Financial Reports**): Apple files sales under it, and Crewlet keeps it with the key.

| Level | Role of the agent's key |
|---|---|
| `read` | Marketing (App Manager or Developer work, with a warning: they reach more), or Sales for an agent that reads sales |
| `operate` | App Manager, or Customer Support for an agent that answers reviews |
| `administer` | App Manager |

A Sales or Customer Support key makes only what its role may: sales reports, or reading and replying to reviews. Anything else the agent asks is refused, naming the role it needs.

**The check** makes reads only: it signs with each connected team's issuer to find the key's team, makes a read every role may make (a key that fails it is revoked or another team's), lists the team's apps and confirms the agent's apps are in that team, then reads the key's role from five reads whose roles Apple publishes (Apple has no call that names a key's role). Only Apple's role denial (`403 FORBIDDEN_ERROR`) counts as a read the role may not make; a 403 for an agreement or a feature the team lacks, a 404 or a 409 says nothing of the role. Where Apple's answers leave several roles that each fit the level, the key is kept and the check names them. Each probe's status and Apple's code are shown under the role step and logged as `appstoreconnect_role_probe`. An Admin key, a key at a role that does not fit the level, a Sales key without the vendor number, another team's key and a key another agent keeps are refused and kept nowhere. A check Apple did not answer keeps nothing and says so; a refusal says what is missing and what to do, at the step Apple refused: a team whose Account Holder has not accepted the latest agreement fails the first step, and the Account Holder accepts it in App Store Connect. A kept key is sealed under `APP_STORE_CONNECT_KEY_<HANDLE>`, reaches no box, and is checked again on every pass.

**When Apple refuses it.** A key revoked at Apple, or one whose team or role no longer fits, stops the agent's calls: they are refused with the reason until you hand in a new key.

**Replacing and giving one up.** Apple cannot change a key's role or revoke one by API. To replace a key, set up a new one: Crewlet switches once it works and asks you to revoke the old one at Apple. Turning the agent off clears its sealed key and asks for the same revoke; there is no company key to go back to. Either way the row says which key to revoke until a pass finds Apple refusing it.

## Levels

| Level | The agent may |
|---|---|
| `read` (View, the default) | Read everything its apps hold |
| `operate` (Manage) | Also make changes nobody outside the team sees at once (a draft version's text, a draft custom product page or in-app event, internal TestFlight), and ask for anything customers or external testers see |
| `administer` (Administer) | Also ask for account changes: users, invitations, devices, certificates, bundle IDs, profiles, sandbox testers, webhooks, and deletes that cannot be undone |

A seat names the apps it may touch (`apps`, required while on), chosen on the integration's **Agents** tab from every app of the connected teams: the chooser lists each by name with its bundle ID, to tick and search, and an app it does not list can still be added by its Apple ID. Crewlet lists a team's apps with an agent's own key, first the key being checked on **Set up identity**, then any checked key in that team; it keeps the list for ten minutes and lists again on every pass and on **Check now**. The integration's **Settings** tab lists each connection's apps. Until one agent's key in a team is checked, the team lists no app, and the chooser and the **Settings** tab say that app names appear once an agent's key is checked.

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
| `app_store_connect_reply_to_reviews` | public | Asks to post 1 to 10 replies to one app's reviews, allowed or denied together; needs a Customer Support key |
| `app_store_connect_sales` | read | One Pacific day's units and proceeds for the agent's apps, per currency; needs a Sales key |

A write is classed by what its target is when it is made: an edit to a version in Prepare for Submission is operate, the same edit to a live version public. A state Crewlet cannot read keeps the stricter class.

## Decisions

A public or admin call is held for an administrator, who answers **Allow once** or **Deny** at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)); an allowed call is sent with the asking agent's own key. The administrator reads the exact request and the target as it is now; if the target changes before the answer, Allow once sends nothing and the agent is told to ask again. An unanswered ask lapses after a day. Every call, ask, answer and outcome is in the access log. A call that failed before its request left (its lease, the target's read again, its context's end) is recorded as failed for that cause and told to the agent as not sent; a write whose request got no answer from Apple is recorded as an unknown outcome, and such a reply is reported `unknown`, to check on the review.

## Limits

- Apple allows each key a number of requests per hour and says how many are left on every answer. An agent's reads stop at a tenth of its key's limit, which is kept for approved acts.
- A reply is at most 5,970 characters; Apple publishes it within a day.
