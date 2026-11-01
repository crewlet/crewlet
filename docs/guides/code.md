# Hosting Code

How to host your company's git repositories in the engine itself, give the
people who use them a credential, and clone, fetch and push. What a repository
*is* — rows on the code log, each push's pack one object in the object store, a
disposable cache on every node that serves git — is the [Code
Host](../concepts/code-host.md) page; this one is the steps.

It is not exclusive: the [GitHub](../integrations/github.md) and
[GitLab](../integrations/gitlab.md) integrations keep working beside it, and a
company may host some repositories here and keep others on a vendor.

---

## Turn it on

Set `code.native` in the company document and declare a repository:

```yaml
code:
  native: true
  repos:
    - name: web
      owner: { seat: founder }   # a seat's handle, or { unit: Engineering }
```

Apply it like any other change (`crewlet config import`, `PUT /config`, or the
dashboard). Then:

- **Every data node** commits the repositories' pushes, and every node that
  serves git — one whose Tier A [`node.roles`](../concepts/configuration.md#noderoles)
  names `git`, or names none, since an undeclared node has every role — builds
  the code host and starts serving `/git/`. A git node needs **git 2.55 or
  newer** on its host (the engine's image builds the newest release) and a
  place for its cache, sized by its Tier A [`git` block](../concepts/configuration.md#git); the
  [deployment guide](deployment.md#the-code-host) says how to size both.
- **On a node that booted with no company, or one running no native backend
  yet**, the activation that sets `code.native` starts the code host — no
  restart.
- **On a node whose native runtime is already running** (a company that has run
  the engine's own tracker or knowledge base on it), turning `code.native` on —
  or off — takes effect at **that node's restart**, and the node says so in its
  log as `native_backend_needs_restart`. Turned on, such a node answers git and
  `/code/` with a `503` naming `native_backend_needs_restart` until it
  restarts, and no `Retry-After`: waiting changes nothing, and another node
  serves it. Turned off, it keeps serving git, under the settings of the last
  revision that hosted code, until it restarts. Restart the nodes one at a
  time, as for any upgrade.
- **A node whose apply of the revision fails** — its code host could not be
  built: the cache directory is another program's, git is older than 2.55 —
  answers git and `/code/` with a `503` naming `config_apply_failed`, never that
  the company hosts no code; `GET /fleet` and the node's log say why, and a
  node still applying it answers `503 applying` with `Retry-After: 5`. A node
  whose apply failed before it could even read the revision — it could not
  fetch it from the coordination store, or open it with its key — answers
  `503 code_unknown`, saying it cannot tell whether the company hosts code
  rather than that it does, and with no `Retry-After` either. On `/code/` each
  is the error code of the API's refusal envelope; on a git route the same
  sentence ends with the code in brackets.

Turning `code.native` off later deletes nothing: the repositories, their rows
and their packs stay, served again by the revision that turns it back on.

## Declare repositories

Each entry under `code.repos` is one repository, and every activation makes the
fleet match the list:

```yaml
code:
  native: true
  deleted_retention_days: 7          # how long a removed repository stays restorable
  repos:
    - name: web                      # the address: /git/web.git
      owner: { unit: Engineering }
      visibility: unit               # only Engineering's seats and leads read it; unset is company
      default_branch: main           # what a clone checks out; unset is main
      description: The customer-facing web app.
      protect:
        - branch: main               # nobody force-pushes or deletes main
        - branch: release/**         # every branch inside release/ ...
          allow_delete: true         # ... may be deleted, never force-pushed
        - tag: "v*"                  # a release tag never moves
      path_owners:                   # whom a change touching these paths asks for a review
        - paths: ["web/**"]          # everything inside web/
          unit: Engineering          # its lead is asked
        - paths: ["**/*.sql", "deploy/**"]
          seat: dba                  # every SQL file, at any depth, and deploy/
    - name: legacy
      owner: { seat: founder }
      archived: true                 # read-only: takes no push
```

A name is lowercase letters, digits and hyphens, at most 63 — it is the
address, so **renaming a repository deletes it and creates another**. A rule
protects the branches or tags its pattern matches: without `allow_force` a
branch cannot be moved to a commit that does not descend from it, without
`allow_delete` it cannot be deleted, and `direct_push: none` freezes it — while
the default, `persons`, takes a person's push to it and never a seat's.
`crewlet validate` warns about every repository whose default branch no rule
protects, since anybody who may push could then force-push or delete it. Every
field, the pattern grammar and what each change does at its activation are in
[Declaring repositories](../concepts/code-host.md#declaring-repositories).

`visibility` decides which of the company's **seats** read a repository:
`company`, the default, is every seat, and `unit` only the seats the chart
relates to its owner — the owning unit's members and the leads above them.
It never narrows a person: you, and everyone with a token, read every
repository whatever it says ([Visibility](../concepts/code-host.md#visibility)).

`path_owners` names who owns which part of the tree: a unit, whose lead is
asked, or a seat, and the globs of the paths it owns. A glob starts at the
repository's root — `*.go` is the Go files at the root alone, `**/*.go` every
one, `web/**` everything inside `web/` — and a glob with no `*` names one
file. A path owner is **whom a change touching its paths asks for a review**,
and that is all it is: it reads and pushes the repository as its seat always
could, so owning `deploy/**` lets no seat push there that could not before
([Path owners](../concepts/code-host.md#path-owners)).

## Give each person a token

A person pushes with a token that acts as them — as the human seat they hold,
which every person does from the moment they join
([Humans in the Org Chart](../concepts/humans-in-the-org.md)) — so there is
nothing to bind. Their own **machine token** is that token: they mint it
themselves, signed in as themselves, either from the dashboard's Account page
(`#/account`, **Personal access tokens › New token**) or from a terminal:

```sh
crewlet iam token -login
```

which asks for their login, their password and, where they hold one, their
second factor, and prints the token once
([`crewlet iam`](../reference/cli.md#crewlet-iam)). Revoking it ends it, and
removing its owner ends every token they minted.

A **Tier A token** pushes too when the identity directory binds it to a human
seat: it acts under the login `token:<id>`, and

```sh
crewlet iam create -kind machine -login token:<id> -seat <handle> -grants none
```

binds it in one record — on a human seat nobody holds, since a person's seat
is theirs alone, so its pushes are that seat's rather than any person's
([Acting as your seat](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)).

Either way it is the same principal the dashboard's writes and an assistant's
operator tools act as, so a person's pushes are theirs: each is recorded as one
`operator_acted` event with transport `git`, naming the seat, its kind and the
credential the push came through (`pat:<credential id>` for a machine token,
`token:<id>` for a Tier A token), the repository and what became of the push,
and the repository's push history (below) shows the seat as `actor_seat`.

**A credential bound to no seat fetches only** — a Tier A token or a service
account the directory binds to none. It acts as no person, so a push with it
is refused at its first request, before anything is sent:
`fatal: remote error: this token is not bound to a person: …`. A pipeline that
only clones needs no seat.

## Clone, fetch and push

The address is the API's own, on `api.port` — never the public listener:

```sh
git clone https://crewlet.example.com/git/web.git
```

git asks for a user name and a password: the password is the token, and the
user name is ignored. So that git never asks, give it a credential helper.
The engine's own reads the token from your environment and keeps it nowhere
else — scoped to the code host's address, so the token is offered to it and
to nothing else, and configured **from empty**, so that address has this
helper alone:

```sh
export CREWLET_API_TOKEN=…
git config --global credential.https://crewlet.example.com.helper ''
git config --global --add credential.https://crewlet.example.com.helper '!crewlet code credential'
```

The empty value clears the helpers git would otherwise also ask, because git
hands a credential it was answered to every helper it lists to store: a
storing helper your system configures (osxkeychain on macOS, the credential
manager with Git for Windows) would keep the token, answer with it before this
helper is asked, and go on sending a rotated token's old value until a refusal
erased it. It answers only over HTTPS (or plain http to your own machine), and `store` and
`erase` do nothing, so the token's one home is the variable
([`crewlet code credential`](../reference/cli.md#crewlet-code-credential)). Your
platform's keychain works too (`git config --global credential.helper
osxkeychain`, `libsecret` or `manager`); `credential.helper store` keeps the
token in plain text in `~/.git-credentials`.

**In CI**, give each runner a token of its own. A token shared by many
runners works while their blobless checkouts' lazy fetches fit together in a
quarter of a node's room for large requests — 256 MiB on a four-core node —
and the checkouts past that wait their turn, up to 55 s, behind the ones
ahead of them. Those hold their room until the node has synced the repository
and git has read them, so on a node that has not served the repository
lately, whose cold sync can outlast that wait, the ones waiting past the
quarter fail: thirteen lazy fetches of 20 MB fill a four-core node's quarter,
and the fourteenth and every later one fail their checkouts. A token per
runner lifts that to the node's whole room, and routing each repository to the nodes that
hold it keeps the syncs warm ([Deployment](deployment.md)). A runner's token
is also its own turn when the node's transfer slots are all busy: a credential
bound to no seat — a Tier A token, or a service account's machine token — is a
`machine` in the node's [fair share](../concepts/code-host.md#fair-share),
one principal per credential. With people and machines both waiting, people
get about two thirds of what the slots spend and machines one third, and
among the machines the credential that has asked least goes first: so a
burst of CI clones or lazy fetches waits behind the runners that have asked
less lately, and behind people's clones until people have spent more than
twice what the machines have, when the machines' third comes round. A token
shared by every runner is one principal, so its whole burst waits behind
every machine credential that has asked less — on a busy node for as long as
ten seconds, when a request still without a slot is answered `503`. A
pipeline that pushes acts as the human seat it is bound to — one nobody else
holds — so the share counts it as that seat, among the people: its burst
still waits behind every person who has asked less lately.

Hand the token over without a prompt. The same helper, for one command,
through `-c`:

```sh
CREWLET_API_TOKEN=$CREWLET_TOKEN git -c credential.helper= \
  -c 'credential.helper=!crewlet code credential' \
  clone https://crewlet.example.com/git/web.git
```

(the empty `credential.helper=` first sets aside any helper the runner's own
configuration lists). Or as a bearer header, which every route accepts:

```sh
git -c "http.extraHeader=Authorization: Bearer $CREWLET_TOKEN" \
  clone https://crewlet.example.com/git/web.git
```

or as the password, through `GIT_ASKPASS` — a script git runs to answer its
prompts:

```sh
printf '#!/bin/sh\necho "$CREWLET_TOKEN"\n' > /tmp/askpass && chmod +x /tmp/askpass
GIT_ASKPASS=/tmp/askpass git clone https://crewlet.example.com/git/web.git
```

Fetch and push as you would anywhere. Protocol v2 — git's default since 2.28 —
is what a blobless clone (`git clone --filter=blob:none`) needs. A partial
clone may ask for `blob:none`, `blob:limit=<n>`, `tree:0`, `object:type=<type>`
or a `combine:` of them — the filters git can answer from a repository's bitmap,
where the node holds one. `sparse:oid` and `tree:<n>` past 0 are refused; a
blobless clone with a sparse
checkout reaches the same files ([the filters
served](../concepts/code-host.md#a-push-end-to-end)).

Repositories use git's default SHA-1 object format; a local repository created
with `--object-format=sha256` cannot push to one.

### What git tells you, and what to do

Every refusal names its reason in the line git prints — but for a fetch's
request refused in the middle of a transfer, where git prints only the HTTP
status and the node's log keeps the reason. The ones a person meets most:

| git prints | What to do |
|---|---|
| `repository "<name>" not found` | Check the name in `code.repos`. |
| `this token is not bound to a person` | Push with a token that acts as your seat — your own machine token (`crewlet iam token -login`, or the dashboard's Account page). |
| `(stale: refs/heads/main is at <id>; fetch first)` | Fetch, integrate, push again. |
| `(protected_force: …)`, `(protected_delete: …)`, `(protected_direct: …)` | The rule named says what it allows; a change to it is a change to `code.repos` for a declared repository, and a person's `code_protect_branch` for one a tool made ([Who may change a protection](../concepts/code-host.md#who-may-change-a-protection)). |
| `(ref_conflict: refs/heads/feature is in the way: …)` | A branch and a directory of branches never share a name (`feature` beside `feature/x`). Delete the one in the way in the same push — `git push origin :feature HEAD:feature/x` — or choose another name; a clone holding the old one fetches with `git fetch --prune`. |
| `(pending: durable at <position>, …)` | The push is recorded and was not applied on the node that answered within a minute. Run the same `git push` again: if it landed, git says `Everything up-to-date`. |
| `(unknown: outcome unknown (op …); …)` | The answer was lost — it may or may not have landed. Fetch, then push again. |
| `(not_applied: …)` | The push was recorded and every node dropped it: nothing landed. Push again. |
| `(compaction_behind: <repo> holds <n> live packs, …)` | The repository holds as many packs as one may until its maintainer — the git node that merges its packs and publishes its layers — merges them, which it does every minute: run the same `git push` again in a minute. If it persists, the node's `code_compaction_behind` alarm is up and its log says why ([A busy repository's packs are merged](#a-busy-repositorys-packs-are-merged)). |
| `… retry in 30 s …` (a `503`) | The node is draining or stopping: retry, and a balancer sends you to another. |
| `error: RPC failed; HTTP 503 …` then `fatal: could not fetch <id> from promisor remote` (the checkout of a blobless clone) | The checkout's one fetch for the files it lacks found the node's room for large fetch requests in use — it waited 55 s for its turn, or found no place to wait — and git does not retry it: run the checkout again. git prints no reason for a request answered mid-transfer; the node's `code_fetch_spool_unserved` line names the token and what held it back, and if it was the token's own requests, the token is shared by several CI runners — give each its own. |

An `ok` from the engine means the push's own record **has been applied** — it
is never said of a push that is merely on its way. Every other condition, the
exact line git prints and what to do about it is in [What git prints, and what
it means](../concepts/code-host.md#what-git-prints-and-what-it-means).

## Open a change from a push

A push opens a [change](../concepts/code-host.md#changes) — `web!42`,
proposing its branch into another — when it asks with a push option:

```sh
git push -o change origin feature
```

The change proposes `feature` into the repository's default branch, titled by
the subject of the commit you pushed, and git prints which one it opened, with
the link to its page on the dashboard — on `api.external_url`, the address you
reach the deployment at:

```text
remote: crewlet: change web!42 opened: https://crewlet.example.com/dashboard#/code/repos/web/changes/42
```

It opens **in the push's own record**, so the commit and the change land
together or not at all. More options shape it:

| Option | What it does |
|---|---|
| `-o change` | Asks for a change. The options below do nothing without it, so a target kept in your `push.pushOption` configuration waits for a push that asks. |
| `-o change.target=<branch>` | The branch it proposes into, by name (`main`) or in full (`refs/heads/main`): the repository's default branch unless you name one. |
| `-o change.title=<title>` | Its title, one line of at most 256 bytes: the subject of the commit you pushed unless you name one — or, for a commit with no message, the branch's name. |
| `-o change.description=<text>` | Its description, at most 32 KiB — one line, since git takes no line break in a push option. |
| `-o change.draft` | Opens it as a draft, not yet asking to be merged. |

- **Push the change's branch alone.** A change proposes one branch, so a push
  that asks for one sets exactly one; tags beside it are fine.
- **A branch heads one live change.** A push to a branch that already heads an
  open change revises it — `change web!42 updated`, and the same link — and
  opens no other: the options shape only a change a push opens.
- **The push lands whatever becomes of its change.** An option the engine does
  not read, a target that is no branch, or a branch already at its target's
  commit leaves the push to land with no change, and git says why in the same
  line, `change not opened: <code>: <why>` ([every line it
  prints](../concepts/code-host.md#when-a-push-asked-for-a-change)). Pushing the
  same commit again moves nothing, so fix what the line says and push the
  branch's next commit with `-o change`.
- **Every other option is read by nobody** — `ci.skip`, or any other a forge
  takes.

Whoever may push the branch may open a change from it: the push's own update
is the check ([A change a push opens](../concepts/code-host.md#a-change-a-push-opens)).

**Or with a tool.** A seat — and your own assistant, as you — opens a change
of a branch already pushed with `code_open_change`, naming the branch, a title
and, if it is not the default branch, its target. It carries the work item the
seat's turn is charged to unless the call names another — a native item's key
(`ENG-42`), any tracker's item as `<backend>:<id>`, or `none` — and asks the
`reviewers` it names once it is open. `code_update_change` changes one thing
about a change a call — what it says, whether it is a draft, its target — and
`code_close_change` closes one or, with `reopen`, opens a closed one again;
each names the `version` of the change its caller read, and a change that
moved since is refused `stale_version` rather than changed under them. Who may
is the [change's rule](../concepts/code-host.md#opening-editing-and-closing-a-change):
whoever may push its branch opens one, and its owner, a maintainer of the
repository or any person steers it ([the tools](tools-and-mcp.md#the-companys-code)).

## Review a change

A change is reviewed where it lives: on its own page in the dashboard's
**Code** workspace (`g o`), or through the code tools by a seat or by your own
assistant, as you. Each ask, review, comment and answer is one record of the
change's **conversation** ([Reviews and
findings](../concepts/code-host.md#reviews-and-findings)), and each record
tells the seats and people it concerns.

**Find what waits on you.** **Code › Changes** lists the open changes, and
**Waiting on** narrows them to the ones waiting on you, on a review, on a fix
or on a person. A record that concerns you leaves a notice in your
**Inbox** — the change, why you hear of it ("asked for your review", "asked
to look again") and where its merge stands — whose **Open the diff** opens the
change's page on its files. A seat is woken instead, told the change, why it
hears and the tool that answers ([Who a change
wakes](../concepts/code-host.md#who-a-change-wakes)).

**Ask for a review.** **Request review** on the change's page names
reviewers by handle, with a note; a seat or your assistant asks with
`code_request_review`, which also withdraws an ask. An author of the change,
anybody who reviews the repository and any person may ask, at most 32
reviewers at once. A person may always be asked; a seat only where its grant
lets it review the repository, and never an author of the change, whose
review would count for nothing ([Asking for
review](../concepts/code-host.md#asking-for-review)). A change whose
revision touches paths the repository's `path_owners` name asks their owners
itself, once its home has read the revision ([Path
owners](../concepts/code-host.md#path-owners)).

**Read it.** **Conversation** is the change's readiness, its description and
its timeline in the order its records landed; **Files** is a revision's
diff — the head unless you pick another, or what changed since an earlier
revision — with each finding under the lines it points at, and one whose
lines have changed since drawn above the diff, never dropped. A seat reads the
same with `code_get_change`, a part at a time ([Reading a
conversation](../concepts/code-host.md#reading-a-conversation)).

**Review it.** On the head revision, pick lines by their numbers and draft a
finding under them: how much it weighs (`blocker`, `major`, `minor`, `nit`),
what it is about, a title, a body and, on the new side, the lines you suggest
in their place. Drafts stay in the browser tab until **Approve**, **Request
changes** or **Comment** sends them all as ONE review with your summary — a
review and every finding it raises are one record, never half there. A seat
reviews with `code_review_change`. Anybody who reviews the repository, and
any person, may review — never an author of the change.

- **A request for changes stands until its reviewer reviews again.** A push
  answers none: the reviewer's next review, approving or asking again, does.
- **A review of a later revision carries what still stands.** It names the
  findings of yours that still stand and should — the page offers each,
  checked — and every other finding you raised on an earlier revision is
  superseded. A second review of the same revision adds to the first.
- **An approval carries to a later revision only where nothing it approved
  changed**: the same code, or the same diff, as a rebase that moved nothing
  near the change keeps it ([Merge
  readiness](../concepts/code-host.md#merge-readiness)).

**Answer the findings.** An author answers a finding `resolved`, `wont_fix`
or `disputed` with `code_resolve_finding` — a seat, or your assistant as
you — and says why in a reply on its thread, which the page offers under
every finding. A blocker an author answered still stands, for its raiser to judge: only its
raiser's dismissal — or a maintainer's, or any person's, each `dismissed`
through the same tool — or the raiser's review of a later revision that does not carry it clears it
([Findings](../concepts/code-host.md#findings)). **Apply suggestion**
(`code_apply_suggestions`) commits a finding's suggested lines onto the
change's branch as one commit of yours, against the head on screen, and
answers the finding resolved; it makes you an author of the change, so your
approval of it no longer counts ([A review's suggestions
applied](../concepts/code-host.md#a-reviews-suggestions-applied)).

**Discuss it.** A comment (`code_comment_on_change`) starts a thread — on a
line of a revision, or on the change as a whole — or replies to a thread's
first comment or to a finding: threads are one level deep. Whoever started a
thread, an author, a maintainer or any person resolves it ([Comments and
threads](../concepts/code-host.md#comments-and-threads)).

**See whether it is ready.** A live change's readiness is one judgement on
one snapshot: its head's facts read, no draft, an approval counted where its
branch's gate asks one, no blocker standing, a clean merge into its target,
and whatever else the gate adds — its first unmet precondition in one
sentence naming who can clear it ([Merge
readiness](../concepts/code-host.md#merge-readiness)). Its authors whom the
branch's gate lets merge it hear `ready_to_merge` once, when it turns ready —
or, where the gate lets none of them, the lead of the repository's owner
([Who a change wakes](../concepts/code-host.md#who-a-change-wakes)) — and
merge it as [Merging a change](#merging-a-change) says.

**Who heard about it.** The page's **Woke** tab is whom each record about the
change reached, newest first, each with the one reason they heard under; the
same answer is `GET /code/repos/{name}/changes/{n}/routing` ([A change's
questions](../reference/api-endpoints.md#a-changes-questions)).

### The Inbox card

A notice about a change opens in your Inbox as a card that leads with what
the change is FOR: its title, the work item it implements and the opening of
its description, then what the record said, then **Where it stands** — the
engine's sentence of where its merge stands and the agent findings that stand
open, by weight — and **Open the diff**, one press to its files.

While the change is live and its branch has a merge gate, the card also says
**Its gate**, read for the one card you have open (the list asks nothing per
card):

- **What its checks measured**: each check the branch requires, on the
  change's head at its latest attempt — a rerun stands in for the attempt it
  reran — with its state and how long it ran (`unit failed in 1m 4s`,
  `e2e running`) — durations, never tokens. A branch that requires no check
  draws no list, rather than "none measured".
- **Its change classes**: how many of the paths the head revision touches
  change only whitespace or only comments, and — where every path was
  classified — that the rest change code
  ([Change classes](../concepts/code-host.md#change-classes)).
- **The human paths that asked for you**: where the gate holds a path the
  change touches to a person's approval and waits on you, the engine's
  sentence naming the globs and the branch. A person it waits on sees it;
  nobody else's card says it.

A merged or closed change has no gate left to meet, and its card asks for
none.

## Merging a change

A ready change is merged with `code_merge_change`, naming the change and the
head you read (`expected_head`) — by a seat, or by your own assistant or the
dashboard, as you. The code host builds the merge on the repository's home git
node onto the target's tip and lands it, the target moved and the change
merged in **one record** its committer decides against the target's merge
gate. It answers one of three things:

- **merged** — the commit the target moved to;
- **queued at place n** — a target whose gate requires a check takes the
  change into its merge queue instead, and the queue's landing is the merge:
  the change's people hear `merged` when it lands. `code_dequeue_change`
  takes it out again, at its enqueuer's, its owner's, a maintainer's or a
  person's ask;
- **`not_ready`** — a change its gate does not allow yet: the answer names
  **every** unmet precondition and who each waits on, nothing is merged or
  queued, and its authors who may merge it hear `ready_to_merge` once the
  last is met (where none may, the lead of the repository's owner). A seat is
  told to end its turn rather than ask again.

`method` is `merge` (a merge commit, the default), `squash` or
`fast_forward`. `delete_branch` deletes the change's branch as it merges;
unsaid, a seat's branch (`seats/<handle>/…`) is deleted unless a rule over the
target keeps it (`delete_source: none`), and any other branch is kept.
`close_task` closes the change's work item once it merges — a second write of
the same operation, so the same call made again is the same close, and the
tracker's write, so it is decided as `update_work_item` is: a person who does
not use the tracker has the change merged and the item left as it was. A
close that fails or is refused leaves the change merged and says why
(`merged; ENG-42 not closed: …`), and a queued change's landing closes it. `override_reason` is a
person's reason to merge past the gate, merged directly and recorded on the
change; a seat cannot override.

### From the change's page

A live change's page (**Code › Changes**, then the change) carries the same
call as controls above its tabs, each made as you and never ahead of the
engine's answer:

- **Where its gate stands** — how many of its conditions are met, and the
  first one that is not, judged with *you* as the merger, so a reader without
  the authority to merge into the target is told so before pressing anything.
- **Merge**, or **Add to queue** where the target's gate requires a check, with
  how it merges (**Merge commit**, **Squash** or **Fast-forward**), **Delete
  its branch** — left unticked, the rule above decides — and **Close ENG-42
  once it merges** where the change names a work item. It sends the head the
  page shows as `expected_head`, so a push that landed since you opened the
  page is refused rather than merged unseen. While the gate is unmet the
  button is held with the gate's own sentence.
- **Override**, a person's alone: a dialog that asks your reason, says what
  the gate still waits on, and merges directly past it — even into a branch
  whose merges land through its queue. The reason is kept with the merge
  (the change's Provenance tab shows it) and the override is audited.
- **Re-run checks** — every check of the change's head whose latest attempt
  did not pass, each at its next attempt (`code_rerun_checks` with
  `all_failed`).

A refusal is said beside the controls in words of its own: a head that moved
("somebody pushed to it since you loaded it"), a target that moved under every
rebuild of the merge, a change that does not merge cleanly onto the target's
tip, a full queue; and in the engine's own words where those say more — every
unmet precondition and who clears it (`not_ready`), or why you may not.

## Protecting a branch

A branch's protection is a rule over the branches a pattern matches: what a
push may do to them ([Protection
rules](../concepts/code-host.md#protection-rules)), and — with a **merge
gate** — what a change must meet before it merges into one. Unset, a rule's
`direct_push` lets only a person push to the branch, so a seat's work reaches
it as a change. A rule is declared with the repository in the company
document, or set on a repository a tool made with `code_protect_branch`, by a
seat or your own assistant ([Who may change a
protection](../concepts/code-host.md#who-may-change-a-protection)):

```yaml
code:
  repos:
    - name: web
      protect:
        - branch: main
          merge_by: maintainers    # writers (unset), maintainers or persons
          approvals: one
          human_paths:
            - glob: "infra/**"     # a person approves anything here
          checks: [unit, lint]     # each must pass on the change's head
```

Walk one change through it. A seat pushes `seats/dev/retry` and opens
`web!42` into `main` ([Open a change from a push](#open-a-change-from-a-push)).
The head's checks run, from `.crewlet/checks.yaml` as `main` holds it ([Adding
checks](#adding-checks)); a reviewer approves; a person approves the change's
`infra/` paths. Its readiness — on its page, and in `code_get_change` — lists
each of those conditions and whom each waits on, and its authors who may
merge into `main` hear `ready_to_merge` when the last is met. Because the gate requires checks, a
merge **adds it to `main`'s queue**, which tests it merged on the branch
before it lands ([The merge queue](../concepts/code-host.md#the-merge-queue)),
and `main`'s tip is checked again once it lands, `code_green` advancing on
green ([From a push to a green branch](../concepts/code-host.md#from-a-push-to-a-green-branch)).

**Settings › Code** lists every protected repository, each rule with what it
lets a push do and what its gate asks, and says where the repository's rules
are set — the company document, or the code tools. A repository's own page
shows the same under its **Settings** tab. A seat may tighten a gate and never
loosen it ([Who may change a
protection](../concepts/code-host.md#who-may-change-a-protection)); every
field of a gate is in [Merge gates](../concepts/code-host.md#merge-gates).

## Rebasing a change

A change whose target has moved on is rebased onto it with
`code_rebase_change`, naming the change and the head you read
(`expected_head`) — by a seat, or by your own assistant, as you. The code host
replays the change's commits its target does not hold onto the target's tip,
oldest first, and moves the change's branch to the last of them with **one
forced push** of yours: the branch's protections and your right to force-push
it hold the rebase as they hold a `git push --force`, and the change records a
new revision at its new head.

- **Each commit stays its author's.** A replayed commit keeps its author's
  name, address and date — the zone it was written in included — and its
  message byte for byte; only its committer is the engine's, `Crewlet
  <engine@<code.identity_domain>>`, dated when you asked.
- **The first conflict stops it.** A commit that does not apply onto what is
  rebased so far ends the rebase there, nothing changed: the answer names the
  commit (`2 of 5`) and each conflicted path with its stages' ids and the start
  of the file as git marked it. Resolve it in a coding box, or rebase it
  yourself with `git rebase` and push.
- **A commit already in the target is dropped**, as `git rebase` drops it.
- **What it refuses:** a change of more than 1 000 commits its target does not
  hold (`too_many_commits`), one whose history holds a merge commit
  (`merge_commit` — a merge needs a mainline to replay), and a commit git would
  not write back as it was — an author in a form git trims, or a message in
  another encoding than UTF-8 (`rejected_object`). Each is a rebase for a
  coding box or your own clone.

The rebase is built on the repository's [home git
node](../concepts/code-host.md#the-home-git-node) under forty seconds of git
([How a rebase is built](../concepts/code-host.md#a-change-rebased)).

## Reverting a change

A merged change that turns out wrong is undone by **a change of its own**,
never by moving its target back: `code_revert_change`, naming the change — by
a seat, or by your own assistant, as you. The code host undoes what the
change's merge brought into its target, onto the target's current tip, as one
commit of yours on a **new branch** — `seats/<handle>/revert-<n>` for a seat,
`revert-<n>` for a person — and the push of that branch opens the change
**"Revert web!42"** into the same target, in the same record. That change is
gated, reviewed and merged like any other; pass `merge` and it is merged as
the call's second write, as far as its target's gate allows — a merge it
could not make yet is said beside the opened change, and the revert stays
open for you to merge.

- **Every way it was merged reverts the same way.** A merge commit is
  reverted against its first parent (git's `revert -m 1`), a squash against
  its one parent, and a fast-forward over the change's whole range, from the
  base its head revision was measured against — so the revert is the whole of
  the change, as one commit, whatever its method.
- **A conflict stops it.** Where the target has since changed what the change
  changed, nothing is created: the answer names each conflicted path with its
  stages' ids and the start of the file as git marked it. Resolve it in a
  coding box, or revert it yourself with `git revert` and push.
- **What it refuses:** a change that is not merged (`not_merged`), and a
  revert branch already there (`exists` — a revert of the change is under
  way: find its change, or delete the branch to revert again).

The revert is built on the repository's [home git
node](../concepts/code-host.md#the-home-git-node) as any engine write is
([How a revert is built](../concepts/code-host.md#a-change-reverted)).

## Adding checks

A repository's checks — the builds, tests and linters a change is measured
by — live in the repository itself, in `.crewlet/checks.yaml` at the root of
its tree:

```yaml
checks:
  - name: unit
    run: make test
    timeout_seconds: 900
    report:
      junit: out/junit.xml
  - name: lint
    run: make lint
    timeout_seconds: 300
    paths: ["**/*.go", .golangci.yml]
  - name: web
    run: npm ci && npm test
    timeout_seconds: 1200
    checkout: [web]
    env: [NPM_TOKEN]
```

Three keys are required — a `name`, the command to `run`, and its
`timeout_seconds` — and the rest narrow it: `paths` runs a check only for a
change touching one of its globs, `report` names the JUnit or SARIF file it
writes, `run_in` picks a container on a git node or a remote `e2b` box,
`checkout` checks out a cone of directories (or `none`), and `env` names the
company's check secrets it is given. Every key, its bound and why is in [The
checks file](../concepts/code-host.md#the-checks-file).

1. **Write the file, and check it before you push it.** It is read strictly,
   as a config document is — an unknown key, a key written twice, a number
   where text belongs (`name: 1.10` is a number: quote it) are refused — and
   `crewlet validate` reads it exactly as the code host does, with no node,
   no network and no credential:

   ```bash
   crewlet validate .crewlet/checks.yaml
   # .crewlet/checks.yaml: 3 checks (unit, lint, web)
   ```

   A broken file names every fault at once, each at its field and line — the
   first 128, and how many more there are past them; `-json` gives them as a
   fix loop reads them:

   ```bash
   crewlet validate .crewlet/checks.yaml -json
   ```

   ```json
   { "valid": false, "tier": "checks", "file": ".crewlet/checks.yaml",
     "problems": [ { "path": "checks[0].timeout_seconds", "kind": "out_of_range", "line": 4,
       "message": "checks[0].timeout_seconds: value out of range: 99999 seconds: a check runs for 1 to 21600 seconds (code.CheckTimeoutCeiling, six hours) (line 4)" } ],
     "warnings": [] }
   ```

2. **Never `run_in: direct`.** A check runs a change's own code; `direct` would
   run it as the engine's own user on the engine host, so the file refuses it,
   as it refuses `self`.

3. **Land it on the branch it governs.** A change's checks are the ones its
   *target* branch defines, never its own: a change that edits `checks.yaml` is
   checked by the file it edits, and its edit governs the changes after it
   lands.

Once the file is on a branch, the repository's home git node queues its
checks at the head of every live change into that branch ([How a head's
checks are dispatched](../concepts/code-host.md#how-a-heads-checks-are-dispatched))
and, on a protected branch, the required ones at its newest landing ([Tip
checks and code_green](../concepts/code-host.md#tip-checks-and-code_green)),
and every git node runs a queued check in a box of the company's sandbox
catalogue ([Who runs a check](../concepts/code-host.md#who-runs-a-check)). A
change's readiness counts the checks its branch's gate requires, and a merge
into that branch puts the change in its merge queue, which the home git node
lands as below.

**A branch whose gate requires a check lands through its merge queue.** The
home git node tests the waiting
changes merged on the branch before any of them lands: up to `depth`
candidates at once, each one change while the queue keeps up and up to
`batch` changes as it falls behind, every candidate's tip checked under the
`checks.yaml` the branch itself holds. The longest run of candidates whose
newest tip is green lands in one go, each change merged as whoever queued it,
and a work item they asked closed is closed as them. A red candidate is
bisected: its changes' own commits are checked until the change that broke it
is found, and only that change leaves the queue (its `code_queue_ejected`
event names the checks); the rest are tested again without it. A change that
conflicts with the ones ahead of it leaves the queue naming the paths, and a
push to the branch while candidates are testing starts them again on the new
tip. [What the home does with a
queue](../concepts/code-host.md#what-the-home-does-with-a-queue) has the
detail.

### Choosing a queue's depth and batch

A queue lands at most `depth` × `batch` changes a round, and a round lasts as
long as the slowest required check: at ten-minute checks and one change a
candidate, the default depth of four lands 576 changes a day and the most,
eight, 1 152. Size it for how many changes the branch is asked to land:

```yaml
code:
  checks:
    max_timeout_seconds: 1800      # what the slowest check really takes
  repos:
    - name: web
      owner: {unit: engineering}
      protect:
        - branch: main
          checks: [unit, lint]
          landing: {depth: 6, batch: 2}   # up to 6 candidates of up to 2 changes a round
```

`crewlet validate` warns at a rule's `landing` when the queue cannot land one
change a day for every seat that may merge into the branch, priced at a round
as long as `code.checks.max_timeout_seconds` — the longest a check may run —
so set that to what your checks actually take. A running queue asked past
what it lands raises `code_merge_rate_over_capacity` on its home git node,
priced at the rounds its trains really took over the last hour. Either way,
raise `depth` or `batch`, make the required checks faster or fewer, or split
the work over more branches or repositories ([How fast a queue
lands](../concepts/code-host.md#how-fast-a-queue-lands)).

## Reading a failed check

A check that fails says why in four places, each on its attempt — read whole
with `code.read`'s `check` ([Reading a repository's
checks](../concepts/code-host.md#reading-a-repositorys-checks)):

1. **Its summary**, the line you read first. Write it yourself: a check's
   command is told a file in `$CREWLET_CHECK_SUMMARY`, and whatever it writes
   there — up to 4 KiB, a paragraph — is the summary, over anything the engine
   could say. Otherwise the summary is what the check's reports count (`12 of
   340 tests failed`, `5 findings at error or warning`), and failing that its
   exit status and how long it ran (`exited 2 after 3m12s`). A report that was
   named and not read is said after it.

   ```bash
   # in the check's command
   go test ./... 2>&1 | tee out/test.log
   echo "$(grep -c '^--- FAIL' out/test.log) tests failed" > "$CREWLET_CHECK_SUMMARY"
   ```

2. **Its failures**: what the reports its file names (`report: {junit: …,
   sarif: …}`, paths in its checkout) say failed, read once its command ends —
   each a test id, a file and a line, and a message of up to 2 KiB: a JUnit
   case's `<failure>` or `<error>` with its classname and name, and a SARIF
   result at level `error` or `warning` with its rule. At most **200** of them,
   in the reports' own order, within 64 KiB together; the rest are counted, not
   listed. A report is read up to 32 MiB.

3. **Its tail**: the last 16 KiB of its output, which is where a build that
   failed before any test ran says why.

4. **Its log**: the last 64 MiB of its output, kept as one object in the
   fleet's object store and read a page at a time — never in a row. A passed
   check keeps its summary alone, so nothing of a green run is stored. Where
   the object store has no room, the check ends without its log and its
   summary says so; its failures and its tail are kept either way. Only those
   64 MiB are read from the box, however much more the check wrote, and its
   reports and its log are read within a minute of its end: a box slower than
   that ends its check without what was still unread, its summary saying so,
   rather than leaving it running.

A check that ran past its `timeout_seconds` is **failed** `timed_out` — the
time is the check's own, so it measured the commit — with its tail and its
log. One that ended **errored** measured nothing: its box or its setup failed
(`box_failed`, `setup_failed`), or its runner was lost (`runner_lost`), and
the engine queues it again, up to its third attempt; a file naming a cell or a
secret the company does not allow is `defs_invalid`, and is not queued again.

A seat reads all four with `code_get_check_failures` — naming the commit by
`change` (its head) or `sha`, and the check by `name` — its failures a page at
a time and its log 32 KiB a page, every page from the attempt the first one
read even once a rerun ends or the change's head moves, and every word fenced
as text nobody in the company wrote ([Text somebody else
wrote](../concepts/agent-runtime.md#text-somebody-else-wrote)); and
`code_get_change`'s `checks` part lists a change's head's checks in a line
each. **Running a check again** is `code_rerun_checks`, naming each check and
the attempt you read as its latest (or `all_failed`), so two people who read
the same red check queue one attempt between them, never two. A person reruns
any ended attempt; a seat only one its infrastructure or a stop ended —
`errored` or `cancelled` — and only on a repository it may write or a change
it wrote, because a failure is a measurement of the code, and a seat that
could rerun one would re-roll a flaky test until it passed. A check runs at
most ten attempts.

## The Queue and Checks

The dashboard's Code workspace draws both in sections of their own, each over
the same questions a seat's tools ask.

**Code › Queue** (`#/code/queue`) is the merge queues of your gated branches:
a band per repository and branch, its entries in the order the queue lands
them — the place, the change, who enqueued it and where it stands (`waiting`
for a candidate, or `testing` in one). Choose a repository (`?repo=web`) and
each entry also shows the **train base** it is tested on, a meter of the
**required checks** of what it is tested in, each at its latest attempt —
passed, failed and running, the rest not yet a verdict — and how long it has
been queued against the slowest of those checks' declared `timeout_seconds`,
since the queue waits for its slowest required check. Under each of its
bands, in the danger tone, is what the queue **put out in the last day**: the
change, why (`a required check failed`, `its merge conflicted`, `it no longer
met the branch's gate`, `its head moved`, `the branch moved under the queue`)
and the required checks that
failed on what it was tested in or the paths its merge conflicted on. A branch
with nothing queued and nothing put out lately draws no band, and a company
with nothing queued says so. The queues are read fifty entries at a time;
**Load more** reads the next fifty.

**Code › Checks** (`#/code/checks`) is every repository's attempts that
ended in a window — a day by default, at most the thirty days an ended
attempt is kept — newest ended first, with a chart of when they ended (the
failed ones the foot of each bar) and chips that narrow the listing by state
and by a check's name; the engine narrows them, and the chart and the chips
count the attempts loaded, as each says. A repository's page has its own
**Checks** tab, every attempt it keeps with no window, and a change's page
has one too: the attempts that judge it, its head's, checked for its target.

Press an attempt to open it: what it was checking (a change's head, a queue's
candidate, a branch's tip, a landing bisected), the commit and branch, how
long it ran, its exit code and why it ended; its **failures**, each
`path:line` a link into the diff it was checked on — the comparison of its
base and its commit, opened on that file; its **tail**; and **Read its log**,
which reads the log a failed run kept a page at a time, only when you ask. A
check measures durations and exit codes, never tokens: it runs no model.

## External CI

A check the engine does not run — your own CI's, on a runner of yours — is
reported onto the same rows, so a change's readiness, its checks part and the
dashboard read it beside the engine's own. A report is a **person's**: your CI
calls `code_report_check` over [your own
assistant's](../reference/api-endpoints.md#operatormcp--your-own-assistant)
surface with a token of your own (`crewlet iam token -login`), and the report
names you. A token bound to no seat only reads, and no seat may report: a
check result is a measurement, and a seat runs no CI whose result it could
send.

```json
{"name": "code_report_check", "arguments": {
  "repo": "web", "sha": "<the commit's whole id>", "name": "e2e",
  "status": "running", "url": "https://ci.example.com/runs/812"}}
```

Report `running` when the run starts, then how it ended — `passed`,
`failed`, `errored` or `cancelled` — with its `summary`, `tail` and
`failures` (each a `path`, `line`, `test_id` and `message`, at the bounds an
engine check keeps) and the run's `url`. Name the commit by `sha` and the
branch it is checked for by `checked_for` (the repository's default branch
when left out), or by `change`, which reports for the change's head and
target. An ending sent again is answered from the row, so a CI that retries a
lost answer records nothing twice; a run after an ended one is the check's
next attempt. One check of a commit is one runner's or one CI's, never both:
a name the engine has queued at that commit for that branch is refused
`native_check`, so the engine's result is never overwritten, and a name your
CI reported first is left to it — the engine's dispatch queues nothing of a
name the commit already holds an attempt of. So name your CI's checks apart
from the ones `.crewlet/checks.yaml` defines.

## Finding code

A code search reads lines, not files: an expression, matched one line at a
time with Go's regular expressions, over a commit of each repository
searched — its default branch's tip unless the search names another. It is
written the way the knowledge base's search is, filters first:

```text
repo:billing repo:ledger path:src/**/*.go ref:main  retryBudget
```

- `repo:<name>` searches that repository (repeat it for more); with none, a
  search covers every repository you may read that is searched by index.
- `path:<pattern>` keeps the files a `.gitignore`-style pattern matches.
- `ref:<ref>` searches a branch, a whole ref or a commit's id.
- The text after them is searched as it is written; a search may be asked
  as a regular expression or as a whole word instead, and to ignore case. It
  needs one run of at least three characters to look up, matches within a
  line, and takes at most 400 bytes — the knowledge search's bound.

**Indexed or grep, per repository.** A repository declared in the company
file is searched by index unless its entry says `search: grep`; one a seat's
tool makes, and every scratch repository, by grep. An indexed repository's
default branch is kept as a trigram index on two of your git nodes, so a
search of the whole company reads only the files that can match. A grep
repository is read with git grep whenever it is searched, which costs a
large repository a second or two of a git node's CPU, so a search reaches it
only when a `repo:` filter names it — at most eight of them at once.

**A search says what it did not search.** Several repositories are searched
in one question to every git node at once, each repository answered by the
node first in its order and, where that one does not answer, once more by the
next. Every answer carries a coverage block naming every repository it could
not search and why — a node not answering, an index still being built, a node
behind, a grant its chart refused, a ref the repository does not hold — and
the grep repositories a search with no `repo:` left out. An empty answer with
an empty coverage means nothing matched; an empty answer with a coverage that
names repositories means those were not looked at: search them again by name,
or ask after the node named. At most 500 lines come back, in the order of
repository, path and line, and `truncated` says more matched. See
[Code search](../concepts/code-host.md#code-search).

**Searching from the dashboard.** **Code › Search** (`#/code/search`) takes
the search as written above, with its mode, a repository, a path and a ref
as controls beside it, and draws the lines by repository and file, each line
opening its file at the commit it was searched at; what was not searched is
said above them on every answer. In ⌘K, the **Code** tab searches the same
way and lists the first eight lines.

**Searching from a script.** The same search is the question `code_search`,
on [`GET /code/search`](../reference/api-endpoints.md#get-codesearch):
`?q=` is the search as written above, with `mode`, `ignore_case` and one
`repo`, `path` and `ref` beside it where a script would rather not write them
into the text. Its answer names the commit each repository was searched at,
so read a hit's file at that commit, not at the branch, which may have moved
since.

**A seat searches with a tool.** The same search is `code_search`, a seat's
and your own assistant's ([The company's
code](tools-and-mcp.md#the-companys-code)): `query` in the grammar above,
with the same `mode`, `ignore_case`, `repo`, `path` and `ref` beside it. Each
line arrives fenced as a file's text that nobody vouches for, and the 500
lines come a page at a time, each page under one tool answer.

## Seats and code

Nothing to set up: once the company hosts code, your seats work in its
repositories through thirty-six of the [code
tools](tools-and-mcp.md#the-companys-code), on the server and in their own
names — every one but `code_report_check`, `code_purge_unreachable` and
`code_import_repo`, each a person's. A seat holds
no git credential and needs none. Every seat reads with thirteen of the tools;
a seat whose `role.code.access` is `full_access` also changes the
repositories with the other twenty-three; and a seat whose role has a
sandbox works in a clone besides. What each may do is its grant, derived from
the chart ([Who may read and push](../concepts/code-host.md#who-may-read-and-push)).

### What your seats read

Every seat reads the repositories with thirteen of the code tools — a file's
lines, a directory, what changed between two commits, how two commits relate,
whether a fix reached a branch and which push put it there, a path's history
and who last changed each line of a file, a repository's changes: what each
one changes, its findings placed in the revision it reads and what changed
since an earlier revision, and what a check of a commit said — with no clone
and no token. And it finds where something is with `code_search`, the
[search](#finding-code) a person makes, across the repositories it reads that
keep a search index, or those its `repo:` filters name:
its prompt tells it to search before reading whole trees.

What each seat may read is its grant, from the chart: its
[`role.code.access`](../getting-started/configuration.md#role-fields-summary) caps it, and a
`visibility: unit` repository is read only by the owning unit's seats and the
leads above them ([Who may read and push](../concepts/code-host.md#who-may-read-and-push)),
so a seat outside the unit finds the repository absent from its listing and
every read of it refused. Your own assistant reads through the same tools on
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant),
as you, which reads every repository; asked by you, `code_get_repo` also hands
out the URL to clone from, on `api.external_url`.

### What your seats change

A seat at `full_access` changes the repositories with the other twenty-three code
tools, one change a call: a commit of up to 1 000 file operations to a branch
(`code_commit_files`), a commit reverted or picked onto one (`code_revert`,
`code_cherry_pick`), a branch created or deleted (`code_create_branch`,
`code_delete_branch`), a repository created, changed, renamed, archived,
deleted, restored or protected ([below](#scratch-repositories)), and a change
opened, edited, closed, reviewed and discussed ([Or with a
tool](#open-a-change-from-a-push)). A `read_only` or `review` seat — and
`read_only` is what a seat that sets none runs at — is offered the thirteen
reads alone, none of the twenty-three. Your assistant has the same twenty-three
and `code_report_check`, `code_purge_unreachable` and `code_import_repo`
besides, each a person's alone, as you.

**A commit is built on the server and lands as one push** in the seat's name
([Engine-built commits](../concepts/code-host.md#engine-built-commits)). The
git node holding the repository builds it over exactly the directories it
touches — never a checkout, so a commit to a repository of a hundred thousand
files costs what it changes — checks its objects as it checks a `git push`,
and lands it through the committer, the protections and the limits a
person's push meets. A call names the head it read (`expected_head`), so a
branch that moved meanwhile is refused `stale` rather than overwritten: the
seat reads it again and decides. And a call asked again — retried after a
timeout, or made on another git node when the first went silent — is the same
push carrying the same commit, landed once.

**Who a commit names.** Its author is the seat: its role's `name`, and the
`email` the role states or, where it states none a commit can name (a
`${VAR}` reference is never written into history), `<handle>@` the code
host's identity domain. Set [`code.identity_domain`](../getting-started/configuration.md#code)
to a domain of yours, so those addresses read as your company's in every
clone; unset, they end `@seats.invalid`, a name nobody can own:

```yaml
code:
  native: true
  identity_domain: example.com   # docs-writer, stating no email, commits as docs-writer@example.com
```

Its committer is the engine, `Crewlet <engine@example.com>`. That marks the
commits the code host built in your history and proves nothing — any git
client can write the same committer — so who made a commit is its push's
record, never the commit ([Reading a seat's pushes](#reading-a-seats-pushes)).
Nothing is added to the message.

**Where a seat may commit** is its grant: a seat in the owning unit writes the
repository's branches, its unit's lead also forces and deletes them, and every
`full_access` seat owns `seats/<its handle>/` on every repository it reads,
where a seat outside the unit puts its work in place of a fork. Beyond its
grant, a seat's push is held to what a person's never is ([A seat's
push](../concepts/code-host.md#a-seats-push)):

- **A protected branch takes no seat's push**, its unit's lead's included
  (`protected_direct`): `direct_push: persons`, what a rule says unless it
  says `none`, is a person's push alone. A seat's work reaches `main` when a
  person pushes it: the seat commits to a branch of its own and opens a change
  of it for you to read.
- **A seat outside the owning unit fills at most half** of the repository's
  refs and half its bytes, so the owners always keep room to push
  (`repo_refs`, `repo_size`).
- **A seat lands at most `code.limits.pushes_per_seat_day` pushes a day**
  ([The daily quota](#the-daily-quota), `push_quota`).
- **A data node behind the company's configuration decides no seat's push**:
  the revision it failed to apply may be the one that took the seat's grant
  away, so the repository's next committer decides it.

What each refusal means is in [What a write is
refused](../concepts/code-host.md#what-a-write-is-refused), and the seat's tool
answers with the same code and sentence.

### Scratch repositories

A seat that wants somewhere to try something makes a **scratch repository**
of its own with `code_create_repo`: named after the seat, `<handle>.<name>` —
`docs-writer.parser-spike` — and owned by it. Only that seat creates one, at
`full_access`. It takes no protection rule, it is searched with `grep` and
never indexed, and the seat changes, renames, archives, deletes and restores
it as it does any repository it maintains. Two settings bound them:

```yaml
code:
  scratch:
    max_per_seat: 50   # 0..1000; 0 lets no seat make one. Unset is 50
    idle_days: 7       # 1..365; untouched this long, one is deleted. Unset is 7
```

- **`max_per_seat`** counts **every** scratch repository a seat owns until it
  is purged, a deleted one included, so a seat creating and deleting in a loop
  holds no more; past it a create is refused `scratch_limit`.
- **`idle_days`**: one with no push, rename, settings change or restore for
  that long is **deleted** by the `code-retention` duty, and restorable for
  `code.deleted_retention_days` before it is purged. Reading it is no
  activity; an archived one is kept; and one a live turn has named is never
  taken from under it, however long the turn waits ([A scratch repository's
  idle expiry](../concepts/code-host.md#a-scratch-repositorys-idle-expiry)).
  The duty's `code_retention_pass` line counts the ones it `expired` and the
  idle ones a turn kept (`scratch_held`).

You never create a scratch repository — it is a seat's — and you maintain
every one: archive one to keep it, or restore a deleted one before its purge
with `code_restore_repo`, from your assistant.

A unit's effective lead at `full_access` may also create a **project**
repository for its unit with the same tool — one label, like a declared one,
and counted with the declared ones against the 10 000 the code host holds
(`repo_limit`). On a repository it maintains a seat may add or tighten a
protection with `code_protect_branch`, never remove or loosen one: that is
yours ([Who may change a
protection](../concepts/code-host.md#who-may-change-a-protection)). A
repository `code.repos` declares refuses every tool's request
(`config_managed`): change its declaration instead. Everything a tool may do
to a repository is in [Repositories made by
tools](../concepts/code-host.md#repositories-made-by-tools).

### Renaming a repository

A repository a tool made is renamed with `code_update_repo`'s `rename_to`, by
the seat that maintains it or by you through your assistant. The new name
keeps its kind's form: a scratch repository keeps its seat's handle
(`docs-writer.parser`), and a project stays one label. **The old name
redirects for 30 days**: a clone or a fetch of the old URL still works — git
prints `warning: redirecting to` the new one where it sends its credential up
front — while a push to the old name is refused, naming the new one and when
the old one stops serving. Point every clone at the new name:

```sh
git remote set-url origin https://crewlet.example.com/git/docs-writer.parser.git
```

Nothing else takes the old name while it redirects, renaming back takes it
back, and once the 30 days are over it is free. A **declared** repository is
not renamed this way: its name in `code.repos` is its address, so a new name
there deletes it and creates another ([Declare
repositories](#declare-repositories)).

### The daily quota

A seat lands at most `code.limits.pushes_per_seat_day` pushes a day on the
company's [calendar](../getting-started/configuration.md#the-companys-clock),
every repository together and every push it lands — built by its tools or sent
from its coding box: 1 000 unless set, about thirty-three times the thirty
pushes a seat of the reference company makes a day. It is a bound on a seat
pushing in a loop, not a budget, and your pushes are never counted:

```yaml
code:
  limits:
    pushes_per_seat_day: 1000   # 1..100000; one seat's pushes a company day
```

Past it, a push is refused whole as `push_quota`, its sentence naming when the
count starts again — the company's next midnight — and **nothing records it**:
a record of every refusal of a runaway seat would cost the code log what the
quota exists to bound, so a refused push is not in the push history. A seat
that meets it is pushing in a loop: read what its turns are doing before you
raise the bound. Its git work is already done by then — the pack it built is
stored, named by nothing, and deleted by the object collector a day later.

### Reading a seat's pushes

Every push a seat lands names the seat and **the turn that made it**: its
turn, the work item that turn was charged to and the model it ran on, stamped
by the engine from the call — never read from the commit, which any client
could have written. `crewlet code status -repo` prints the turn and the work
item in `BY`, and for a push from a coding box its run and the coding agent
the box ran ([`crewlet code status`](../reference/cli.md#crewlet-code-status)):

```text
PUSHED AT             BY                                                                NODE    REFS  RESULT
2026-10-12 09:31 UTC  docs-writer (turn 0192f6a1-…, native ENG-12)                      data-a  1     ok refs/heads/seats/docs-writer/eng-12 4c1d2e3..8a7b6c5
2026-10-12 09:02 UTC  swe (claude-code run 7f3e9c2a-…, turn 0192f69c-…, native ENG-42)  data-b  1     ok refs/heads/seats/swe/eng-42 0000000..1f2e3d4
```

and `GET /code/repos/{name}/pushes` answers each with `actor_kind` `agent`,
the seat's handle as `actor`, how its pack was built (`via`: `engine` for a
commit its tools built, `git` for its coding box's push) and the run behind it
under `run` — `turn_id`, `work_backend` and `work_key`, `model`, and for a
box's push `run_id` and `coding_agent` ([API
Endpoints](../reference/api-endpoints.md#the-code-host-git-and-code)). Going
the other way, from a commit to whoever landed it, `code_landed_in`,
`code_log` and `code_blame` name the push behind each commit ([Which push
landed a commit](../concepts/code-host.md#which-push-landed-a-commit)).

### Working in a clone

A seat whose role has a [sandbox](../concepts/code-sandbox.md) also works in a
clone, and there it is a git user of the code host as you are, with a
credential of its run's own rather than yours or any token's. It names the
repositories in `run_sandbox`'s `repos`, and the engine clones each into the
coding box before the coding agent starts
([Repositories on this company's code host](../concepts/code-sandbox.md#repositories-on-this-companys-code-host)):

- **on the seat's own branch** for the work item its turn is charged to,
  `seats/<handle>/<the item's key>`, unless the call names another — so every
  run on one item continues what the last one pushed. A work item whose key
  changes starts a new branch;
- **from that branch's head**, or the default branch's where the branch does
  not exist yet, unless the call names a `base`;
- **to push, where the seat may**, else to read: a seat that may not push a
  repository is still given it to look at. The run's credential pushes only
  what the run was given and its seat may push, and ends with the run.

The box reaches the git nodes at `/box/git/`, through the address every node
that runs seats is given in `CREWLET_BOX_GIT_URL`
([Deployment](deployment.md#exposing-webhooks-without-the-admin-api)), and
every request it makes is admitted by its run's record, never by its token
alone ([Coding boxes](../concepts/code-host.md#coding-boxes)). The commits it
makes carry `Crewlet-Seat`, `Crewlet-Run` and `Crewlet-Work-Item` trailers by
convention, which a box can drop or forge and nothing on the code host reads:
the push's record is what says who pushed a commit ([Commit
trailers](../concepts/code-sandbox.md#commit-trailers)).
Its pushes are held to the seat's rules above, and when the run ends, the seat
is told what it pushed from the code host's own record of its pushes — each
branch it moved, from where to where, and the change it heads — not from what
the run says of itself
([What a run pushed is reported for it](../concepts/code-sandbox.md#what-a-run-pushed-is-reported-for-it)).
A run can work on a change — `change: web!42` puts it on that change's branch,
from its head — or ask for one: `open_change` opens a change from the run's
branch once the run has succeeded, once, carrying the seat's work item
([A run's changes](../concepts/code-sandbox.md#a-runs-changes)).

### A seat's idle branches

A seat leaves a branch under `seats/<handle>/` for each piece of work, and
nobody else deletes them, so the code host does: a branch **a seat set in its
own namespace and nobody has set for 14 days** is deleted by the
`code-retention` duty, as a push on the engine's own account ([The ref
lifecycle](../concepts/code-host.md#the-ref-lifecycle)). A branch a person
pushed last, the repository's default branch, a live change's source or target
and a protected branch are never deleted; nor is a branch a seat's work still
depends on — one a live turn read or wrote with a code tool, one a coding
run's launch gave its box, or one its box pushed — for as long as that turn,
or the run it is waiting on, has not ended, however long the run takes. A
turn that reads a branch nobody has set for nearly 14 days records its hold
on the repository, so the read and a deletion already under way are ordered:
when the deletion lands first, the turn's call is refused, the answer naming
the commit the branch was at, and nothing about the branch is sent.

**A deleted branch is restored** with `code_create_branch` at the commit it
was at, by the seat or by you through your assistant. The deletion's row in
the push history — `GET /code/repos/{name}/pushes` and `crewlet code status
-repo` — names every branch it deleted beside that commit (`via` `expiry`,
`actor_kind` `system`) for the 181 days the history keeps it. The row says
where the branch was; restoring it needs the commit itself, which the
repository is held to keep only for the deletion's **14-day** prune grace
([ref drops](../concepts/code-host.md#retention-and-history)), and not even
that once a person purges the repository's unreachable objects. Restore a
branch you need within two weeks of its deletion: after that, it comes back
only until the repository's next [expunge](../concepts/code-host.md#expunge)
drops the commit. A repository
setting refs faster than they go idle raises `code_refs_near_limit` before it
reaches the 100 000 live refs it may hold ([alarms](../reference/alarms.md)).

## Who read a unit's repository

Every clone and fetch that sends a `visibility: unit` repository's contents,
and every server-side read of one, is recorded — whoever made it: a person, a
token or a seat. An admin asks who read one, from a script with a token that
carries `admin`, or from their own assistant with `code_repo_readers`, a page
at a time:

```sh
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/code/repos/payments/readers"
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/code/repos/payments/readers?from=2026-07-01&to=2026-09-30"
```

Each reader — a credential and the person it acted as, or a seat — comes with
how it read (`git` or `tool`), how often, how many bytes it was sent and when
it last read, summed over every node and every day of the range: the thirty
company days ending today unless you name `from` and `to`, and at most 181.
Hand a page's `next` back as `?cursor=` for the page after it. A
`visibility: company` repository's readers are every seat and nobody records
them; its answer says so. What counts as a read, how long it is kept and why
only an admin may ask are in [Who read
it](../concepts/code-host.md#who-read-it).

## Moving a repository in

A repository that lives at another forge comes in one of three ways, each a
person's to start: **imported once** by the repository's home git node, which
then holds it like any other; imported and then kept in step with its source
for good as a **pull mirror**, which moves only when its source does; or
**pushed from your machine** into a repository that exists, for a source only
your machine can reach. The first two dial the source from the node, over
HTTPS alone and only at a public address unless Tier A says otherwise
([Reaching other forges](deployment.md#reaching-other-forges)); what each moves, and how often, is
[Mirrors and imports](../concepts/code-host.md#mirrors-and-imports).

### Import from GitHub or GitLab

There are two imports. **The node's** creates the repository and has its home
git node fetch the source: nothing runs on your machine, and with `-mirror` the
repository stays in step with the source for good. **Your machine's**
(`-local`) has your own git clone the source and push it into a repository that
exists, for a source only your machine can reach. Either is a person's: run it
with your own token exported ([Give each person a token](#give-each-person-a-token)).

#### The node imports it

```sh
crewlet secrets set GITHUB_TOKEN          # the source's token, where it is private
crewlet code import https://github.com/acme/web.git web -owner unit:eng -credential GITHUB_TOKEN
crewlet code import https://github.com/acme/api.git api -owner unit:eng -mirror   # a public source, mirrored
```

The repository is created at once, owned as `-owner` says, and reads
`importing` until its home git node has fetched every branch and tag of the
source and landed them, in chunks of packs and refs that no push limit bounds,
so `code.limits.max_push_bytes` caps none of it; it takes no push meanwhile, and
`crewlet code status -repo web` shows it `importing` until it is done. The home dials HTTPS alone, and only a public address
unless Tier A's `git.import_allow_private` lets it reach your own network.
`-credential` names the variable holding the source's token — never the token,
which the repository's settings would carry on every data node and in every
backup; the node resolves it from the secret store, then its environment, each
time it dials. A blob past `code.limits.max_blob_bytes` or a history past
`code.limits.repo_max_bytes` ends the import early: the repository turns
`active` holding what landed, and the import is not tried again. A **pull mirror**
(`-mirror`) is imported the same way and then moves only by its home's syncs
from the source, so every push to it is refused. Your own assistant does the
same with `code_import_repo`, and so does **Import a repository** on the
dashboard's Code › Repositories, where the repository's Overview then says
how far the import went; no seat is offered it. Every flag is in
[`crewlet code import`](../reference/cli.md#crewlet-code-import).

#### Your machine pushes it

Create or declare the repository first — `-local` fills one that exists, and
creates none — then:

```sh
crewlet code import https://github.com/acme/web.git web -local
crewlet code import git@gitlab.com:acme/web.git web -local     # SSH works as well
```

**Your git does the work, with your access.** It clones the source with your
own credentials — your credential helper, your SSH key, your proxy — and pushes
it to the code host as you, so the engine reaches no other host and the
repository's push history names you for every step. Where the source asks for a
password none of your helpers holds, or ssh for a host key or a passphrase,
your git asks you on the terminal, as it would for a clone of your own. Your token must act as
your seat ([Give each person a token](#give-each-person-a-token)): a token bound
to nobody fetches only, and the import says so before it downloads anything.
The source is cloned into the system's temporary directory, so point `TMPDIR`
at a volume with room for it, and for two steps' packs beside it — up to three
times `code.limits.max_push_bytes`.

**What it takes, and what it skips.** Every branch and every tag. A forge's own
refs — GitHub's `refs/pull/*`, GitLab's `refs/merge-requests/*` — are the
forge's rather than the repository's: they are listed as skipped, and the
history only they reach is never downloaded. Git LFS files stay on the LFS
server they live on; the import carries the pointer files the history holds.

**What it refuses** — before anything is pushed where the source shows it,
every finding at once so you fix the source once, and otherwise before the
step that would meet it:

| The import says | What to do |
|---|---|
| `blob <id> (<path>) is 812.0 MiB, past code.limits.max_blob_bytes (50.0 MiB)` | Large binaries belong in tracker files. Rewrite the history without them (`git filter-repo --strip-blobs-bigger-than 50M` on a copy) and import that, or raise `code.limits.max_blob_bytes`. |
| `the source's history packs to … as one pack, past code.limits.repo_max_bytes` | Raise `code.limits.repo_max_bytes` — with room for the steps, below — or import a smaller history. |
| `web would hold … of packs once the next step lands … past code.limits.repo_max_bytes` | The steps store more than the source's own pack (below). Raise `code.limits.repo_max_bytes` and run the same command again: it resumes. |
| `commit … alone packs to …: it brings in the history of …, and no other branch of the source can carry it` | A merge of a history larger than one step that no other branch reaches. Mirror the source (`git clone --mirror`), give the commit it names a branch there (`git branch`), and import from the mirror: that history is stepped on the branch first. Delete the branch afterwards if you do not want it. |
| `the source is a shallow clone` | Import from a full clone. |
| `1 branch the code host keeps for its seats' own branches, which live under refs/heads/seats/<handle>/ — refs/heads/seats, and refs/heads/seats/<name> as a branch itself, would block them, since no ref is created beside a directory of its name: "refs/heads/seats/ana" — rename them in the source` | Rename the branch in the source (`git branch -m seats/ana seats-ana` in a clone you then import from, or on the forge) and run the import again; nothing was imported. A branch inside a namespace, `seats/ana/x`, is an ordinary branch and is kept. |
| `refs/heads/main is at … on web and … in the source — this is not a resumed import of the same history` | Something else was pushed to the repository: import into an empty one, or delete the ref there first. |

**Large histories go in steps.** One push carries at most
`code.limits.max_push_bytes` (1 GiB at most), so the import pushes the default
branch along its history in steps that each fit — every step a fast-forward of
the branch itself — then the other branches and tags in batches. Each step is
sent whole and measured before it is sent, so what it sends is what the
repository stores, and no step is refused for its size; a single commit or tag
that is past the limit on its own is refused naming it, since nothing can split
it, and a merge that brings in more than one step is stepped from the other
side first. `crewlet code import … -local -dry-run` prints the plan and pushes
nothing.

**A stepped import stores more than the source.** Every step keeps whole the
files it touches whose earlier versions an earlier step stored, so a history
pushed in steps takes more room than one pack of it — measured on this
project's own history, 34 MB as one pack, about twice that in eleven steps. The
object store keeps the steps' packs as they were pushed, and every git node
serving the repository caches them, until the repository's home git node
merges them ([A busy repository's packs are merged](#a-busy-repositorys-packs-are-merged)).
A merge may store a file a step kept whole as a delta again, but how much that
saves depends on the history, so leave `code.limits.repo_max_bytes`, the object
store and the git nodes' caches room for the repository as it was pushed rather
than for the source's own pack size. Run a large import with
`code.limits.max_push_bytes` at its 1 GiB ceiling, which takes the fewest
steps.

**Resuming.** A step that fails — a network drop, a node restarting, a rule
that refuses it — stops the import with git's own reason, and every step before
it has landed whole. Run **the same command again**: it reads what the
repository already holds and pushes the rest. A branch protected with
`direct_push: none` refuses its own import, as it refuses every push: lift the
rule for the import and put it back after.

Every step, flag and message is in
[`crewlet code import -local`](../reference/cli.md#crewlet-code-import--local).

## Mirroring to another forge

A repository the company keeps here can be published to a repository at
another forge — GitHub, GitLab, a forge on your own network — as a **push
mirror**: its home git node pushes it there whenever it moves, and every 15
minutes while it does not. Declare it on the repository:

```yaml
code:
  native: true
  repos:
    - name: web
      owner: {unit: eng}
      mirror_to:
        url: https://github.com/acme/web.git
        credential: "${GITHUB_MIRROR_TOKEN}"   # the as seat's own token there
        as: jane                               # a seat that reads web
        # refs: [refs/heads/main, refs/heads/release/*, refs/tags/*]
```

```sh
crewlet secrets set GITHUB_MIRROR_TOKEN   # a token of jane's own GitHub account, allowed to push
```

**What reaches the other forge is what passed.** A branch a protection rule
protects is pushed at its **green** commit — the newest landed on it whose
required checks passed — so a commit that has not passed its checks here never
appears there, and a branch with no green commit yet is left as the other
forge has it. Every other branch it covers, and every tag, is pushed as it is.
Left unset, `refs` covers the default branch and every tag; the seats' own
branches (`refs/heads/seats/…`) are never covered. The other forge's copy is
the repository's: the mirror force-pushes onto it and deletes a covered branch
the repository no longer has, so make it a repository nobody pushes to there,
and allow force-pushes on its protected branches. Commits keep their messages
as they are, a coding box's `Crewlet-Seat`, `Crewlet-Run` and
`Crewlet-Work-Item` trailers with them.

**It pushes as the seat `as` names, never as a bot**, with that seat's own token
at the other forge: what appears there is that person's or that agent's push.
The seat must read the repository, and is asked again before every push, so a
chart change that takes its read away stops the mirror. The token is never
written in the company file — `credential` names the variable that holds it,
resolved from the secret store and then the node's environment when it
dials. A forge that wants a particular user name with its token is given it in
the variable itself, as `user:token`.

**When it cannot push**, its home logs `code_mirror_attempt_failed` with the
reason and tries again a minute later, longer after each failure in a row; a
mirror that has not been brought level for half an hour raises
[`code_mirror_failing`](../reference/alarms.md) on its home, which clears with
its next push. Every push that moved something is in the event log as
`code_mirror_pushed`, naming what it set, deleted and held back. The rules are
[Mirrors and imports](../concepts/code-host.md#mirrors-and-imports).

## Status and verify

**`crewlet code status`** lists what is hosted — each repository's status,
owner, default branch and its head, refs, packs, size and last push — and
`-repo web` shows one in detail with its newest pushes: who made each and what
became of every ref, `ng` with git's reason where a push was refused. Run it
beside a node's Tier A file, or anywhere with `-url` and the token exported in
`CREWLET_API_TOKEN`, which needs no Tier A file at all; it reads with the same
token git uses, as the principal that token acts as:

```sh
crewlet code status
crewlet code status -repo web -pushes 20
```

**`crewlet code verify`** checks a git node's **cache** — its disposable copy
of each repository — against the repositories' rows: every pack's bytes, the
refs, and git's own connectivity check. The rows and the object store are the
record and the cache is derived from them, so a damaged copy is one node's —
but it is what that node serves, so run it:

- **on a schedule on each git node** (`-url` names the node): it exits non-zero
  when any copy is not clean, which is the hook for your cron. A repository the
  node holds no copy of is passed over;
- **when git prints an error about a repository on one node and not on the
  others**, with `-repo` naming it;
- and with **`-repair`** to repair what it found, and check the copy again: a
  copy whose own directory is all that is out of place — a file nothing in the
  engine writes, an edited config, a stray pack — is put right in place, its
  packs and indexes kept and nothing downloaded; a damaged one is rebuilt, the
  packs it found whole kept and the rest downloaded again.

```sh
crewlet code verify -url https://git-1.internal.example.com:8080
crewlet code verify -url https://git-1.internal.example.com:8080 -repo web -repair
```

A verify is one of the deployment's own controls, as a backup is, so the token
must carry **`admin`**: any other is refused naming it before the node checks
anything. The check runs on the node, not on your connection: it reads every byte of the
copy — measured at about nine seconds a gigabyte — and the command waits for it
while a proxy's timeout ends nothing, by the deadline the node states for the
check from what the copy holds. A sweep checks every repository at the position
it listed them at, so it costs the code log one barrier however many it
verifies. Every flag and what each line means is in
[`crewlet code verify`](../reference/cli.md#crewlet-code-verify).

Both read the API's `/code/` routes, which a script reads too, with the same
token — `/code/` is always guarded:

```sh
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/code/repos"
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/code/repos/web/pushes?limit=10"
```

A listing is read from the answering node's copy by default; a script that must
not conclude a repository is missing from an old copy asks
`read_level=linearizable`. The routes, their parameters and their answers are
in [API Endpoints](../reference/api-endpoints.md#the-code-host-git-and-code).

## Export

**`crewlet code export`** writes one repository — every branch, every tag and
every object behind them — to a single [git
bundle](https://git-scm.com/docs/git-bundle) on your machine, through your own
git (2.40 or newer) and the same token:

```sh
crewlet code export web web.bundle
git clone web.bundle web          # anywhere, with no engine at all
```

It is how a repository leaves the engine — for an archive, an audit, or another
host — and it is not a backup of the code host: a [backup](backup.md) of a data
node holds every repository's rows and packs, and is what restores the code
host itself. The export needs room for the repository twice beside the bundle
while it runs, since it clones a mirror there and bundles it; see
[`crewlet code export`](../reference/cli.md#crewlet-code-export).

## A busy repository's packs are merged

Every push that carries objects stores one pack, and the repository's
[maintainer](../concepts/code-host.md#who-maintains-a-repository) — the git
node that also publishes its bitmapped index for every other git node, its
home on a fleet of alike nodes — merges its small packs into a few with no step
of yours: it looks every minute and merges them once sixteen have gathered, or
a busy repository's minute of pushes at every look
([Compaction](../concepts/code-host.md#compaction)). The packs a merge replaces
are kept a day for any clone still reading them, then deleted. A repository
that takes pushes faster than its maintainer merges them can reach the 2 048
live packs one may hold: a push carrying objects is then refused
`compaction_behind`, while one that only moves or deletes refs still lands —
run the push again a minute later. The maintainer raises the
`code_compaction_behind` alarm while any repository it maintains stands there,
and its log names what keeps the merge from landing: a node whose memory cannot
hold the merge (`code_cache_compaction_refused`, a node with more memory, or
fewer git transfers, holds it), an object store with no room
(`code_compaction_refused`, `store_full`), or a merge that failed
(`code_compaction_failed`).

## Delete and restore a repository

Remove its entry from `code.repos` and apply: the repository is **deleted** —
it takes no push and is not served — and its refs and packs are kept for
`code.deleted_retention_days` (unset is 7). **Declaring it again under the same
name within that window restores it** as it was, with the settings you declare
it with now. Past the window it is purged, in bounded steps, and its name is
free again. A purge cannot be undone, which is why the window exists; see
[Deletion and purge](../concepts/code-host.md#deletion-and-purge).

## A leaked secret is rotated, then purged

A force-push moves a branch; it does not remove a commit's objects. What no
branch or tag reaches stays in the repository's packs, fetchable by its id,
until an [expunge](../concepts/code-host.md#expunge) drops it — on the
repository's maintainer every week once a quarter of the repository, and at
least 64 MiB, is unreachable, keeping what a force-push or a deletion left
for two weeks. A
credential that was ever pushed is in every clone that fetched it, so:

1. **Rotate it** at whatever issued it. This is the step that makes you
   safe; everything after is tidiness.
2. **Take it out of the history you keep**: rewrite the branches and tags
   that reach it (`git filter-repo` — a revert keeps the commit in the
   history), and force-push them; a protected branch takes the push only
   where its rule's `direct_push` lets a person push and its `allow_force`
   lets the branch be rewritten, so loosen the rule for the push and tighten
   it again ([Protecting a branch](#protecting-a-branch)). An expunge also keeps what work in flight
   still names: every revision of a draft or open change, so close a change
   that ever carried the secret and open a new one from the rewritten
   branch; a queued change's head, so take it out of its queue; and a
   protected branch's last green commit, until a newer commit's checks pass.
3. **Purge the repository's unreachable objects**, so the commit goes now
   rather than two weeks on:

   ```sh
   crewlet code purge-unreachable web
   ```

   or `code_purge_unreachable` from your assistant ([`crewlet code
   purge-unreachable`](../reference/cli.md#crewlet-code-purge-unreachable)),
   or **Purge unreachable history** on the repository's Settings tab in the
   dashboard, which names who still keeps copies before you confirm.
   It is a person's alone, on your own token, and it keeps no grace: every
   object nothing reaches goes, whoever left it and however recently. The
   answer comes once the request is recorded; `crewlet code status -repo web`
   shows it until the maintainer has served it, then when it committed and
   what it dropped.

The answer also says what still holds the secret after the purge: each git
node's cache until it next syncs the repository, the object store for a day,
every backup taken before the purge (on the `nats` store, before the store
deletes it — [A purge and the backups before
it](backup.md#a-purge-and-the-backups-before-it)) and, on a versioned S3
bucket, the old version its own rules keep — and nothing on the server
reaches a clone that already fetched it. Which is why the rotation comes
first ([What still holds a purged
object](../concepts/code-host.md#expunge)).

## See also

- [Code Host](../concepts/code-host.md) — what a repository is, its changes
  and their reviews, the committer, the cache on a git node, and a push end to
  end
- [Deployment: The code host](deployment.md#the-code-host) — git on the host,
  the cache's volume, the balancer in front of `/git/`
- [Running a Fleet](fleet.md) — which nodes to give the `git` role
- [Scaling Out: Git nodes](../concepts/scaling.md#git-nodes) — what git nodes
  and data nodes each scale, and a git node's memory, cores and disk
- [Read Consistency](consistency.md) — what level a git answer and a `/code/`
  read are given
