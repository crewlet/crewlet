# Expo

Expo gives each agent its **own Expo robot user** on the company's Expo account, at the role you choose, with an access token of its own. An agent reads EAS builds, submissions and updates as itself and, where its role allows it, starts builds and publishes updates, through the EAS CLI in its own container.

Expo lets robots be created only in its website, so **you create each agent's robot at Expo in a few clicks and paste its token**. Crewlet checks the token, seals it, and from then on reads everything about the robot with that token. Crewlet holds no Expo token of its own. Nothing here receives events from Expo yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

Each level is one of Expo's own robot roles, and Crewlet compares a robot by its role:

| An agent on | Expo role | What Expo says it allows |
|---|---|---|
| `viewer` (the default) | Viewer | View all projects |
| `runner` | Runner | View all projects and perform low-risk actions on them |
| `developer` | Developer | Create builds, publish updates to unprotected channels and manage credentials |
| `release_manager` | Release Manager | Everything a Developer does, and publish to protected channels |

No agent ever holds Expo's Admin or Owner role. Every agent's robot is its own, so Expo names the agent behind each build and update it starts, and turning one agent off never touches another.

Whether a Viewer can list EAS builds through the CLI is not yet checked against a live account. If an agent on `viewer` is refused when it lists builds, move it to `runner`.

**Developer can publish to every channel you have not protected, and manage credentials.** If an agent should not reach production, protect that channel, and turn on credentials protection if its builds should not use your signing credentials, both under Expo's [protected resources](https://docs.expo.dev/eas/protected-resources/), before you give the agent `developer`.

## Setting it up from the dashboard

Company › Integrations › Expo asks one thing: the **account name**, the Expo account or organization the agents work in, exactly as it appears in its address, `expo.dev/accounts/<name>`. There is nothing to test at this point. The first agent's token proves the name.

See [Setting an integration up](../reference/api-endpoints.md#setting-an-integration-up) for the routes behind it.

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
```

Nothing else is written for the agent: Crewlet puts the robot's token in the agent's box as `EXPO_TOKEN` itself ([The agent's Expo tools](#the-agents-expo-tools)).

### Each agent's access is a level

A level is one of Expo's robot roles, the same in every company:

| Level | Expo role |
|---|---|
| `viewer` | Viewer (`VIEW_ONLY`) |
| `runner` | Runner (`RUNNER`) |
| `developer` | Developer (`DEVELOPER`) |
| `release_manager` | Release Manager (`RELEASE_MANAGER`) |

Crewlet reads the robot's role on the account with its own token and compares it with the seat's level. It does not compare the permissions behind a role. A robot holding Admin, Owner or a custom role is refused. Expo carries the role only on its deprecated `viewerUserPermission.role` field, which Crewlet depends on (see [Troubleshooting](#troubleshooting)); where Expo does not say the role, Crewlet refuses rather than guess it from permissions.

### Setting a robot up

Turning an agent on draws three steps, two of them yours. The agent's row shows **Needs you** and a **Set up** button until they are done:

1. **You:** on the account's access tokens page at Expo (`expo.dev/accounts/<name>/settings/access-tokens`), choose **Add robot**. Name it as the sheet shows, `crewlet-<company>-<handle>` (`crewlet-<handle>` where the company has no name), and give it the role the agent's level names.
2. **You:** on the robot's row, choose **Create token**. Expo shows the value once.
3. **You paste it, and Crewlet checks it** before it seals anything:
   - the token works;
   - it belongs to a robot, not to a person;
   - the robot holds a role on this account;
   - the role is the agent's level, and is not Admin or Owner;
   - the robot is not another agent's.

   A token that passes is sealed as `EXPO_TOKEN_<HANDLE>`, and the robot's ID and name are recorded. A token that fails is refused beside the field with what to do, and nothing is stored. Pasting a token is a credential write: it needs the `secrets.write` authority and a recent sign-in, and the audit records the robot and the token's visible prefix, never its value.

The name is a label only. Crewlet identifies a robot by the ID its token proves, never by its name.

### What each change does

| You do | What happens |
|---|---|
| Turn a seat on | The three steps above. |
| Change its level | Crewlet cannot change a robot's role, so the row asks you to change it with **Edit robot** at Expo. The next pass, or **Check now**, reads the role back and the row goes in sync. |
| Turn it off | Crewlet clears `EXPO_TOKEN_<HANDLE>` at once, so the agent loses Expo straight away. The row then asks you to **Delete robot** at Expo, and goes quiet once Expo stops accepting the old token. **Done, check again** reads it now; while the robot still exists, the row says so and links to Expo. |
| Keep the robot at Expo instead | Choose **Keep the robot at Expo** on that row. The step closes, Crewlet stops tracking the robot, and the audit records it as your choice. |
| Turn it back on before deleting the robot | Crewlet forgets the turned-off robot's token, so it is never reused. Paste a new token, for the same robot or a new one. |
| Rotate its token | Paste a new token created on the same robot's row. Crewlet checks that it is the same robot, seals it, and asks you to delete the robot's other tokens at Expo. |

Until a turned-off robot is deleted, Crewlet keeps its token as `EXPO_RETIRED_TOKEN_<HANDLE>`, only to see when Expo stops accepting it. Nothing hands it to an agent: the company document is refused if a seat names it, in its box, its tools or anywhere else. It is cleared when Expo stops accepting it, when the seat is turned back on, when you keep the robot, and on a disconnect.

**A robot's other tokens are asked for until they are gone.** A robot can hold live tokens besides the one sealed for its agent: one made before the first paste, or the old one after a rotate. Every pass lists them on the row by the prefix Expo shows and when each was created, and asks you to delete them at Expo, until Expo shows them revoked or deleted.

### The token is named for the seat

The token's value is sealed under one name the engine derives for the seat, `EXPO_TOKEN_<HANDLE>` (for example `EXPO_TOKEN_QA`). The company document never names it, so nothing written to the document can point a seat's token at another variable. Expo documents no expiry for access tokens, so nothing rolls them on a schedule. **Rotate credential** on the Agents tab replaces a value you believe has leaked.

### The agent's Expo tools

An agent works with Expo two ways.

**The EAS CLI, as itself.** The EAS CLI reads an access token from `EXPO_TOKEN` in place of a login. While an agent is on at Expo, Crewlet sets `EXPO_TOKEN` in its box to the agent's own sealed token, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)). An agent turned off is given none from its next run, and Crewlet clears the sealed token at once. Until a token is pasted, a run starts without it and its start says it is waiting for the Expo integration (`sandbox_env_unresolved`, at info). The Agents tab reports per seat whether the tool is live or waiting for its token. A line of the seat's own `sandbox.env` or setup step that still sets `EXPO_TOKEN` is ignored for the agent's token, and reported as an advisory at that line ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)). With the token in its box, an agent runs commands such as these with no `eas login`:

```sh
npx eas-cli@27.2.0 build:list
npx eas-cli@27.2.0 build:view <build-id>
npx eas-cli@27.2.0 update:list
npx eas-cli@27.2.0 submit --platform ios --latest
```

Each command names the one EAS CLI release Crewlet pins, so every agent runs the same release and `npx` fetches it once. A command acts on the project in the working directory, which must already be linked to EAS (`extra.eas.projectId` in its app config). Linking a project is a person's job; see [Troubleshooting](#troubleshooting).

**Expo's hosted MCP server.** Expo's MCP server at `https://mcp.expo.dev/mcp` (Streamable HTTP) lists, starts and cancels builds and workflows, reads their logs, and reads crash reports and store reviews. Add it from Company › Tools › MCP servers, where the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) offers it as `expo`, and a member of the company's Expo account signs it in with OAuth. It is a company-wide grant: every agent it is granted to acts through the account of the person who signed it in, not through its own robot. Its simulator tools start sessions Expo bills, and `search_documentation` needs a paid plan.

### Every pass reads each robot back

Each pass reads every agent's robot with that agent's own token. Expo is never called with any other token.

- **A token revoked or deleted at Expo** answers 401. Crewlet clears it, never sends it again, and the row asks for a new token: **Set up** opens the same sheet at the paste step. A token for a new robot is accepted here too, since the old robot may be gone.
- **A role changed at Expo** by somebody else is drift Crewlet cannot undo. The row names both roles and asks you to change it back with **Edit robot**, or to choose the robot's role here.
- **A robot that lost its role on the account** is treated as turned off by hand. A pass never acts on it in case it was incident response, and the tab offers **Turn back on** or **Keep off**.
- **A seat that leaves the document** is treated as off.

A refusal names the step that failed, Expo's own reason, what to do about it and who does it. **Check now** runs the pass for one agent after you have done a step at Expo.

### Disconnecting

Disconnecting turns every agent off and clears their tokens at once. Crewlet then stops tracking their robots, so delete them on the account's access tokens page at Expo. Builds and updates a robot made stay on the account.

## Let the atlas read the account

Optional. With nothing more than the account's name, the company's [atlas](../concepts/atlas.md#what-integrations-read) shows Expo as one account. To see its projects and whether their builds pass, give the atlas a view-only robot of its own. No agent needs it, nothing waits on it, and no agent's robot is ever used for it.

**The one step.** On the account's **Settings › Access tokens** page at Expo, add a robot named `crewlet-atlas` with the **Viewer** role, create an access token on its row, and paste it under **Company › Integrations › Expo › Atlas robot token**. Press **Test**. Crewlet seals it as `EXPO_ATLAS_TOKEN` and reads the account within a minute, then every hour (the company's `atlas.refresh_interval_seconds`).

**What the atlas gains.** Each project as a service, by its slug, with its name, belonging to the account and built from the GitHub repository linked to it; and, for each of Android and iOS the project was ever built for, a pipeline carrying the result of its latest build (queued, running, passed, failed or cancelled).

**What is never read.** The read is one GraphQL query that names every field it is sent: a project's id, slug, name and linked repository, and a build's platform and status. No build credential, no secret, no environment variable, no build log or message and no artifact is asked for, so none is sent.

**Only a Viewer robot is read with.** Before every read, and when you press Test, Crewlet reads who the token is and what role it holds, as it does for an agent's robot. A person's token, a robot with any other role, a robot with no role on the account, and a robot that holds a token sealed for one of the agents are each refused, and the atlas's Sources page says what to change.

**What Expo shows.** The robot is an actor of its own on the account: what the atlas reads is read as `crewlet-atlas`, never as an agent's robot.

**To undo it.** Delete the robot `crewlet-atlas` at Expo and clear the field (or remove `integrations.expo.atlas` from the company configuration). The atlas stops reading the account. What it recorded stays, each entry showing when it was last seen, until a person retires it.

**Cost.** Two calls that prove the robot and one per fifty projects. A `429` is waited out; a listing that fails part way stops the read, which records nothing and is made again five minutes later.

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
| `provisioning` | yes | The account the agents' robots act for. Required when enabled: the integration receives nothing from Expo, so an enabled block without it would do nothing at all. |
| `provisioning.account_name` | yes | The Expo account or organization, as it appears in `expo.dev/accounts/<name>`: the name alone, with no `@`, slash or space, or a `${VAR}`. Every agent's robot must hold its role on this account. |

| `atlas` | no | The atlas's own read of the account. Leave it out and the atlas shows the Expo account alone. |
| `atlas.token` | no | A `${VAR}` naming the sealed access token of a robot made for the atlas, holding the Viewer role ([let the atlas read the account](#let-the-atlas-read-the-account)). The dashboard seals it as `EXPO_ATLAS_TOKEN`. A robot with any other role, and any agent's own robot, is refused. |

The block holds no agent's credential. Each agent's token is its own, pasted on the Agents tab.

## Expo's plans worth knowing

These are Expo's, not Crewlet's. See [Expo's pricing](https://expo.dev/pricing) for the current figures.

- **Builds and updates an agent starts count against the account's plan**, the same as a person's. The Free plan has a fixed allowance of builds and lower build concurrency; paid plans include credit and then bill by usage. An agent on `runner` or above can therefore spend the account's allowance or its money, so give those levels to the agents that need them.
- **Protected channels and protected credentials** are Expo's way to keep production releases to a few people. Only `release_manager` can publish to a branch a protected channel points to; `developer` can still publish to every other branch.
- **Some MCP tools are billed or need a paid plan.** The simulator and sandbox tools of Expo's MCP server are billed, and `search_documentation` needs a paid plan.

## What is not supported yet

- **Inbound build and submit events.** EAS can post a webhook when a build or a submission finishes, but Crewlet has no route for them yet, so a finished build wakes nobody. An agent that wants a result reads it with `build:list` or `build:view`.
- **Each agent's own MCP sign-in.** Expo's MCP server issues its own OAuth tokens, and whether it accepts an Expo access token is undocumented, so it is not bound to each agent's robot. It acts as the person who signed it in.
- **A CLI command.** The integration is set up from the dashboard or the company document; there is no `crewlet expo` command.

## Troubleshooting

A token you paste is refused beside the field, with what to do:

| The sheet says | Why | What to do |
|---|---|---|
| Expo did not accept this token | The token is wrong, revoked or deleted | Create a new token on the robot's row and paste it |
| This token belongs to the person … | It was created under a person's own account | Create the token on the robot's row |
| Robot … holds no role on … | The robot belongs to another account, or lost its role | Add the robot on this account's access tokens page |
| This robot is a … on …, and you chose … | The robot's role is not the agent's level | Change the role with **Edit robot**, or choose that role here |
| Agents never hold Admin (or Owner) | The robot holds Admin or Owner | Give the robot one of the four agent roles at Expo |
| This robot is already …'s | Another agent has this robot | Add a robot for this agent |
| This token belongs to robot …, not to …'s robot | A rotate's token is from another robot | Create the new token on the agent's own robot's row |
| The token has a space, line break or invisible character | It was wrapped or copied with a hidden character | Copy it again in one piece |
| Expo did not say this robot's role | Expo answered the robot's membership without its role, or no longer answers the deprecated `viewerUserPermission.role` field Crewlet reads it from | Check the robot's role at Expo and paste again; if it persists, Expo has retired the field and Crewlet needs an update |

The card reports `credential_missing` when `integrations.expo.provisioning.account_name` is missing or resolves to nothing; fill in the account name. An agent's own state is reported on its row on the **Agents** tab, with the control that answers it.

When an agent reports an error from the EAS CLI:

| The CLI says | Why | What to do |
|---|---|---|
| `EAS project not configured` | The project in the agent's working directory is not linked to EAS | A person links it once with `eas init` and commits the `extra.eas.projectId` it writes |
| A permission error on `build`, `update` or `submit` | The agent's role does not allow the action | Raise the agent's level on the Agents tab and change the robot's role at Expo to match |
| A permission error publishing to `production` (or another protected channel) | The channel is protected, and only `release_manager` can publish to it | Publish it yourself, or give one agent `release_manager` |
| `EXPO_TOKEN` is not set, or the CLI asks for a login | The agent is off at Expo, or no token is pasted yet; the Agents tab shows its tool as off or waiting | Turn the agent on and paste the robot's token; the next run carries it ([The agent's Expo tools](#the-agents-expo-tools)) |
| An authentication error | The robot's token was revoked or deleted at Expo | The row asks for a new token; paste one |
