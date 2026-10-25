# Running the Dashboard Assistant

This guide is for the administrators and operators who turn the
[Dashboard Assistant](../concepts/assistant.md) on, choose the model it runs
on, keep it within budget, and answer compliance requests about it. What people
see when they use it is on the concept page; every configuration field is in
the [Configuration Reference](../getting-started/configuration.md#the-dashboard-assistant).

## What you are turning on

The assistant answers a person's questions and prepares changes for that person
to approve, **as that person**. It holds no identity, permission or credential
of its own, so turning it on grants nobody any access they did not already
have:

- Each question derives a short-lived grant from the asking person's own
  browser session (for the turn's length at most, scoped to read methods). Every tool
  call is one ordinary API request sent with that grant through the engine's
  own handler, so the guard, the authority table and every route's own checks
  decide it exactly as they decide that person's clicks
  ([ADR-0049](https://github.com/crewlet/crewlet/blob/main/adr/0049-the-assistant-acts-only-through-a-grant-derived-from-the-persons-session.md)).
- A write is never sent by the assistant. It becomes a proposal the person
  approves from their own session, against a server-rendered preview, under a
  single-use grant bound to that one request
  ([ADR-0050](https://github.com/crewlet/crewlet/blob/main/adr/0050-an-assistant-write-executes-only-from-its-owners-approval-of-a-server-rendered-preview.md)).
- The assistant may do whatever its person may, as a read or as a change they
  approve, but for a short human-only list it never takes, whoever approves:
  signing in and the session and token ceremonies (minting or revoking a
  token, changing a second factor, issuing a password or invitation link,
  ending sessions), anything that carries a credential or a secret in clear
  (revealing or writing a secret, entering an integration's credentials, a
  frame's view ticket), its own approvals, and asking the knowledge base's own
  model (it searches the pages itself instead). It adds a seat to the org
  chart, but it does not create a person or invite one: the answer to either
  is a sign-in link, which stays with you, and the drawer says so
  ([ADR-0051](https://github.com/crewlet/crewlet/blob/main/adr/0051-every-authority-row-says-what-the-assistant-may-do-with-it.md)).
  A gesture that asks a recent proof of identity still asks it of the person
  when they approve.
- A conversation belongs to its owner. No route serves one to anybody else,
  except the audited compliance read below
  ([ADR-0052](https://github.com/crewlet/crewlet/blob/main/adr/0052-an-assistant-conversation-belongs-to-its-owner.md)).

Only a **person** signed in through a browser session may use it. Machine
tokens, service accounts and sessions exchanged from a Tier A token are
refused, and the dashboard shows them the reason.

### What it can read and change

Anything the person can reach through the engine's API. Beside its dedicated
tools, the assistant looks a route up in this reference
([API Endpoints](../reference/api-endpoints.md), embedded in the engine),
reads it with the person's own access, and proposes any change on it: the card
shows the request, its body with secrets masked, and the resource as it is now
beside what the change makes of it, field by field (a seat added to a unit is
one added seat with its role and responsibilities). A change prepared against
a resource that has moved since is refused rather than overwritten. Adding a
seat to the org chart, editing a person's permissions, purging a task or
adopting a brand are all proposals the person approves, at the same step-up
the screen would ask.

The dedicated tools are shortcuts with cards and previews of their own, each
offered only to a person who could make the same change on its screen:

| Change | Who is offered it | On approval |
|---|---|---|
| File a task, change one, comment on one, answer a question asked on one | People holding Worklet | Worklet's own write, as the person |
| Reorder priorities, mark inbox notices | People holding Worklet | The person's own record |
| Write a page, edit a page | People holding Doclet | Doclet's own write |
| Share an artifact, withdraw a share | The artifact's owner | The artifact's sharing, with the audience named on the card; a share to the whole company asks the person to confirm it is them |
| Pause or resume an agent, steer a running turn, answer a coding run | Whoever may do it on the seat's screen | The operator control the screen sends |
| Change the company's mission, vision or standing policies | Administrators | `PUT /config/charter`, the route Company › Principles saves through: the card shows a word diff of the mission and vision and one row per policy, the approval asks the administrator to confirm it is them when their sign-in is not recent, and a change made to the principles meanwhile makes the proposal stale (**Prepare it again**) |

A change to the screen the person has open (a filter, a range, a draft in an
open editor) is applied by the page itself and saves nothing.

## Turning it on

1. **Add a dedicated `providers.llm` entry** for the assistant that meets the
   assistant's rules: text mode, a CLI run with no tool of its own, and
   `overrides: { web_tools: denied }`. Which CLIs qualify, and why the others
   are refused, is stated once in the
   [Configuration Reference](../getting-started/configuration.md#the-dashboard-assistant)
   and in [Tool calls in text mode](../concepts/llm-backends.md#tool-calls-in-text-mode).
   A dedicated entry keeps its own `max_concurrent` slots, so a busy company's
   seats never queue people's questions and the reverse.
2. **Prove the entry** with `crewlet llm doctor assistant` (the entry's key),
   which runs the exact argv the assistant runs and reports a shell or a fetch
   that succeeded as a problem. It is the operator's proof before turning the
   assistant on.
3. **Set the `assistant:` block**: `enabled: true` and `llm:` naming the entry
   (a fallback chain, first key first). The defaults for steps, turn timeout,
   proposal lifetime and retention suit most companies. Every key, its range
   and its default are in the generated
   [company schema](https://github.com/crewlet/crewlet/blob/main/schema/company.schema.json)
   (`crewlet schema company`) and described in the configuration reference;
   this page does not repeat them.
4. **Apply the revision** (`crewlet config import`, or the dashboard's
   configuration editor). The next page load lights **Ask** for every person.

`crewlet validate` and every `/config` write refuse a chain that breaks the
rules above, naming the member and the field to change. A stored revision that
somehow carries one still applies, so every seat keeps running, and the
assistant reports **No model** until the chain is fixed.

To turn it off, set `enabled: false` (or remove the block). Conversations are
kept and purged on their normal retention; nothing is deleted by turning it off.

## Choosing the model

Every round is one launch of the vendor's CLI, so the model and the CLI's
start time decide how quickly people get an answer. A mid-tier model is the
recommended starting point: choosing among roughly thirty tools and shaping
their arguments is where small models fail measurably, and a frontier model's
per-round cost is rarely justified for lookups.

Before switching models, run the evaluation suites against the candidate
(`crewlet assistant eval`, below). Keep `turn_timeout_seconds` at its default
of 900 seconds unless measurements on your hosts justify another value: a
turn of thirty steps is thirty-one rounds, and each round pays the CLI's start
time.

## Budgets and limits

The assistant's rounds are charged to the company's token budget windows like
any other spend, recorded under the operator stage and the asking person's
seat, so the [spend views](budgets-and-spend.md) show who used how much.
`assistant.token_budget` adds a per-person ceiling per day, week or month on
top of the company's own. A turn whose budget is spent stops before its next
round and tells the person when the window resets.

These limits are fixed, not configured:

| Limit | Value | Why |
|---|---|---|
| Running turns per person | 2 | A third answers `429 assistant_busy`, so one person cannot occupy the model slots |
| Turns per person per minute | 20, burst of 5 | The same refusal; a loop in a browser cannot spend the budget in seconds |
| One answer per conversation | 1 | A second question in the same conversation waits for the first (`409 assistant_turn_running`). A question asked while a change waits for approval is answered, and the change stays approvable until it expires (`assistant.proposal_ttl_seconds`). A turn waiting on an approval ends as `expired` once every change it proposed has expired, within a minute even if nobody opens the conversation again |
| Open streams per person | 4 per node, 60 opens a minute (burst of 10) | A fifth stream, or a loop reopening one, answers `429 assistant_busy`, so one person cannot hold the node's connections or keep it unsealing a transcript |
| Tool result handed to the model | 16 KiB | Larger answers are cut for the model; the card still shows the snapshot |
| Card size | 1000 rows, 256 KiB | A larger table is cut with a notice and never presented as whole |

## Retention and erasure

A conversation untouched for `assistant.retention_days` (30 by default, 1 to
365) is purged by the trash reaper. **Clear** in the panel purges one at once.
Removing a person from the directory erases every conversation they own in one
record. Messages, cards and previews are sealed under the fleet keyring in the
replicated estate, and nothing a person did not see is stored, apart from the
bounded context a turn waiting on an approval resumes from.

Tier A's `stream.assistant_log_max_bytes` bounds the assistant's state log (512 MiB unless set; see the
[Tier A reference](../getting-started/configuration.md#tier-a)). How long a
conversation is kept is the company's `retention_days`, not that bound.

## Compliance reads

An administrator may read another person's conversation for a compliance
reason. It is a deliberate, recorded gesture, never a browse:

- It is the verb `assistant.conversation.audit_read`: administrators only,
  with a recent identity proof (step-up), a person present, and never through
  the assistant itself, so a machine token and an assistant grant are both
  refused.
- It is an API call made from the administrator's own signed-in session:
  `POST /assistant/audit/conversations/{id}/read` with `{"reason": "..."}`.
  The reason is required (up to 500 characters). Conversation ids appear on
  the audit records below.
- The read is **recorded before the conversation opens**: a record in the
  conversation's own domain, which the owner's transcript lists (who read it,
  when and why), and an `assistant_conversation_read` runtime audit event
  naming the reader, the owner, the conversation and the reason.

The route reference is in [API Endpoints](../reference/api-endpoints.md).

## Audit and monitoring

The assistant adds no hidden channel to the audit log. What it records:

| Event | When | What it carries |
|---|---|---|
| `operator_acted` | A person approved a proposal and the write was sent | The person as the author, transport `assistant`, and the conversation, turn, grant, session and proposal it came from; the conversation is also a listing tag |
| `assistant_turn_completed` | Each turn segment ends | The conversation, turn, end state, error code, prompt version, model, and every step's tool, route pattern, outcome, status and bytes; never an argument, an answer or the question |
| `assistant_conversation_read` | An administrator's compliance read | Reader, owner, conversation and reason |

The `crewlet.assistant.*` metrics (turn duration, time to first output, turns
and steps by outcome, active streams) are listed in the
[metrics reference](../reference/metrics.md). A climb in refused steps is the
model asking for what its people may not read; a climb in failed turns is the
model chain or a route failing.

## Evaluating a model

`crewlet assistant eval -suite <dir>` runs a directory of evaluation cases
through the assistant's real orchestrator and catalogue in process, against a
company each case describes, and reports each case's pass rate. With no
`-model` it plays each case's own scripted rounds, which is deterministic,
calls no model, and is how the engine's own tests run the shipped suite. With
`-model <key> -paid` it asks the candidate model, so the same cases measure a
model before you switch to it; that calls a paid model once per round of every
trial, which is why it needs the explicit `-paid`. The shipped
`injected-instruction` case is the measurement the engine's tests cannot make:
whether this model, given an instruction hidden in a task, declines it. The flags and the suite format are in the
[CLI reference](../reference/cli.md#crewlet-assistant-eval), and the shipped
suite is under `examples/assistant-evals/`.

## Troubleshooting

| The dashboard says | Check |
|---|---|
| Turned off | `assistant.enabled` in the active revision |
| No model | `crewlet llm doctor <key>` for each `assistant.llm` member, and the engine log for the chain's refusal |
| Not a person | The caller is a token or a token's session. Sign in as the person |
| No permission | The person's account and the authority table's assistant column for the verbs they expected |
| Signed out (`model_signed_out`) | The CLI's login expired or is missing. Sign it in again with `crewlet llm login <key>`, then `crewlet llm doctor <key>` |
| Usage limit reached (`model_busy`) | The plan is spent until the reset the notice names. Add a second model to `assistant.llm` to keep answering through a limit |
| Took too long (`model_timeout`) | The entry's `timeout_seconds`, and the CLI's own start time on the host |
| Answer could not be read (`model_protocol`) | The model wrote its tool calls as markup or in another vendor's format, twice. The engine log's `assistant_model_protocol` line names the fault. Asking again usually works; if it keeps happening, put another model first in `assistant.llm` |
| Cannot be started (`model_unavailable`) | The CLI's binary on the engine host's `PATH`: `crewlet llm doctor <key>` names it |

**The model's state is checked before anyone asks.** When every model in
`assistant.llm` is known unusable, the drawer says so above its composer and
disables Send rather than letting a person ask and wait for a failure. What
the node knows comes from its own calls and configuration, never from a call
made to check: a spent plan until its reset, a login a call was refused on, a
CLI with nothing that signs it in, or a CLI that could not start. A login
written with `crewlet llm login`, or any call through the entry that answers,
clears it at once; with no new evidence the node tries the model again after
ten minutes, so a sign-in the engine cannot see never keeps the assistant away
for longer. An administrator is shown the command that fixes it, and anybody
else is told to ask an administrator. The codes are listed in the
[API reference](../reference/api-endpoints.md#how-a-turn-fails-on-the-model).
| Answers are slow | `crewlet.assistant.first_output.latency`, the entry's `max_concurrent`, and the CLI's own start time on the host |
