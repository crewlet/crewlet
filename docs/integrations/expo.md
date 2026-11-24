# Expo

Each agent gets its **own Expo robot user** on the company's Expo account, at the role you choose, with its own access token. It reads EAS builds, submissions and updates as itself and, where its role allows, starts builds and publishes updates, through the EAS CLI in its container.

Expo creates robots only in its website, so **you create each robot there and paste its token**. Crewlet checks and seals it, and reads everything about the robot with it. Crewlet holds no Expo token of its own. Nothing receives events from Expo yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

Each level is one of Expo's robot roles; Crewlet compares a robot by its role:

| An agent on | Expo role | What Expo says it allows |
|---|---|---|
| `viewer` (the default) | Viewer | View all projects |
| `runner` | Runner | View all projects and perform low-risk actions on them |
| `developer` | Developer | Create builds, publish updates to unprotected channels and manage credentials |
| `release_manager` | Release Manager | Everything a Developer does, and publish to protected channels |

No agent holds Expo's Admin or Owner role. Each robot is the agent's own, so Expo names the agent behind each build and update, and turning one agent off never touches another.

Whether a Viewer can list EAS builds through the CLI is not yet checked against a live account. If an agent on `viewer` is refused listing builds, move it to `runner`.

**Developer can publish to every unprotected channel, and manage credentials.** Before giving an agent `developer`, protect channels it must not reach (production), and turn on credentials protection if its builds should not use your signing credentials, both under Expo's [protected resources](https://docs.expo.dev/eas/protected-resources/).

## Setting it up from the dashboard

Company › Integrations › Expo asks one thing: the **account name**, as in `expo.dev/accounts/<name>`. Nothing is tested here; the first agent's token proves it.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

## Giving each agent its own Expo robot

The integration's **Agents** tab (or the agent's own page) turns a seat on or off and sets its level, writing the seat's `roles[].integrations.expo` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has a robot. Required in every block. No block means off; a new seat is off. |
| `level` | `viewer`, `runner`, `developer` or `release_manager`. Unset is `viewer`. |

```yaml
roles:
  - handle: qa
    integrations:
      expo:
        enabled: true
        level: runner
```

Crewlet puts the token in the agent's box as `EXPO_TOKEN` itself ([The agent's Expo tools](#the-agents-expo-tools)).

### Each agent's access is a level

| Level | Expo role |
|---|---|
| `viewer` | Viewer (`VIEW_ONLY`) |
| `runner` | Runner (`RUNNER`) |
| `developer` | Developer (`DEVELOPER`) |
| `release_manager` | Release Manager (`RELEASE_MANAGER`) |

Crewlet reads the robot's role with its own token and compares it with the seat's level, not the permissions behind it. Admin, Owner and custom roles are refused. Expo carries the role only on its deprecated `viewerUserPermission.role` field (see [Troubleshooting](#troubleshooting)); where it is absent Crewlet refuses rather than guess from permissions.

### Setting a robot up

The robot is the agent's own identity at Expo, set up on the same sheet every vendor's identity uses ([ADR-0179](../../adr/0179-a-seat-may-hold-its-own-identity-at-a-growth-vendor.md)). The agent's row shows **Needs you** and its box a **Set up identity** button until done:

1. **You:** on `expo.dev/accounts/<name>/settings/access-tokens`, choose **Add robot**. Name it as the sheet shows, `crewlet-<handle>` (from the one rule Crewlet names every agent identity by, at most 30 characters), with the role the level names.
2. **You:** on the robot's row, choose **Create token**. Expo shows the value once.
3. **You paste it, and Crewlet checks** before sealing:
   - the token works;
   - it belongs to a robot, not a person;
   - the robot holds a role on this account;
   - the role is the agent's level, not Admin or Owner;
   - the robot is not another agent's.

   A passing token is sealed as `EXPO_TOKEN_<HANDLE>` (for example `EXPO_TOKEN_QA`) and the robot's ID and name recorded. A failing one is refused beside the field with what to do, and nothing is stored. Pasting needs the `secrets.write` authority and a recent sign-in; the audit records the robot and the token's visible prefix, never its value.

The name is a label; Crewlet identifies a robot by the ID its token proves.

### What each change does

| You do | What happens |
|---|---|
| Turn a seat on | The three steps above. |
| Change its level | Crewlet cannot change a robot's role, so the row asks you to use **Edit robot** at Expo. The next pass, or **Check now**, reads it back. |
| Turn it off | Crewlet clears `EXPO_TOKEN_<HANDLE>` at once. The row then asks you to **Delete robot** at Expo, and goes quiet once Expo stops accepting the old token. **Done, check again** reads it now; while the robot exists the row says so and links to Expo. |
| Keep the robot at Expo instead | Choose **Keep the robot at Expo**. The step closes, Crewlet stops tracking the robot, and the audit records your choice. |
| Turn it back on before deleting the robot | The turned-off robot's token is forgotten, never reused. Paste a new token, for the same or a new robot. |
| Rotate its token | Paste a new token created on the same robot's row. Crewlet checks it is the same robot, seals it, and asks you to delete the robot's other tokens at Expo. |

Until a turned-off robot is deleted, Crewlet keeps its token as `EXPO_RETIRED_TOKEN_<HANDLE>`, only to see when Expo stops accepting it. Nothing hands it to an agent: the company document is refused if a seat names it anywhere. It is cleared when Expo stops accepting it, when the seat is turned back on, when you keep the robot, and on disconnect.

**A robot's other tokens are asked for until they are gone.** A robot may hold live tokens besides the sealed one (made before the first paste, or the old one after a rotate). Every pass lists them on the row by Expo's prefix and creation time and asks you to delete them until Expo shows them revoked or deleted.

### The token is named for the seat

The engine derives the sealed name, `EXPO_TOKEN_<HANDLE>`; the company document never names it, so nothing there can point a seat's token at another variable. Expo documents no token expiry, so nothing rolls them on a schedule. **Rotate credential** on the Agents tab replaces a value you believe leaked.

### The agent's Expo tools

**The EAS CLI, as itself.** The EAS CLI reads `EXPO_TOKEN` in place of a login. While an agent is on, Crewlet sets `EXPO_TOKEN` in its box to its own sealed token, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)). An agent turned off gets none from its next run. Until a token is pasted, a run starts without it and says it is waiting for the Expo integration (`sandbox_env_unresolved`, at info; a warning where the integration cannot say whether it holds the agent up, unless this node is only still reading its findings: [`unsure`](../concepts/code-sandbox.md#failure-modes)); the Agents tab shows per seat whether the tool is live or waiting. A seat's own `sandbox.env` or setup line that still sets `EXPO_TOKEN` is ignored for the agent's token and reported as an advisory at that line. With the token, an agent runs commands such as these with no `eas login`:

```sh
npx eas-cli@27.2.0 build:list
npx eas-cli@27.2.0 build:view <build-id>
npx eas-cli@27.2.0 update:list
npx eas-cli@27.2.0 submit --platform ios --latest
```

Each names the one EAS CLI release Crewlet pins, so every agent runs the same release and `npx` fetches it once. A command acts on the working directory's project, which must already be linked to EAS (`extra.eas.projectId` in its app config); a person links it (see [Troubleshooting](#troubleshooting)).

**Expo's hosted MCP server.** `https://mcp.expo.dev/mcp` (Streamable HTTP) lists, starts and cancels builds and workflows, reads their logs, and reads crash reports and store reviews. Add it from Company › Tools › MCP servers, where the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) offers it as `expo`; a member of the company's Expo account signs it in with OAuth. It is a company-wide grant: every agent granted it acts through that person's account, not its own robot. Its simulator tools start sessions Expo bills, and `search_documentation` needs a paid plan.

### Every pass reads each robot back

Each pass reads every robot with that agent's own token; Expo is never called with any other.

- **A token revoked or deleted at Expo** answers 401. Crewlet clears it, never sends it again, and the row asks for a new token: **Set up** opens at the paste step. A token for a new robot is accepted, since the old robot may be gone.
- **A role changed at Expo** by somebody else is drift Crewlet cannot undo. The row names both roles and asks you to change it back with **Edit robot**, or to choose the robot's role here.
- **A robot that lost its role on the account** is treated as turned off by hand; a pass never acts on it, in case it was incident response, and the tab offers **Turn back on** or **Keep off**.
- **A seat that leaves the document** is treated as off.

A refusal names the failed step, Expo's reason, what to do and who does it. **Check now** runs the pass for one agent after a step at Expo.

### Disconnecting

Disconnecting turns every agent off and clears their tokens at once. Crewlet stops tracking the robots, so delete them on the access tokens page at Expo. Builds and updates a robot made stay.

## Let the atlas read the account

Optional. With only the account's name, the company's [atlas](../concepts/atlas.md#what-integrations-read) shows Expo as one account. To see its projects and whether builds pass, give the atlas a view-only robot of its own. No agent needs it or waits on it, and no agent's robot is used for it.

**The one step.** On the account's **Settings › Access tokens** page at Expo, add a robot named `crewlet-atlas` with the **Viewer** role, create a token on its row, and paste it under **Company › Integrations › Expo › Atlas robot token**. Press **Test**. Crewlet seals it as `EXPO_ATLAS_TOKEN` and reads the account within a minute, then every hour (the company's `atlas.refresh_interval_seconds`).

**What the atlas gains.** Each project as a service, by slug, with its name, belonging to the account and built from its linked GitHub repository; and, per platform (Android, iOS) the project was ever built for, a pipeline with its latest build's result (queued, running, passed, failed or cancelled).

**What is never read.** One GraphQL query names every field: a project's id, slug, name and linked repository, and a build's platform and status. No build credential, secret, environment variable, log, message or artifact is asked for.

**Only a Viewer robot is read with.** Before every read, and on Test, Crewlet reads who the token is and its role, as for an agent's robot. A person's token, any other role, no role on the account, and a robot holding a token sealed for an agent are each refused, and the atlas's Sources page says what to change.

**What Expo shows.** The robot is an actor of its own: reads appear as `crewlet-atlas`, never an agent's robot.

**To undo it.** Delete the robot `crewlet-atlas` at Expo and clear the field (or remove `integrations.expo.atlas` from the company configuration). The atlas stops reading; what it recorded stays, each entry showing when last seen, until a person retires it.

**Cost.** Two calls that prove the robot and one per fifty projects. A `429` is waited out; a listing failing part way stops the read, which records nothing and retries five minutes later.

## Configuration

```yaml
integrations:
  expo:
    enabled: true
    provisioning:
      account_name: example-org
    atlas:
      token: ${EXPO_ATLAS_TOKEN}
```

| Field | Required | Meaning |
|---|---|---|
| `enabled` | yes | Turn the integration on. |
| `provisioning` | yes | The account the robots act for. Required when enabled: the integration receives nothing from Expo, so without it the block does nothing. |
| `provisioning.account_name` | yes | The Expo account or organization as in `expo.dev/accounts/<name>`: the name alone, no `@`, slash or space, or a `${VAR}`. Every robot must hold its role on it. |
| `atlas` | no | The atlas's own read of the account. Left out, the atlas shows the account alone. |
| `atlas.token` | no | A `${VAR}` naming the sealed token of a Viewer robot made for the atlas ([let the atlas read the account](#let-the-atlas-read-the-account)); the dashboard seals it as `EXPO_ATLAS_TOKEN`. Any other role, and any agent's robot, is refused. |

The block holds no agent credential; each token is pasted on the Agents tab.

## Expo's plans worth knowing

Expo's, not Crewlet's; see [Expo's pricing](https://expo.dev/pricing).

- **Builds and updates an agent starts count against the account's plan.** Free has a fixed build allowance and lower concurrency; paid plans include credit then bill by usage. An agent on `runner` or above can spend it, so give those levels only to agents that need them.
- **Protected channels and credentials** keep production releases to few people. Only `release_manager` can publish to a branch a protected channel points to; `developer` can publish to every other.
- **Some MCP tools are billed or need a paid plan.** The simulator and sandbox tools are billed; `search_documentation` needs a paid plan.

## What is not supported yet

- **Inbound build and submit events.** EAS can post a webhook when a build or submission finishes, but Crewlet has no route for it, so a finished build wakes nobody. An agent reads results with `build:list` or `build:view`.
- **Each agent's own MCP sign-in.** Expo's MCP server issues its own OAuth tokens and whether it accepts an Expo access token is undocumented, so it acts as the person who signed it in.
- **A CLI command.** Set up from the dashboard or company document; there is no `crewlet expo` command.

## Troubleshooting

A pasted token is refused beside the field, with what to do:

| The sheet says | Why | What to do |
|---|---|---|
| Expo did not accept this token | Wrong, revoked or deleted | Create a new token on the robot's row and paste it |
| This token belongs to the person … | Created under a person's account | Create it on the robot's row |
| Robot … holds no role on … | Another account's robot, or it lost its role | Add the robot on this account's access tokens page |
| This robot is a … on …, and you chose … | Its role is not the agent's level | **Edit robot** at Expo, or choose that role here |
| Agents never hold Admin (or Owner) | The robot holds Admin or Owner | Give it one of the four agent roles at Expo |
| This robot is already …'s | Another agent has it | Add a robot for this agent |
| This token belongs to robot …, not to …'s robot | A rotate's token is from another robot | Create it on the agent's own robot's row |
| The token has a space, line break or invisible character | Copied with a hidden character | Copy it again in one piece |
| Expo did not say this robot's role | Expo answered without the role, or no longer answers the deprecated `viewerUserPermission.role` field | Check the role at Expo and paste again; if it persists Expo retired the field and Crewlet needs an update |

The card reports `credential_missing` when `integrations.expo.provisioning.account_name` is missing or resolves to nothing; fill it in. An agent's own state is on its **Agents** row, with the control that answers it.

When an agent reports an EAS CLI error:

| The CLI says | Why | What to do |
|---|---|---|
| `EAS project not configured` | The working directory's project is not linked to EAS | A person runs `eas init` once and commits the `extra.eas.projectId` it writes |
| A permission error on `build`, `update` or `submit` | The role does not allow it | Raise the level on the Agents tab and change the robot's role at Expo to match |
| A permission error publishing to `production` (or another protected channel) | Only `release_manager` can publish to a protected channel | Publish it yourself, or give one agent `release_manager` |
| `EXPO_TOKEN` is not set, or the CLI asks for a login | The agent is off at Expo or no token is pasted; the Agents tab shows off or waiting | Turn it on and paste the robot's token; the next run carries it ([The agent's Expo tools](#the-agents-expo-tools)) |
| An authentication error | The token was revoked or deleted at Expo | The row asks for a new token; paste one |
