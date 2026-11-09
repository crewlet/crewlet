# The Setup Advisor

The setup advisor is one list of what your company's setup costs it, why each item matters, and the change that fixes it. It is for administrators. The engine builds each change; you preview it and send it. **The engine never applies a fix on its own**, because a grant, a model, a schedule or a policy is your company's decision.

```mermaid
flowchart LR
  D[Company document] -->|the same checks crewlet validate runs| A[GET /advisor]
  A -->|finding and its change| Y[You]
  Y -->|PUT the change with dry_run=true| P[Preview: the server's own answer]
  P -->|Apply: PUT again under the entity tag| C[New revision]
```

## Where you see it

**The go-live check** heads Settings › Advisor: the three things to do before your agents start, each done when nothing of its kind is left. Fix what blocks the company, look at what costs it, and read what is worth knowing. It is counted from the same answer as the list under it, and a part of the setup the server could not read leaves its step unsaid rather than done. When you create a company in the organization builder, its last step opens it.

**Settings › Advisor** lists every finding, the most costly first, with what the engine is handling listed apart so it never reads as yours to do. **Review the change** opens the preview: pick an option where there is more than one, and the screen asks the engine to check the exact write (the fields it changes, before and after, and any warning it would add). Nothing is saved until you press **Apply**, which saves it as a revision by you.

A finding that **blocks** the company and is yours to fix also waits in your **Inbox** under **Needs you**, and counts on **Home**, with what to do and a button that opens the advisor. It leaves both once the setup no longer raises it. Findings that cost or are worth knowing stay on the advisor alone, so they never crowd the decisions your agents are waiting on.

## What it lists

Every finding has a **title** (what is wrong), a **why** (what it costs), the **path** in the company document it sits at, a **severity** and its **fix**.

| Severity | Meaning |
|---|---|
| `blocks` | The company cannot work as configured. |
| `costs` | It works, and pays for the setting in spend, waiting or work nobody sees. |
| `note` | Worth knowing, and may be exactly what you meant. |

The advisor reads the same checks `crewlet validate`, every `/config` write and every node's apply log raise, so a finding here is never a second opinion. Three of them come with a change the advisor builds:

| Finding | When | The change it offers |
|---|---|---|
| No agent watches the engine's own alarms (`grant.health_unheld`) | No agent seat holds `health_tools`, so an alarm that fires on a server wakes nobody. | Give one agent `health_tools`: one option per agent seat, in the chart's order. No agent gets any other access. |
| An agent learns on the model it works on (`learning.auxiliary_model`) | A seat's reflection, summaries and memory (`llm_auxiliary`) run on the same model as its own work, so learning costs a share of its spend at that price. | Point `llm_auxiliary` at another `providers.llm` entry: one option per entry on a different model. With none, add a smaller model first. |
| A schedule runs twelve or more times a day (`schedule.frequent`) | Each fire starts a turn whether or not anything changed. A check that waits for an event, such as a deploy or a merge, is better as a work item dated for when to look. | Run it once a day at 09:00 on the days it runs now, or remove it. Its task stays as written. |

Every other warning the document raises is listed too, in the words `crewlet validate` prints, with the setting to open (`config.advisory`, `config.reference`, `config.admission`).

So is everything your integrations' last check reported (`integration.<kind>`, for example `integration.credential_rejected` or `integration.approval_required`), in the integration's own words. Who acts follows the integration's own verdict: a credential that was refused or is missing, an app waiting for you to approve its permissions, or a delivery path that cannot be set up is yours, with the step to take and, where the vendor names one, the page to take it on; an account the engine is still creating is listed apart as the engine's. If a server cannot read the integrations' state, the answer's `unread` names `integrations` and the screen says so, so a short list never reads as nothing to do.

## Previewing and applying a change

A change is one entity write: `entity` (`roles` or `units`), `id` (a seat's handle or a unit's id), `set` (the top-level fields to replace) and `summary`. To apply one:

1. `GET /config/{entity}/{id}`: the entity and its own `ETag`. Replace the fields `set` names.
2. `PUT /config/{entity}/{id}?dry_run=true` with that body and `If-Match` set to that `ETag`. The answer is the preview: whether the company would still be valid and any warning the write would introduce. Nothing is stored.
3. Send the same `PUT` without `dry_run`, with the `summary` as `X-Summary`. It is recorded as a revision by you, like any change you make.

If the entity changed after your read, the write is refused for its `If-Match` instead of overwriting the change; read it again. A company whose document is managed elsewhere (Tier A's `api.auth.company_writers`) refuses the write; make the same change in its source instead.

## What never happens on its own

The engine does not grant access, change a model, move or remove a schedule, edit a policy or close anything because of a finding. The advisor only reads, and its package has no way to write (ADR-0115).

## Reading it

`GET /advisor` takes `admin`. From a script, with an administrator's token in `CREWLET_API_TOKEN`:

```sh
curl -s -H "Authorization: Bearer $CREWLET_API_TOKEN" https://crewlet.example.com/advisor
```

The answer is `{"revision": "...", "findings": [...], "unread": [...]}`, the most costly finding first. See the [API reference](../reference/api-endpoints.md).
