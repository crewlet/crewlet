# Running Ask Crewlet

This guide is for the administrators and operators who turn
[Ask Crewlet](../concepts/assistant.md) on, choose the model it runs
on, keep it within budget, and answer compliance requests about it. What people
see when they use it is on the concept page; every configuration field is in
the [Configuration Reference](../getting-started/configuration.md#ask-crewlet).

## What you are turning on

Ask Crewlet answers a person's questions and prepares changes for that person
to approve, **as that person**. It holds no identity, permission or credential
of its own, so turning it on grants nobody any access they did not already
have:

- Each question derives a short-lived grant from the asking person's own
  browser session (for the turn's length at most, scoped to read methods). Every tool
  call is one ordinary API request sent with that grant through the engine's
  own handler, so the guard, the authority table and every route's own checks
  decide it exactly as they decide that person's clicks
  ([ADR-0050](https://github.com/crewlet/crewlet/blob/main/adr/0050-the-assistant-acts-only-through-a-grant-derived-from-the-persons-session.md)).
- A write is never sent by Ask Crewlet. It becomes a proposal the person
  approves from their own session, against a server-rendered preview, under a
  single-use grant bound to that one request
  ([ADR-0051](https://github.com/crewlet/crewlet/blob/main/adr/0051-an-assistant-write-executes-only-from-its-owners-approval-of-a-server-rendered-preview.md)).
- Ask Crewlet may do whatever its person may, as a read or as a change they
  approve, but for a short human-only list it never takes, whoever approves:
  signing in and the session and token ceremonies (minting or revoking a
  token, changing a second factor, issuing a password or invitation link,
  ending sessions), anything that carries a credential or a secret in clear
  (revealing or writing a secret, entering an integration's credentials, a
  frame's view ticket), its own approvals, and asking the knowledge base's own
  model (it searches the pages itself instead). It adds a seat to the org
  chart, but it does not create a person or invite one: the answer to either
  is a sign-in link, which stays with you, and the drawer says so
  ([ADR-0052](https://github.com/crewlet/crewlet/blob/main/adr/0052-every-authority-row-says-what-the-assistant-may-do-with-it.md)).
  A gesture that asks a recent proof of identity still asks it of the person
  when they approve.
- A conversation belongs to its owner. No route serves one to anybody else,
  except the audited compliance read below
  ([ADR-0053](https://github.com/crewlet/crewlet/blob/main/adr/0053-an-assistant-conversation-belongs-to-its-owner.md)).

Only a **person** signed in through a browser session may use it. Machine
tokens, service accounts and sessions exchanged from a Tier A token are
refused, and the dashboard shows them the reason.

### What it can read and change

Anything the person can reach through the engine's API. Beside its dedicated
tools, Ask Crewlet looks a route up in this reference
([API Endpoints](../reference/api-endpoints.md), embedded in the engine),
reads it with the person's own access, and proposes any change on it. The card
says what the change does in plain words, read from the resource as it is now
and what the change makes of it: "Add seat Technical Writer", "Remove seat QA
Engineer", "Adopt version 4 of the brand kit", with one row per field that
moves, each named by the seat or unit it belongs to ("Technical Writer ›
Goal"). A route that acts rather than writes a resource (a purge, a restore, a
move) is titled by what it does, as the route itself declares it: "Purge item
ENG-4", "Restore page Runbook", with everything the request sends as its rows,
a prune's cutoff included. A record is named by what people call it, read with
the person's own access where its address says nothing a person reads (a page
by its title, a chat room by its name), and in plain words ("the page") where
that read is not theirs, never by an internal id. A change that deletes
something, destroys it for good, removes a member such as a seat, or removes
fields is marked destructive: the card says what goes and whether the
configuration history can revert it. Nothing undoes a purge, erasing or pruning
a chat room's messages, or a room's horizon, so each of their cards leads with
what goes, read from what the change sends, and says it cannot be undone:
"Erase 2 messages from #general", "Delete what #general said before 1 January
2031", "Keep #general's messages for 7 days". Keeping a room's messages for
ever loses nothing, and is not marked destructive. A change that calls one of
the tools the dashboard's own controls send (a reaction in chat, say) is shown
as that call: titled by the tool ("React to message"), with the arguments it
sends. A call one of the dedicated tools below makes is prepared with that tool
instead, whose card reads what it changes. The exact request,
its body with secrets
masked, is under **Technical details** for an administrator; everybody else
reviews the rows, which are built from the same request. A change prepared against
a resource that has moved since is refused rather than overwritten. Before any
card is drawn, a route that can check a change without making it (every write
of the company document, `?dry_run=true`) is asked to, as the person: a change
the route would refuse goes back to Ask Crewlet to correct, and never reaches
the person to approve, and the warnings the change itself introduces are on
the card (one the company already carried is left off). A write the
history records carries a one-line summary of what it does, shown on the card
and stored with the revision. A change that replaces a whole object (a unit, a
seat, the charter) never removes a field by leaving it out: a field
Ask Crewlet drops without saying so is refused before the card, and one the
person asked to remove leads the card ("This removes ..."). A task or a
page that names no project or space
is shown, and sent, with the one it will land in, or refused when the person's
team owns none (ADR-0055). Adding a
seat to the org chart, editing a person's permissions, purging a task,
erasing or pruning a chat room's messages, setting a room's horizon or
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
| Post in a chat room, reply in a thread, send a direct message, edit or delete a message, change a room's topic, purpose or archive, change who is in a room, leave a room | People holding Chatlet | Chat's own write, as the person: the card names the room and who will read it, a membership change shows who joins and who leaves and takes out only the people Ask Crewlet names, deleting a message or taking somebody out of a room (yourself included) is marked destructive with what goes at the top, and a change made meanwhile to the room's settings or members, or to the message, makes the proposal stale (**Prepare it again**); a message said in the room does not |
| Change the company's mission, vision or standing policies | Administrators | `PUT /config/charter`, the route Company › Principles saves through: the card shows a word diff of the mission and vision and one row per policy, the approval asks the administrator to confirm it is them when their sign-in is not recent, and a change made to the principles meanwhile makes the proposal stale (**Prepare it again**) |
| Set or remove one token ceiling: a seat's or the company's, for a day, a week or a month | Administrators | The write Company › Budgets saves: the seat as read with only that window's ceiling changed (`PUT /config/roles/{handle}`), or a merge patch of the company's one window (`PATCH /config`). The card shows one row, the ceiling now and the new one, the history line and the route's warnings; the approval asks the administrator to confirm it is them when their sign-in is not recent, and a change to the configuration meanwhile makes the proposal stale. A lead changing a seat they lead goes through the generic change or Budgets itself |

A change to the screen the person has open (a filter, a range, a draft in an
open editor) is applied by the page itself and saves nothing. Each screen tells
Ask Crewlet what each of its changes takes (the keys a filter writes, the
seats and windows Budgets draws, the org builder's operations), and a change
that does not match is refused before any card is drawn, so the person never
approves a change the screen cannot apply. Such a change goes stale only when
what it depends on moves: opening a row's details or another editor beside it
does not.

## Turning it on

1. **Add a dedicated `providers.llm` entry** for Ask Crewlet that meets
   Ask Crewlet's rules: a CLI run with no tool of its own beside the engine's
   `crewlet` server, and
   `overrides: { web_tools: denied }`. Which CLIs qualify, and why the others
   are refused, is stated once in the
   [Configuration Reference](../getting-started/configuration.md#ask-crewlet)
   and in [Tool calls on this host](../concepts/llm-backends.md#tool-calls-on-this-host).
   A dedicated entry keeps its own `max_concurrent` slots, so a busy company's
   seats never queue people's questions and the reverse.
2. **Prove the entry** with `crewlet llm doctor assistant` (the entry's key),
   which runs the exact argv Ask Crewlet runs and reports a shell or a fetch
   that succeeded as a problem. It is the operator's proof before turning
   Ask Crewlet on.
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
somehow carries one still applies, so every seat keeps running, and
Ask Crewlet reports **No model** until the chain is fixed.

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

## Briefs

The drawer's **Brief me on this page** row (or "Brief me on this decision",
"this task", "this agent", "this room" where the screen has one record open)
asks a **brief turn**: an ordinary turn of a new conversation, started,
admitted, metered, recorded and streamed as any question is, that answers in a
fixed shape rather than in prose. What a person sees is on the
[concept page](../concepts/assistant.md#opening-it). What it means for you:

- **It only reads.** A brief turn is offered the person's read tools and the
  brief's own answer, never a write, a page action, a link of its own or deep
  work, and nothing it offers the person writes: an action opens a record's
  screen or asks a question in the same conversation.
- **Its verdict is counted, never written.** A brief answers what the page is
  about (the model's one `about` line) and whether the person has to do
  anything, which is the number of rows the engine kept, drawn once in the
  card's head ("Nothing needs you", "2 things need you"). Every row is
  something to do, ranked, the first the one to do first; a fact merely worth
  knowing goes in the optional `context` line, never a row
  ([ADR-0062](https://github.com/crewlet/crewlet/blob/main/adr/0062-a-briefs-verdict-is-its-rows-counted-and-every-row-is-something-to-do.md)).
- **Its rows are the engine's.** A row cites a task, a project, an agent, a
  page, an artifact or a chat room (by its id or its name, `#launch`, which
  the room's own read resolves), exactly the records whose screen the engine's
  link table opens through a read of the record. A row survives only when the
  record it cites appears in what this turn's own reads answered and the
  record's own read, sent again as the person, answers it; the address is the
  engine's link table's. A row the engine cannot check is dropped, never drawn, and counted
  in `crewlet.assistant.brief.rows_dropped`. A row is drawn danger only when
  that read carried the fact (a seat stopped or whose last turn failed, a task
  marked urgent).
- **Its answer is validated.** The model answers with one `answer_brief` call,
  held to its schema and bounds (an `about` line of at most 90 characters, at
  most three needs whose `do` is at most 80 and `why` at most 60, a `context`
  line of at most 100, at most two follow-ups and ask questions of at most
  80). Every cap is in the tool's schema (`maxLength` in characters,
  `maxItems`) and stated in the brief's prompt, so the model meets it before
  it writes. One the engine refuses is answered with every fault at once and
  written again once; a second refusal, or a refusal in the turn's last round,
  ends the turn `brief_invalid` with nothing drawn. Each refusal is logged as
  `assistant_brief_refused` (turn, attempt, whether it was final, and the
  reason, which names fields and counts, never the brief's words), and a
  `brief_invalid` turn's `assistant_turn_ended` line carries the last reason.
- **It is scoped by the page context.** The page context's `selection` (the
  row a screen has open, the peeked row, or the record a record's screen is
  of) limits the brief to that record; with none, the brief is of the page.
- **Its spend is its own.** Its rounds are recorded under the aux purpose
  `assistant_brief`, charged to the person's and the company's windows like
  any turn's, and its own cap below stops a brief that sets out to read the
  company. Its turns are stamped with prompt version `assistant.brief.v5`.
- **A card it cannot draw reads as a note.** A stored card in a shape this
  version's contract refuses (any brief written before prompt version
  `assistant.brief.v3`, or a newer node's card read by an older one during a
  rolling upgrade) is read back as a notice, "This answer can't be shown
  here. Ask again to see it now.", in the transcript, in a card read and in
  what the model re-reads. Each is logged as `assistant_card_shown_as_notice`
  (the card id, its type and the reason, never its contents) and counted in
  `crewlet.assistant.cards.shown_as_notice`.

## Budgets and limits

Ask Crewlet's rounds are charged to the company's token budget windows like
any other spend, recorded under the operator stage and the asking person's
seat, so the [spend views](budgets-and-spend.md) show who used how much.
`assistant.token_budget` adds a per-person ceiling per day, week or month on
top of the company's own, edited on Company › Budgets under **Ask Crewlet, per person**; a raised ceiling holds from the next question once the node has
applied the revision, with no restart. A question whose budget is spent is
refused before its first round, and a turn whose budget runs out stops before
its next one. Either way the drawer says whose ceiling it is (the person's own,
or the company's), when its window resets, and that raising the ceiling clears
it sooner; to a reader who may raise it, it offers **Open Ask Crewlet budget** (or
**Open company budget**), which opens that ceiling on Company › Budgets, and it
tells anybody else who can. A refused question goes back in the box, ready to
send again; only the person's own question limit (`assistant_busy`) also holds
the box until its wait passes, since a raised ceiling clears a spent budget at
once.

These limits are fixed, not configured:

| Limit | Value | Why |
|---|---|---|
| Running turns per person | 2 | A third answers `429 assistant_busy`, so one person cannot occupy the model slots |
| Turns per person per minute | 20, burst of 5 | The same refusal; a loop in a browser cannot spend the budget in seconds |
| One answer per conversation | 1 | A second question in the same conversation waits for the first (`409 assistant_turn_running`). A question asked while a change waits for approval is answered, and the change stays approvable until it expires (`assistant.proposal_ttl_seconds`). A turn waiting on an approval ends as `expired` once every change it proposed has expired, within a minute even if nobody opens the conversation again |
| Open streams per person | 4 per node, 60 opens a minute (burst of 10) | A fifth stream, or a loop reopening one, answers `429 assistant_busy`, so one person cannot hold the node's connections or keep it unsealing a transcript |
| Tool result handed to the model | 16 KiB | Larger answers are cut for the model; the card still shows the snapshot |
| Card size | 1000 rows, 256 KiB | A larger table is cut with a notice and never presented as whole |
| Attachments | 4 per question, 40 per conversation, 5 MiB each, PNG, JPEG, WebP or GIF | The type is read from the file's bytes, never its name, so a page dressed as an image is refused (`415 assistant_file_refused`); past 40 a conversation answers `409 assistant_file_limit`, which bounds what one conversation keeps in the object store to 200 MiB for its retention |
| Files a deep-work run delivers | 10 per run, 100 per conversation, 5 MiB each | The same bound on what runs hand back |
| Rounds of a brief | 7 rounds of reads, or `assistant.max_steps` where lower, then 2 rounds kept for the answer and its one rewrite | A page's brief reads three to five things (often side by side) and answers. A read asked for in the rounds kept for the answer is not sent, and the answer to a read in the last round of reads tells the model the reads are spent, so an answer the engine refuses always has a round left for its rewrite |
| Tokens of a brief | 100,000, every round together | About five rounds of a 20,000 token context; past it the brief stops with "The brief read more than a brief may", which spends nothing of the company's windows beyond the rounds already made |

## Deep work

Deep work lets Ask Crewlet hand a large task to a coding agent with its own
tools (shell, files, web search and fetch, image reading) for up to twenty
minutes at a time (ADR-0054). It is off unless `assistant.deep_work` names an
entry:

```yaml
providers:
  llm:
    assistant-deep:
      agent: claude-code     # deep work runs the CLI's own loop and tools, in a box
      model: sonnet
      # run_in: e2b          # optional; see "Where a run happens"
assistant:
  enabled: true
  llm: [assistant]
  deep_work:
    llm: assistant-deep
    timeout_seconds: 1200    # 60..1200, the longest one run takes
    max_steps: 150           # 10..400 agentic rounds and Crewlet calls a task makes
    max_tokens: 3000000      # 10000..20000000 tokens a task may spend
```

The entry must be Claude Code: a run on the engine host is confined around that
CLI's own install and temporary directories, which no other CLI has been
measured against, and a run parked on a proposal is continued in that CLI's own
session after the approval. A run's
wall clock never passes the read grant of the turn it belongs to, which is why
twenty minutes is the ceiling; a longer task proposes, waits and continues on
the fresh grant the approval brings. Its tokens are charged to the person's and
the company's budgets like any turn's, and appear on the spend history as
`assistant_deep_work`.

### What a run can reach

Its only way into Crewlet is the person's own Ask Crewlet tools, served to it
over the engine's tool bridge, so every read is the person's request and every
change a proposal they approve. No API token, grant or engine credential is
placed in its box; it holds a bridge token valid for that run alone, which
stops working when the run ends. It does reach the web, always: a page can try
to steer it, and it could carry what it read out in an address it fetches.

### Where a run happens

A run goes to the first of these the company configures:

1. The deep-work entry's own `run_in`.
2. An `e2b` cell under `providers.sandbox`.
3. A `container` cell with `providers.sandbox.local.image`.
4. The engine host, **confined**. Every command the run starts runs inside the
   strongest boundary the host enforces: `sandbox-exec` on macOS, bubblewrap
   over namespaces on Linux, or Landlock applied by the `crewlet` binary
   itself. It cannot read or write the node's store and estate, the stream
   store, the log file, the config files the node was started from, any
   providers.llm login, any seat's box, or the operator's home directory, and
   it writes only in its own box. A host that can enforce none of these runs
   no deep work, and Ask Crewlet reports why. Install bubblewrap
   (`apt install bubblewrap`) or run on a kernel with Landlock, or configure a
   cell above.

   Its box is made under the company's box root, where a seat's `direct`
   boxes are made too: `providers.sandbox.local.state_dir`, else
   `$CREWLET_SANDBOX_LOCAL_HOME`, else `~/.crewlet/sandboxes`, in a directory
   of its own there (`<box root>/assistant/boxes/<id>`), which a seat's
   backend never lists or reaps. So what a run checks out, installs and builds
   lands on the volume you sized for boxes and never on the one holding the
   store and the stream ([Where a box writes](deployment.md#where-a-box-writes)).
   A revision that moves `state_dir` leaves a box kept for a proposal where it
   is: for as long as the node runs, the approval continues the run in it and
   its earlier box root stays denied to every other run.

A run dials `CREWLET_MCP_BRIDGE_URL`, the fleet's address for the tool bridge
([Code Sandbox](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)),
and its call is relayed to the node that opened its session wherever it lands.
Where every box runs on the engine host — a single node whose
`providers.sandbox` configures no `e2b` cell, such as one with only the
`direct` cell the [Quickstart](../getting-started/quickstart.md) sets up —
point that variable at the node's loopback address
(`http://127.0.0.1:<api.port>`; `[::1]`, `localhost` and a name under it,
such as `engine.localhost`, are loopback too). A confined run's bridge token
is then admitted from that host's loopback only, under a loopback host name or
a container runtime's name for the engine host, so a page in a browser on the
host cannot spend a leaked one under a name of its own, and it is never
relayed to another node. A run in a `container` box on the same host is
handed the runtime's name for the engine host in place of loopback
([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)) and is held to
loopback all the same, so it can do nothing a confined run cannot. A runtime
that delivers the box's calls over the host's loopback (Docker Desktop, and
Podman's machine on macOS and Windows) meets that hold, and the bridge admits
the runtime's name for the host those calls carry; one that delivers them from
its bridge network (Docker Engine and Podman on Linux) does not, so run deep
work `direct` or remote there.
Don't do this where seats run boxes on other machines: they are given the same
address, and a box on another machine cannot dial it.

## Retention and erasure

A conversation untouched for `assistant.retention_days` (30 by default, 1 to
365) is purged by the trash reaper. The setting is read from the revision the
fleet's activation pointer names, and only on a node that runs that revision:
a node that refused the fleet's revision, or booted on an offline import the
fleet has not taken, purges no conversation for its age until it runs the
fleet's, so a purge comes late rather than by a setting the company has since
changed. **Clear** in the panel purges one at once.
Removing a person from the directory erases every conversation they own in one
record. Messages, cards and previews are sealed under the fleet keyring in the
replicated estate, and nothing a person did not see is stored, apart from the
bounded context a turn waiting on an approval resumes from.

Files go with their conversation. An image a person attaches and a file a
deep-work run delivers are kept in the fleet's
[object store](../concepts/object-store.md) as one object each,
sealed under the same keyring with the conversation and the file bound in, so
the bucket holds none of them in the clear; the replicated row naming each
object is removed with the conversation, and the object store's collector
deletes the object at its next pass. A node without an object store keeps no
files: there Ask Crewlet takes no attachment and offers no deep work.
Downloading a kept file is always a download, never a page: it is served as an
attachment of the type its bytes were read as, with `nosniff`, a `sandbox`
content security policy and `Cache-Control: private, no-store`.

Tier A's `stream.assistant_log_max_bytes` bounds Ask Crewlet's state log (512 MiB unless set; see the
[Tier A reference](../getting-started/configuration.md#tier-a)). How long a
conversation is kept is the company's `retention_days`, not that bound.

## Compliance reads

An administrator may read another person's conversation for a compliance
reason. It is a deliberate, recorded gesture, never a browse:

- It is the verb `assistant.conversation.audit_read`: administrators only,
  with a recent identity proof (step-up), a person present, and never through
  Ask Crewlet itself, so a machine token and an assistant grant are both
  refused.
- It is an API call made from the administrator's own signed-in session:
  `POST /assistant/audit/conversations/{id}/read` with `{"reason": "..."}`.
  The reason is required (up to 500 characters). Conversation ids appear on
  the audit records below.
- The read is **recorded before the conversation opens**: a record in the
  conversation's own domain, which the owner's transcript lists (who read it,
  when and why), and an `assistant_conversation_read` runtime audit event
  naming the reader, the owner, the conversation and the reason.
- One file the conversation keeps is read the same way:
  `POST /assistant/audit/conversations/{id}/files/{file}/read` with the same
  body records the read beside the conversation first and answers the file as a
  download, and its audit event names the file (`file_id`) as well.

The route reference is in [API Endpoints](../reference/api-endpoints.md).

## Audit and monitoring

Ask Crewlet adds no hidden channel to the audit log. What it records:

| Event | When | What it carries |
|---|---|---|
| `operator_acted` | A person approved a proposal and the write was sent | The person as the author, transport `assistant`, and the conversation, turn, grant, session and proposal it came from; the conversation is also a listing tag |
| `assistant_turn_completed` | Each turn segment ends | The conversation, turn, end state, error code, prompt version, model, and every step's tool, route pattern, outcome, status and bytes; never an argument, an answer or the question |
| `assistant_conversation_read` | An administrator's compliance read | Reader, owner, conversation and reason |

A brief turn is an `assistant_turn_completed` like any other, told apart by its
prompt version (`assistant.brief.v5`), and its spend is recorded under the aux
purpose `assistant_brief`.

The `crewlet.assistant.*` metrics (turn duration, time to first output, turns
and steps by outcome, active streams, briefs written and refused, and brief
rows dropped) are listed in the
[metrics reference](../reference/metrics.md). A climb in refused steps is the
model asking for what its people may not read; a climb in failed turns is the
model chain or a route failing.

## Evaluating a model

`crewlet assistant eval -suite <dir>` runs a directory of evaluation cases
through Ask Crewlet's real orchestrator and catalogue in process, against a
company each case describes, and reports each case's pass rate. With no
`-model` it plays each case's own scripted rounds, which is deterministic,
calls no model, and is how the engine's own tests run the shipped suite. With
`-model <key> -paid` it asks the candidate model, so the same cases measure a
model before you switch to it; that calls a paid model once per round of every
trial, which is why it needs the explicit `-paid`. The shipped
`injected-instruction` case is the measurement the engine's tests cannot make:
whether this model, given an instruction hidden in a task, declines it. The
`brief-*` cases (`mode: brief`) measure the brief: an Inbox brief whose two
needs the engine checks and counts, an all-clear page that nothing needs, an instruction injected in a task, a
record gone before the brief, an over-long first answer whose one rewrite is
drawn, and an answer written exactly at its caps that is accepted first time. The flags and the suite format are in the
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
| Cannot be started (`model_unavailable`) | The CLI's binary on the engine host's `PATH`: `crewlet llm doctor <key>` names it |

**The model's state is checked before anyone asks.** When every model in
`assistant.llm` is known unusable, the drawer says so above its composer and
disables Send rather than letting a person ask and wait for a failure. What
the node knows comes from its own calls and configuration, never from a call
made to check: a spent plan until its reset, a login a call was refused on, a
CLI with nothing that signs it in, or a CLI that could not start. A login
written with `crewlet llm login`, or any call through the entry that answers,
clears it at once; with no new evidence the node tries the model again after
ten minutes, so a sign-in the engine cannot see never keeps Ask Crewlet away
for longer. An administrator is shown the command that fixes it, and anybody
else is told to ask an administrator. The codes are listed in the
[API reference](../reference/api-endpoints.md#how-a-turn-fails-on-the-model).
| Answers are slow | `crewlet.assistant.first_output.latency`, the entry's `max_concurrent`, and the CLI's own start time on the host |
