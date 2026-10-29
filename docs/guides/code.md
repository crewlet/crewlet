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
the subject of the commit you pushed, and git prints which one it opened:

```text
remote: crewlet: change web!42 opened
```

It opens **in the push's own record**, so the commit and the change land
together or not at all. More options shape it:

| Option | What it does |
|---|---|
| `-o change` | Asks for a change. The options below do nothing without it, so a target kept in your `push.pushOption` configuration waits for a push that asks. |
| `-o change.target=<branch>` | The branch it proposes into, by name (`main`) or in full (`refs/heads/main`): the repository's default branch unless you name one. |
| `-o change.title=<title>` | Its title, one line of at most 256 bytes: the subject of the commit you pushed unless you name one. |
| `-o change.description=<text>` | Its description, at most 32 KiB — one line, since git takes no line break in a push option. |
| `-o change.draft` | Opens it as a draft, not yet asking to be merged. |

- **Push the change's branch alone.** A change proposes one branch, so a push
  that asks for one sets exactly one; tags beside it are fine.
- **A branch heads one live change.** A push to a branch that already heads an
  open change revises it — `change web!42 updated` — and opens no other: the
  options shape only a change a push opens.
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

Every git node runs a queued check in a box of the company's sandbox
catalogue ([Who runs a check](../concepts/code-host.md#who-runs-a-check)), but
nothing queues one yet in this release: the file is defined and validated
([What it does not do yet](../concepts/code-host.md#what-it-does-not-do-yet)).

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
   summary says so; its failures and its tail are kept either way.

A check that ran past its `timeout_seconds` is **failed** `timed_out` — the
time is the check's own, so it measured the commit — with its tail and its
log. One that ended **errored** measured nothing: its box or its setup failed
(`box_failed`, `setup_failed`), or its runner was lost (`runner_lost`), and
the engine queues it again, up to its third attempt; a file naming a cell or a
secret the company does not allow is `defs_invalid`, and is not queued again.

## Seats and code

Nothing to set up: once the company hosts code, your seats work in its
repositories through the twenty-nine [code
tools](tools-and-mcp.md#the-companys-code), on the server and in their own
names. A seat holds no git credential and needs none. Every seat reads with
eleven of the tools; a seat whose `role.code.access` is `full_access` also
changes the repositories with the other eighteen; and a seat whose role has a
sandbox works in a clone besides. What each may do is its grant, derived from
the chart ([Who may read and push](../concepts/code-host.md#who-may-read-and-push)).

### What your seats read

Every seat reads the repositories with eleven of the code tools — a file's
lines, a directory, what changed between two commits, how two commits relate,
whether a fix reached a branch and which push put it there, a path's history
and who last changed each line of a file, and a repository's changes: what
each one changes, its findings placed in the revision it reads and what
changed since an earlier revision — with no clone and no token.

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

A seat at `full_access` changes the repositories with the other eighteen code
tools, one change a call: a commit of up to 1 000 file operations to a branch
(`code_commit_files`), a commit reverted or picked onto one (`code_revert`,
`code_cherry_pick`), a branch created or deleted (`code_create_branch`,
`code_delete_branch`), a repository created, changed, renamed, archived,
deleted, restored or protected ([below](#scratch-repositories)), and a change
opened, edited, closed, reviewed and discussed ([Or with a
tool](#open-a-change-from-a-push)). A `read_only` or `review` seat — and
`read_only` is what a seat that sets none runs at — is offered the eleven
reads alone, none of the eighteen. Your assistant has the same eighteen, as
you.

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
([ref drops](../concepts/code-host.md#retention-and-history)). Restore a
branch you need within two weeks of its deletion: after that, it comes back
only while the repository still holds the commit. A repository
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

## Import from GitHub or GitLab

Declare the repository first — the import fills one that `code.repos`
declares, and creates none — then, with your token exported:

```sh
crewlet code import https://github.com/acme/web.git web
crewlet code import git@gitlab.com:acme/web.git web     # SSH works as well
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
side first. `crewlet code import … -dry-run` prints the plan and pushes
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
[`crewlet code import`](../reference/cli.md#crewlet-code-import).

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

## A leaked secret is rotated, not force-pushed away

A force-push moves a branch; it does not remove a commit's objects. Every push
is stored as it arrived, a repository's packs are kept for as long as the
repository is, and an object is fetchable by its id from any node that holds
it. A credential that was ever pushed is a credential to **rotate** — and then
to remove from the history you keep, for tidiness rather than for safety.

## See also

- [Code Host](../concepts/code-host.md) — what a repository is, the committer,
  the cache on a git node, and a push end to end
- [Deployment: The code host](deployment.md#the-code-host) — git on the host,
  the cache's volume, the balancer in front of `/git/`
- [Running a Fleet](fleet.md) — which nodes to give the `git` role
- [Scaling Out: Git nodes](../concepts/scaling.md#git-nodes) — what git nodes
  and data nodes each scale, and a git node's memory, cores and disk
- [Read Consistency](consistency.md) — what level a git answer and a `/code/`
  read are given
