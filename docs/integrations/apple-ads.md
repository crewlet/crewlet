# Apple Ads

Agents read and work on your Apple Ads campaigns through Crewlet's own tools: App Store and Apple Maps campaigns, ad groups, keywords and negative keywords, ads and creatives, budget orders, reports, impression share, recommendations and change history. Every operation of the Apple Ads Platform API is offered ([the full list](../reference/apple-ads-operations.md)), each in one class, and no agent ever holds a key or a token: Crewlet keeps your API users' keys and makes every call ([ADR-0170](../../adr/0170-a-growth-integrations-credential-stays-in-the-engine.md)). The older Campaign Management API v5 is not used: Apple shuts it down on January 26, 2027.

## Connect an API user

1. An account administrator invites an API user in Apple Ads (**Account Settings**, **User Management**), with the role agents need on each ad account (see [Roles](#roles)).
2. Make the user's key pair:

   ```sh
   openssl ecparam -genkey -name prime256v1 -noout -out private-key.pem
   openssl ec -in private-key.pem -pubout -out public-key.pem
   ```

3. Signed in as that user, paste `public-key.pem` under **Account Settings**, **API**. Apple then shows its client ID, team ID and key ID.
4. On the integration's page, add the connection: the three IDs and `private-key.pem`, and optionally the organization's ID, which the pass then checks the user answers.

A company may connect several API users, each under its own connection id. Each call goes through the connection whose user reaches its ad account.

```yaml
integrations:
  apple_ads:
    enabled: true
    approvers: [founder]          # optional: asked first; only an administrator answers
    ad_accounts: ["123456789"]    # optional: the ad accounts agents may be given
    connections:
      main:                       # the connection's id
        org_id: "987654321"       # optional
        client_id: SEARCHADS.aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1
        team_id: SEARCHADS.aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1
        key_id: a273d0d3-4d9e-458c-a173-0db8619ca7d7
        private_key: ${APPLE_ADS_MAIN_KEY}
    spend:                        # optional: without both caps every spend act is refused
      accounts:
        - {account: "123456789", currency: USD, per_change: "50.00", daily: "300.00"}
roles:
  - handle: growth-lead
    integrations:
      apple_ads: {enabled: true, level: operate, ad_accounts: ["123456789"]}
```

The key's variable may be named at its own field alone: the company document refuses it anywhere else.

## Keys and tokens

For each connection Crewlet signs a client secret with the private key (ES256, ten minutes from a minute back), exchanges it at Apple for an access token, and keeps the token in memory until a minute before it ends. Every call to an ad account names it in `X-AP-Context`; budget orders and new ad accounts are the organization's and name none. A key Apple refuses is a finding on the integration's page.

## Give an agent its own identity

Not offered: every agent's calls go as the company's API users. An API user of its own needs an Apple Account per agent, which a person invites and activates by hand, so on the **Agents** tab the company's identity is the normal state, In sync, with nothing to set up ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)). Apple Ads still records Crewlet's changes under the connection's API user, and the access log names the agent for each.

## Roles

Apple Ads gives an API user a role per ad account in its own interface; the API cannot set one. Crewlet reads each connection's roles from Apple, shows them, and refuses an operation the role on that ad account cannot make, naming the roles that can.

| Role | What agents can do with it |
|---|---|
| API Account Read Only | Read everything on the ad account |
| Limited Access: API Read Only | Read the campaigns Apple lets the user see |
| Limited Access: API Read & Write | Read and change the campaigns Apple lets the user see |
| API Account Manager | Everything, budget orders and new ad accounts included |
| Admin | Everything on the ad account |

## Levels

| Level | The agent may |
|---|---|
| `read` (default) | Read everything its ad accounts hold, and their reports |
| `operate` | Also make, at once, a change that raises no spend and publishes nothing (a pause, a lowered budget, bid or target CPA, a rename, a negative keyword, an image upload, a creative or location group nothing uses yet), and ask for any spend or creative customers see |
| `administer` | Also ask for ad account changes and deletes, which Apple cannot undo |

A seat names the ad accounts it may touch (`ad_accounts`, required while on). Every object a call names, in its path or its body, is read under that ad account and refused if Apple answers it for another; a budget order may name the seat's ad accounts alone.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `apple_ads_operations` | read | Lists every operation, with the level that offers it (one above the agent's marked), their class and roles |
| `apple_ads_read` | read | Any read or query of the API on the agent's ad accounts |
| `apple_ads_report` | read | A campaign, ad group, keyword, search term or ad report: zone, freshness, truncation, why empty |
| `apple_ads_operate` | operate | A change that raises no spend and publishes nothing; refused where it comes to more |
| `apple_ads_pause` | operate | Pauses campaigns, ad groups, keywords or ads: the brake |
| `apple_ads_spend` | spend | Asks for any change that starts or raises spend |
| `apple_ads_set_budget` | spend | A campaign's daily budget: a lower made at once, a raise asked |
| `apple_ads_set_bids` | spend | Keyword or ad group bids: lowers made at once, raises asked |
| `apple_ads_publish` | public | Asks for a creative change customers see |
| `apple_ads_admin` | admin | Asks for an ad account change or a delete |

An update is classed by what it changes on its target as it reads just before: a pause, a lower and a rename are `operate`; an enable, a raise, wider targeting or any other field is `spend`. A creative an ad serves is `public`, one no ad serves `operate`; a location group an ad group targets is `spend`, one none targets `operate`. A lower or a pause sent through a spend tool is made at once.

## Spend and decisions

A spend, public or admin call is held for an administrator, who answers **Allow once** or **Deny** at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). Before anybody is asked, a spend act passes the spend guard: an ad account's daily capacity is the daily budgets of its enabled, unended campaigns, and a change may add at most `per_change` to it and leave it at most `daily`. A bid or target CPA raise adds nothing to capacity and is still asked. The administrator reads the exact requests, the targets as they are now and the capacity before and after; Allow once runs the guard again and sends nothing if a target changed. An unanswered ask lapses after a day. Every call, ask, answer and outcome is in the access log.

An agent holds only its level's tools, and each tool that takes a method's ID says what lies above it. A method above the agent's level, named on a tool it holds, is refused before Apple Ads is called, naming the method's class, the tool and level that make it, whether an administrator approves each call, and that an administrator raises the level on the Agents tab, asked in words (`request_access` asks for no level). A method Crewlet does not offer is refused as one no level reaches.

## Limits

- Apple says each answer's rate window and how many calls are left in it. Agents' reads stop at a tenth of the window, which is kept for approved acts; a read waits out a short `Retry-After`, and a longer one is answered as Apple's quota.
- Reports take the ad account's time zone, or UTC except for search terms. The last three days' installs may still rise, so a report reaching them is marked estimated; Apple leaves out search terms with too few impressions.
- An uploaded image is at most 8 MiB.
