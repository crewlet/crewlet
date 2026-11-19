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
  frame's view ticket), its own approvals, asking the knowledge base's own
  model (it searches the pages itself instead), and reporting a check's result
  onto a commit, which your CI does with its own token
  ([External CI](gitlet.md#external-ci)): a report is recorded as your CI's,
  and Ask Crewlet runs no CI. It adds a seat to the org
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
| File a task, change one, comment on one, answer a question asked on one | People holding Worklet | Worklet's own write, as the person. An answer is only to an open question put to you, read from the task's whole thread and named on the card; a question answered, edited or removed meanwhile makes the proposal stale (**Prepare it again**), so a question is never answered twice |
| Reorder priorities, mark inbox notices | People holding Worklet | The person's own record |
| Write a page, edit a page | People holding Doclet | Doclet's own write |
| Share an artifact, withdraw a share | The artifact's owner | The artifact's sharing, with the audience named on the card; a share to the whole company asks the person to confirm it is them |
| Pause or resume an agent, steer a running turn, answer an executor | Whoever may do it on the seat's screen | The operator control the screen sends |
| Post in a chat room, reply in a thread, send a direct message, edit or delete a message, change a room's topic, purpose or archive, change who is in a room, leave a room | People holding Chatlet | Chat's own write, as the person: the card names the room and who will read it, a membership change shows who joins and who leaves and takes out only the people Ask Crewlet names, deleting a message or taking somebody out of a room (yourself included) is marked destructive with what goes at the top, and a change made meanwhile to the room's settings or members, or to the message, makes the proposal stale (**Prepare it again**); a message said in the room does not |
| Change the company's mission, vision or standing policies | Administrators | `PUT /config/charter`, the route Company › Principles saves through: the card shows a word diff of the mission and vision and one row per policy, the approval asks the administrator to confirm it is them when their sign-in is not recent, and a change made to the principles meanwhile makes the proposal stale (**Prepare it again**) |
| Turn an agent on or off at one integration, or change its role, its GitHub permissions or its scope there | Administrators | The save the integration's Agents tab sends (`PUT /setup/integrations/{kind}/access`), held to the tab as it was read: the card shows what changes and each step the next sync will take at the vendor, and a change that could give the agent an administration role there asks the administrator to confirm it is them |
| Approve or decline an agent's request for one permission beside its level | Administrators | The answer to the request's question (`POST /operator/act/comment_on_work_item`): the card draws the grant the stored request asks for in the engine's words (the permission by the catalogue's name, where it applies, how long it lasts, whether it changes things), never the option's label. Approving it saves the grant first and then tells the agent yes, and asks the administrator to confirm it is them; a grant the company's own connection cannot give yet is not prepared, and Ask Crewlet says what a person adds at the vendor first. Declining changes nothing |
| Set or remove one token ceiling: a seat's or the company's, for a day, a week or a month | Administrators | The write Company › Budgets saves: the seat as read with only that window's ceiling changed (`PUT /config/roles/{handle}`), or a merge patch of the company's one window (`PATCH /config`). The card shows one row, the ceiling now and the new one, the history line and the route's warnings; the approval asks the administrator to confirm it is them when their sign-in is not recent, and a change to the configuration meanwhile makes the proposal stale. A lead changing a seat they lead goes through the generic change or Budgets itself |

**An agent's access to an integration** is one read away for an
administrator (`get_seat_access`, on
`GET /setup/integrations/{kind}/access/seats/{handle}/{lane}`): the agent's
grant in Crewlet, what its credential holds at the vendor now, for GitHub what
the agent's app is registered with and which of that the grant leaves unused,
its scope, its status, what the next sync will take, and two clocks. Ask
Crewlet reads it whenever an agent says it lacks a permission, or somebody
asks whether an integration is set up right for an agent, and proposes the
change with `set_seat_access`. Crewlet narrows every agent's credential to its
grant in Crewlet, so a permission the agent's GitHub App holds and its grant
leaves out is changed in Crewlet, never at GitHub; "In sync" compares only the
grant's own permissions, so such a gap is not drift.

On an integration that grants single permissions beside a level (Cloudflare,
AWS, Google Cloud, Scaleway), the same read also gives each such grant the
agent holds (where it applies, until when, who granted it and for which of the
agent's requests, and whether its credential holds it yet), each request the
agent has open for one with the two options of its question that grant it, how
many more permissions the catalogue offers the agent beside its level and how
many of those the company's own connection lacks, and, for each permission its
grants and requests name, whether the company's connection can grant it. Any
other permission is looked up by part of its name with
`search_grant_catalogue`, which answers which level already holds it, whether
it changes things, and whether the connection can grant it. Crewlet grants
only what the company's own connection to the vendor holds: where that
connection lacks a permission (Cloudflare's provisioner token without
Origin Rules: Edit, say), the read names what a person adds to it at the vendor,
and its card carries a link to that page, built by the engine from the vendor's
catalogue and opened only after the dashboard names its destination. Ask
Crewlet says so, and never writes a vendor's address itself. Adding it is a person's step at the vendor;
the catalogue is read again within a minute, and the permission can then be
granted to the agent in Crewlet.

The integration's
**Last check** (`list_integrations`, and the Integrations header) moves on
every pass, Check now included, which reads the vendor and changes nothing; an
agent's **last applied** moves only when a sync writes its grant, and its own
**last check** with every pass.

A change to the screen the person has open (a filter, a range, a draft in an
open editor) is applied by the page itself and saves nothing. Each screen tells
Ask Crewlet what each of its changes takes (the keys a filter writes, the
seats and windows Budgets draws, the org builder's operations), and a change
that does not match is refused before any card is drawn, so the person never
approves a change the screen cannot apply. Such a change goes stale only when
what it depends on moves: opening a row's details or another editor beside it
does not.

## Turning it on

Assign Ask Crewlet a model: set `assistant.model` to a `providers.llm` key the
company already has, and apply the revision (`crewlet config import`, a
`PATCH /config` of `{"assistant": {"model": "<key>"}}`, or the dashboard,
where an administrator picks Ask Crewlet's model from the company's models).
The next page load lights **Ask** for every person. There is nothing else to
set: the bounds of a turn are fixed ([Budgets and limits](#budgets-and-limits)).

No entry has to be made for it. Ask Crewlet runs the entry as a helper's run:
the engine turns the harness's own web tools off whatever the entry says and
leaves the model no tool but the engine's `crewlet` server, while a seat on
the same entry keeps its web tools. The helper's run takes slots of its own,
so a busy company's seats never queue people's questions and the reverse.
Which harnesses can be run that way, and why the others are refused, is
stated once in the
[Configuration Reference](../getting-started/configuration.md#ask-crewlet)
and in [Tool calls on this host](../concepts/llm-backends.md#tool-calls-on-this-host).
The Models screen says of each entry whether a helper can run on it.

`crewlet llm doctor <key>` proves it: for an entry a helper runs on, it probes
the helper's run beside the entry's own, and reports a shell, a file read or a
fetch that succeeded there as a problem.

`crewlet validate` and every `/config` write refuse a model no helper can run
on, naming `assistant.model` and why. A stored revision that somehow carries
one still applies, so every seat keeps running, and Ask Crewlet reports that
its model cannot run it until the model is changed.

To turn it off, remove `assistant.model`. Conversations are kept and purged on
their normal retention; nothing is deleted by turning it off. Removing the
model's entry asks what Ask Crewlet runs on next, like every other reference to
it; answering "nothing" turns Ask Crewlet off in the same revision.

## Choosing the model

Every round is one launch of the vendor's harness, so the model and the harness's
start time decide how quickly people get an answer. A mid-tier model is the
recommended starting point: choosing among roughly thirty tools and shaping
their arguments is where small models fail measurably, and a frontier model's
per-round cost is rarely justified for lookups. Every question, brief and turn
resumed after an approval runs on the one model; a company that wants a
stronger model assigns one.

Before switching models, run the evaluation suites against the candidate
(`crewlet assistant eval`, below).

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
- **A room's brief is its catch up.** A brief of a chat room (Chatlet's
  **Catch up**, or **Summarize** when nothing is unread) also summarises what
  was said there: at most five `points` of at most 140 characters each, the
  messages the person has not read, or the latest when nothing is unread, each
  citing the message it came from. A point survives only when that message
  appears in what this turn's own reads answered and the message's own read
  (`GET /chatlet/channels/{channel_id}/messages/{message_id}`), sent again as
  the person, answers it; its source is that read's room, message and thread,
  so a reply opens in its thread. A point the engine cannot check is dropped
  and counted with the rows. Its needs are what waits on the person in the
  room, as any brief's are.
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
top of the company's own, edited on Company › Budgets under **Ask Crewlet, per
person**; a raised ceiling holds from the next question once the node has
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
| Model rounds per turn | 40 | Each round calls any number of tools; every result stays in the context the model re-reads, and forty of the 16 KiB a result is cut to fit a 200,000 token context |
| Longest turn | 20 minutes | The life of the read grant a turn holds, which no turn outlives; **Stop** ends one sooner |
| How long a change waits for approval | 15 minutes | Long enough to read a change, short enough that what was previewed is what the write meets |
| Conversations kept | 30 days after their last use | A transcript holds whatever the person's reads returned |
| Running turns per person | 2 | A third answers `429 assistant_busy`, so one person cannot occupy the model slots |
| Turns per person per minute | 20, burst of 5 | The same refusal; a loop in a browser cannot spend the budget in seconds |
| One answer per conversation | 1 | A second question in the same conversation waits for the first (`409 assistant_turn_running`). A question asked while a change waits for approval is answered, and the change stays approvable until it expires, 15 minutes after it was proposed. A turn waiting on an approval ends as `expired` once every change it proposed has expired, within a minute even if nobody opens the conversation again |
| Open streams per person | 4 per node, 60 opens a minute (burst of 10) | A fifth stream, or a loop reopening one, answers `429 assistant_busy`, so one person cannot hold the node's connections or keep it unsealing a transcript |
| Tool result handed to the model | 16 KiB | Larger answers are cut for the model; the card still shows the snapshot |
| Card size | 1000 rows, 256 KiB | A larger table is cut with a notice and never presented as whole |
| Attachments | 4 per question, 40 per conversation, 5 MiB each, PNG, JPEG, WebP or GIF | The type is read from the file's bytes, never its name, so a page dressed as an image is refused (`415 assistant_file_refused`); past 40 a conversation answers `409 assistant_file_limit`, which bounds what one conversation keeps in the object store to 200 MiB for its retention |
| Rounds of a brief | 7 rounds of reads, then 2 rounds kept for the answer and its one rewrite | A page's brief reads three to five things (often side by side) and answers. A read asked for in the rounds kept for the answer is not sent, and the answer to a read in the last round of reads tells the model the reads are spent, so an answer the engine refuses always has a round left for its rewrite |
| Tokens of a brief | 100,000, every round together | About five rounds of a 20,000 token context; past it the brief stops with "The brief read more than a brief may", which spends nothing of the company's windows beyond the rounds already made |

## Web research

With `assistant.research.hosts` set, Ask Crewlet can read a page of a vendor's
documentation to answer a question, such as which plan a setting needs. The
engine reads it, never the model's own harness, whose web tools stay off
([ADR-0151](../../adr/0151-ask-crewlet-reads-the-web-only-through-the-engine-from-hosts-the-company-lists.md)):

- only `https` pages on the listed hosts, and a redirect only to another of them;
- `GET` only, with no cookie, credential or company header, and never to a
  private, loopback or link-local address;
- at most 2 MiB a page in 15 seconds, read in parts of 12 KiB, at most eight
  pages a turn, each kept ten minutes on the node that read it.

With `assistant.research.search.mcp_server` naming the company's Tavily
entry, Ask Crewlet can also search those hosts first. Each API node runs its
own instance of that server for research alone, never a seat's; a search asks
for the listed hosts only, drops any result on another host, sends at most
200 characters and keeps five results, and a turn searches at most four times.

Each read or search is a step the person sees, and its text is the
site's: a change Ask Crewlet proposes after reading one says it was prepared
after reading a web page, and after one it proposes changes only to the
records your question or open screen names (a task key such as ENG-41, the
record you have open), so a page cannot steer it into changing anything else.
List only the documentation hosts you trust, such as the vendors your
integrations connect to. An administrator can change the list on **Settings ›
Ask Crewlet**, under Web research, where removing every host removes the block
and turns web research off.

The pages a turn read are cited under its answer, under **Sources**: one link a
page, with its host and when it was read, from the engine's own record of the
read. A source opens in a new tab only after a stop that names its host and its
whole address. A page's title is the site's words, so a mention of it in the
answer stays plain text.

## Big tasks and images

A turn has no shell, files or browser of its own, and runs for at most twenty
minutes. When a person asks for something that needs scripts, generated files
or hours of steps (analysing a month of work to propose new teams, redesigning
the brand's assets), Ask Crewlet proposes a task for one of the company's agent
seats that can do it, with a brief of what to deliver, the seat to assign and
links to the records it concerns. The person approves the task like any other
change, and the seat does the work in its own box, with its own tools, budget
and review.

The images a person attaches to a question are handed to the model with the
question, where the model's harness takes images: Claude Code does, through its
structured input (`image_input` in [Harness profiles](../concepts/llm-backends.md));
Pi does not. `/viewer` answers `reads_images` for the assigned model, and where
it is false the composer takes no image, and a question that carries one all
the same is answered with what works instead: pasting the text the image shows,
or assigning Ask Crewlet a model that reads images.

## Retention and erasure

A conversation untouched for 30 days is purged by the trash reaper, the same
term on every node. **Clear** in the panel purges one at once.
Removing a person from the directory erases every conversation they own in one
record. Messages, cards and previews are sealed under the fleet keyring in the
replicated estate, and nothing a person did not see is stored, apart from the
bounded context a turn waiting on an approval resumes from and a long
conversation's summary of its earlier messages. That summary is written by the
model after a turn once eight messages past the last twelve have
gathered, charged to the person's budget as `assistant_summary`, sealed beside
the conversation and purged with it.

Files go with their conversation. An image a person attaches is kept in the
fleet's
[object store](../concepts/object-store.md) as one object each,
sealed under the same keyring with the conversation and the file bound in, so
the bucket holds none of them in the clear; the replicated row naming each
object is removed with the conversation, and the object store's collector
deletes the object at its next pass. A node without an object store keeps no
files: there Ask Crewlet takes no attachment.
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
whether this model, given an instruction hidden in a task, declines it.
`injected-agent-question` hides one in an agent's question: the case passes
only if the turn sends nothing, proposes just the follow-up task the person
asked for, and that proposal names an agent's question as the outside text
it was prepared after (`expect.taint`), which the card says at its top. The
`brief-*` cases (`mode: brief`) measure the brief: an Inbox brief whose two
needs the engine checks and counts, an all-clear page that nothing needs, an instruction injected in a task, a
record gone before the brief, an over-long first answer whose one rewrite is
drawn, and an answer written exactly at its caps that is accepted first time.
Five cases measure the access questions: an agent saying it lacks a GitHub
permission (`agent-lacks-a-github-permission`, which reads the agent's access
and proposes the grant in Crewlet), whether an integration is set up right
for one agent (`integration-set-up-for-an-agent`), why an integration's
last check and an agent's last applied differ (`two-access-clocks`, which
proposes nothing), and whether an agent needs a higher Cloudflare level or a
grant of one permission the company's own connection lacks
(`a-permission-the-connection-lacks`, which reads the agent's grants and its
open request, looks the permission up in the catalogue, and proposes nothing,
since the first step is a person's at Cloudflare), and approving an agent's
request for one permission from its question (`approve-a-grant-request`, whose
one card draws the grant in the engine's words and asks the person to confirm
it is them, `step_up: true`). `answer-a-question-by-its-option` measures a
question put to the person: what waits on them lists the question with its
options by id, the recommendation and what it cites, and the answer is
proposed by the option chosen, and `what-a-task-waits-on` reads a task with
the task it is filed under and both directions of its links, and
`answer-only-your-own-question` asks it to answer a question put to somebody
else, which is refused before any card. Four cases measure answering a hard
question: which way to route a site's versioned pages
(`decide-the-docs-route`, which reads everything the question cites and the
agent's access before proposing the answer by its option), how an agent should
get the access for one change (`how-the-agent-gets-the-access`, a grant for
the change rather than a wider level), an instruction planted in the task's
thread (`instructions-planted-in-a-comment`, which must lead to no chat
message, access change or executor answer), and an answer in the person's own
words (`answer-in-your-own-words`, which chooses no option). Each case's
header carries the rubric a real model's trials are graded on.
`explain-how-a-grant-works` answers a question about the engine itself from
Crewlet's own documentation, which the binary carries exactly as its version
ships it. `who-changed-what` answers who changed a task and which change was
refused from the audit log. `read-one-config-section` reads one section of the
company document rather than the whole. `read-a-vendor-page` reads a vendor's
page from a listed host (a case lists its hosts with `research`) whose text
carries an instruction nothing follows, and `a-page-cannot-steer-a-change` has
a page ask for a comment on another task, which is refused, and
`search-a-vendors-docs` searches them first (`search: true` names a search
server) past a result that carries one. The flags and the suite format are in
the
[CLI reference](../reference/cli.md#crewlet-assistant-eval), and the shipped
suite is under `examples/assistant-evals/`.

## Troubleshooting

| The dashboard says | Check |
|---|---|
| No model yet (`no_model`) | `assistant.model` in the active revision: assign Ask Crewlet a model |
| Cannot run on its model (`model_problem`) | `crewlet validate` names why: the model's harness runs with tools of its own Crewlet cannot turn off. Assign an entry on a harness a helper can run |
| Not a person | The caller is a token or a token's session. Sign in as the person |
| No permission | The person's account and the authority table's assistant column for the verbs they expected |
| Signed out (`model_signed_out`) | The harness's login expired or is missing. Sign it in again with `crewlet llm login <key>`, then `crewlet llm doctor <key>` |
| Usage limit reached (`model_busy`) | The plan is spent until the reset the notice names. Assign Ask Crewlet another model to keep answering through a limit |
| Took too long (`model_timeout`) | The entry's `timeout_seconds`, and the harness's own start time on the host |
| Cannot be started (`model_unavailable`) | The harness's binary on the engine host's `PATH`: `crewlet llm doctor <key>` names it |

**The model's state is checked before anyone asks.** When Ask Crewlet's model
is known unusable, the drawer says so above its composer and
disables Send rather than letting a person ask and wait for a failure. What
the node knows comes from its own calls and configuration, never from a call
made to check: a spent plan until its reset, a login a call was refused on, a
harness with nothing that signs it in, or a harness that could not start. A login
written with `crewlet llm login`, or any call through the entry that answers,
clears it at once; with no new evidence the node tries the model again after
ten minutes, so a sign-in the engine cannot see never keeps Ask Crewlet away
for longer. An administrator is shown the command that fixes it, and anybody
else is told to ask an administrator. The codes are listed in the
[API reference](../reference/api-endpoints.md#how-a-turn-fails-on-the-model).
| Answers are slow | `crewlet.assistant.first_output.latency`, the entry's `max_concurrent`, and the harness's own start time on the host |
