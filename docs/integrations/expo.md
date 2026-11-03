# Expo

Expo gives each agent its **own Expo robot user** on the company's Expo account, holding the permissions its level allows, with an access token of its own. An agent reads EAS builds, submissions and updates as itself and, where you allow it, starts builds and publishes updates, through the EAS CLI in its own container.

The engine creates, re-levels and deletes those robots and their tokens itself, through one token you create for it. Nothing here receives events from Expo yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `viewer` (the default) | View the account's projects, builds, submissions and updates | Start, cancel or change anything |
| `runner` | Everything `viewer` can, plus Expo's low-risk actions, such as starting a build | Publish an update |
| `developer` | Everything `runner` can, plus publish updates and manage the project's credentials | Publish to a protected channel |
| `release_manager` | Everything `developer` can, plus publish to protected channels and use protected credentials | Manage the account, its members or its programmatic access |

Each level is meant to match the Expo role of the same name; [Each agent's access is a level](#each-agents-access-is-a-level) says how far that is checked. Every agent's robot is its own, so Expo names the agent behind each build and update it starts, and turning one agent off never touches another. No agent is ever given Expo's Admin or Owner permissions.

## Setting it up from the dashboard

Company › Integrations › Expo asks two things and checks them before it saves:

1. **Account name**: the Expo account or organization the agents work in, exactly as it appears in its address, `expo.dev/accounts/<name>`.
2. **Provisioner token**: the one token Crewlet uses to manage each agent's robot. See [Creating the provisioner token](#creating-the-provisioner-token).

The form seals the token as `EXPO_ACCESS_TOKEN`, points the config at it and activates. The next pass creates a robot for every agent seat that is on. Everything below describes what that writes, and is what you edit by hand if you would rather.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

### Creating the provisioner token

The provisioner token needs Expo's **Admin** role on the account, because Admin is the role that manages programmatic access: robot users, their permissions and their tokens. Crewlet uses it for that one job, and no agent ever holds it. Either of these works:

| Token | How to create it | Trade-off |
|---|---|---|
| A robot user's token (recommended) | In the account's settings on expo.dev, open **Access tokens**, add a robot user with the **Admin** role, then create a token for it | Belongs to the account, so it keeps working when whoever created it leaves |
| A personal access token | Create a token in your own settings, limit it to **only selected accounts**, and give this account the **Admin** role | Never holds more than you do, so it stops working if you leave the account or lose Admin |

Do not use a token with the Owner role: Admin is all Crewlet needs. Expo shows a token's value once. Paste it into the form, or seal it yourself with `crewlet secrets set EXPO_ACCESS_TOKEN`.

### Testing the token before you save it

The form offers **Test key** under the provisioner token. It runs read-only queries against Expo's GraphQL API (`https://api.expo.dev/graphql`) in order and stops at the first that fails, so the result names what to fix. Nothing is stored by a test.

| Stage | What it reads | What a failure means |
|---|---|---|
| The token works | The user or robot the token belongs to | The token is wrong or revoked |
| The account exists | The account by its name | The account name is wrong, or the token cannot see that account |
| The token can manage robots | The token's own permissions on the account | The token's user or robot does not hold Admin on the account |
| The projects are readable | The account's projects | Expo did not answer the listing; the result carries its reason |

When every stage passes, the result names the account and the number of projects on it. That is the whole claim: whether Expo accepts each level's permissions is proved only when the first agent's robot is created, because a test never writes at Expo.

## Giving each agent its own Expo robot

Each agent seat is turned on or off, and given its level, on the integration's **Agents** tab (or the agent's own page, which uses the same control). The tab writes one revision of the company document, under the seat's `roles[].integrations.expo` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has an Expo robot at all. Required in every block. A seat with no block is off, and a new agent seat is off until somebody turns it on. |
| `level` | `viewer`, `runner`, `developer` or `release_manager`. Unset is `viewer`. |

```yaml
roles:
  - handle: qa
    integrations:
      expo:
        enabled: true
        level: runner
    sandbox:
      env:
        EXPO_TOKEN: "${EXPO_TOKEN_QA}"   # the robot's token, for the EAS CLI; see below
```

### Each agent's access is a level

A level is a fixed set of Expo permissions, the same in every company, named after the Expo role that holds the same set:

| Level | Expo permissions | Checked against a live account |
|---|---|---|
| `viewer` | `VIEW` | Not yet |
| `runner` | `VIEW`, `RUN` | Not yet |
| `developer` | `VIEW`, `RUN`, `PUBLISH` | Not yet |
| `release_manager` | `VIEW`, `RUN`, `PUBLISH`, `PUBLISH_PROTECTED` | Not yet |

Expo's API grants permissions (`VIEW`, `RUN`, `PUBLISH`, `PUBLISH_PROTECTED`, `ADMIN`, `OWN`) and its dashboard shows roles, and Expo does not document which permissions each role holds. The sets above follow the role descriptions in Expo's documentation and have **not yet been checked against a live account**. The mapping is one table in the engine, so a check that finds a difference corrects it in one place. `ADMIN` and `OWN` are in no level.

Expo describes its Viewer role as viewing projects through Expo Go. If an agent on `viewer` is refused when it lists builds, move it to `runner`.

### What each change does at Expo

| You do | Crewlet does at Expo |
|---|---|
| Turn a seat on | Creates a robot user named `crewlet-<company>-<handle>` on the account with the level's permissions, then creates an access token for it and seals the value. |
| Change its level | Grants the robot the permissions the new level adds, then revokes the ones it drops, so it never holds less than the lower of the two levels in between. The token does not change, so the agent's tools keep working. |
| Turn it off | Deletes the robot's access token, so the agent stops at once, then asks Expo to delete the robot, and clears the sealed value. |
| Turn it back on | Creates a new robot and a new token. No person is needed. |
| Remove it (only while off) | Nothing at Expo: the robot is already deleted. Crewlet stops tracking it. |
| Rotate its token | Creates a new token, seals it, then deletes the old one. Anything still using the old value stops working. |

Crewlet only ever changes robots and tokens it created. It records the ID of every robot and token it creates and never adopts one by its name, even one under a `crewlet-` name.

### The token is the engine's, and named for the seat

The token's value is sealed under one name the engine derives for the seat, `EXPO_TOKEN_<HANDLE>` (for example `EXPO_TOKEN_QA`). The company document never names it, so nothing written to the document can point a seat's token at another variable or at the provisioner's. Expo shows a token's value exactly once, so a token that was created and **could not be sealed is deleted again** at once rather than left live and held by nobody.

Expo documents no expiry for access tokens, so nothing rolls them on a schedule. **Rotate credential** on the Agents tab replaces a value you believe has leaked.

### The agent's Expo tools

An agent works with Expo two ways.

**The EAS CLI, as itself.** The EAS CLI reads an access token from `EXPO_TOKEN` in place of a login. Declare it in the seat's [`sandbox.env`](../concepts/code-sandbox.md#credentials-and-the-run-environment), pointing at the seat's own sealed token, as in the example above:

```yaml
    sandbox:
      env:
        EXPO_TOKEN: "${EXPO_TOKEN_QA}"
```

The engine names no variable in a run of its own, so without this line (or a setup step's `env` or file naming the same token) the agent's container has no Expo token. While the access sync has not minted the token yet, a run waits for it rather than starting without it. The Agents tab reports per seat whether the tool is live, waiting for its token, or not set up because the seat's box names no `${EXPO_TOKEN_<HANDLE>}`, and says the line to add. With it, an agent runs commands such as these with no `eas login`:

```sh
npx eas-cli@27.2.0 build:list
npx eas-cli@27.2.0 build:view <build-id>
npx eas-cli@27.2.0 update:list
npx eas-cli@27.2.0 submit --platform ios --latest
```

Each command names the one EAS CLI release Crewlet pins, so every agent runs the same release and `npx` fetches it once. A command acts on the project in the working directory, which must already be linked to EAS (`extra.eas.projectId` in its app config). Linking a project is a person's job; see [Troubleshooting](#troubleshooting).

**Expo's hosted MCP server.** Expo's MCP server at `https://mcp.expo.dev/mcp` (Streamable HTTP) lists, starts and cancels builds and workflows, reads their logs, and reads crash reports and store reviews. Add it from Company › Tools › MCP servers, where the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) offers it as `expo`, and a member of the company's Expo account signs it in with OAuth. It is a company-wide grant: every agent it is granted to acts through the account of the person who signed it in, not through its own robot. Its simulator tools start sessions Expo bills, and `search_documentation` needs a paid plan.

### Every pass reads each robot back

Each pass reads every robot this engine manages, including the robot of a seat that has since left the document.

- **A robot's permissions changed at Expo** by somebody else are restored to the seat's level and reported as `drift_restored`, with the before and after.
- **A robot or token deleted at Expo** is dropped from the engine's records, and the next pass creates a fresh one if the seat is on.
- **A robot somebody emptied of every permission** at Expo is treated as turned off by hand, and a pass never grants it back, in case it was incident response. The tab offers **Turn back on** or **Keep off**.
- **A seat that leaves the document** is treated as off: its token is deleted and its robot with it.

A refusal names the step that failed, Expo's own reason, what to do about it and who does it. The step is retried on every pass and on **Retry**.

### Disconnecting

Disconnecting deletes every token this engine created, asks Expo to delete every robot it created, and clears their sealed values. A robot left behind after a disconnect would be an identity nobody manages. Builds and updates a robot made stay on the account. The provisioner token is yours, so it stays at Expo and in the secret store until you revoke it.

## Configuration

```yaml
integrations:
  expo:
    enabled: true
    provisioning:
      account_name: example-org
      access_token: "${EXPO_ACCESS_TOKEN}"   # the provisioner token
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning` | yes | The account and the token the engine manages each agent's robot with. Required when enabled: the integration receives nothing from Expo, so an enabled block without it would do nothing at all. |
| `provisioning.account_name` | yes | The Expo account or organization the robots are created on, as it appears in `expo.dev/accounts/<name>`: the name alone, with no `@`, slash or space, or a `${VAR}`. |
| `provisioning.access_token` | yes | The [provisioner token](#creating-the-provisioner-token). Keep it a `${VAR}` reference into the secret store; the dashboard seals it as `EXPO_ACCESS_TOKEN`. |

## Expo's plans worth knowing

These are Expo's, not Crewlet's. See [Expo's pricing](https://expo.dev/pricing) for the current figures.

- **Builds and updates an agent starts count against the account's plan**, the same as a person's. The Free plan has a fixed allowance of builds and lower build concurrency; paid plans include credit and then bill by usage. An agent on `runner` or above can therefore spend the account's allowance or its money, so give those levels to the agents that need them.
- **Protected channels and protected credentials** are Expo's way to keep production releases to a few people. Only `release_manager` can publish to a branch a protected channel points to; `developer` can still publish to every other branch.
- **Some MCP tools are billed or need a paid plan.** The simulator and sandbox tools of Expo's MCP server are billed, and `search_documentation` needs a paid plan.

## What is not supported yet

- **Inbound build and submit events.** EAS can post a webhook when a build or a submission finishes, but Crewlet has no route for them yet, so a finished build wakes nobody. An agent that wants a result reads it with `build:list` or `build:view`.
- **Each agent's own MCP sign-in.** Expo's MCP server issues its own OAuth tokens, and whether it accepts an Expo access token is undocumented, so it is not bound to each agent's robot. It acts as the person who signed it in.
- **A CLI command.** The integration is set up from the dashboard or the company document; there is no `crewlet expo` command.
- **Levels checked against a live account.** See [Each agent's access is a level](#each-agents-access-is-a-level).

## Troubleshooting

The card's findings, and what clears each:

| Finding | What it says | What clears it |
|---|---|---|
| `credential_missing` | `integrations.expo.provisioning`, its `account_name` or its `access_token` is missing or resolved to nothing | Fill in the field the finding names. For the token, seal it as `EXPO_ACCESS_TOKEN`, or point `access_token` at the variable that holds it |
| `credential_rejected` | Expo refused the provisioner token: it is wrong or revoked | Create a new token as described [above](#creating-the-provisioner-token) and replace it with **Replace key** |
| `grant_short` | The provisioner token lacks the Admin role on the account | Give the token's user or robot the Admin role in the account's members settings (the finding links there), or replace the token with one that holds it |
| `unknown_tier` | Account not found: no Expo account the token can see has the name in `provisioning.account_name` | Correct the name to the one in the account's `expo.dev/accounts/<name>` address |
| `identity_failed` | An agent's robot could not be created, re-levelled or deleted, or its token could not be created, rotated or deleted. Expo's reason is in the detail, and the finding links to the account's access tokens | Fix what the reason names, usually the provisioner's role, then **Retry** on the agent's row |

An agent's own failures are also reported on that agent's row on the **Agents** tab, with the control that answers them.

When an agent reports an error from the EAS CLI:

| The CLI says | Why | What to do |
|---|---|---|
| `EAS project not configured` | The project in the agent's working directory is not linked to EAS | A person links it once with `eas init` and commits the `extra.eas.projectId` it writes |
| A permission error on `build`, `update` or `submit` | The agent's level does not allow the action | Raise the agent's level on the Agents tab, if it should be allowed |
| A permission error publishing to `production` (or another protected channel) | The channel is protected, and only `release_manager` can publish to it | Publish it yourself, or give one agent `release_manager` |
| `EXPO_TOKEN` is not set, or the CLI asks for a login | The seat's `sandbox.env` does not set `EXPO_TOKEN`; the Agents tab shows its tool as not set up | Add `EXPO_TOKEN: "${EXPO_TOKEN_<HANDLE>}"` to the seat's `sandbox.env`, as in [The agent's Expo tools](#the-agents-expo-tools) |
| An authentication error | The agent's token was deleted or rotated at Expo by somebody else | The next pass replaces it; **Retry** on the agent's row does it now |
