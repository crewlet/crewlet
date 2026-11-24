# Hosting Code on Gitlet

How to host your company's git repositories in the engine, give people a credential, and clone, fetch and push. What a repository *is* (rows on the code log, each push's pack one object in the object store, a disposable cache on every node that serves git) is the [Gitlet](../concepts/gitlet.md) page; this one is the steps.

Not exclusive: the [GitHub](../integrations/github.md) and [GitLab](../integrations/gitlab.md) integrations keep working beside it; a company may host some repositories here and others on a vendor.

---

## Turn it on

Set `code.backend: gitlet` in the company document and declare a repository:

```yaml
code:
  backend: gitlet
  repos:
    - name: web
      owner: { seat: founder }   # a seat's handle, or { unit: Engineering }
```

Apply it like any change (`crewlet config import`, `PUT /config`, or the dashboard). Then:

- **Every data node** commits the pushes. Every node that serves git (Tier A [`node.roles`](../concepts/configuration.md#noderoles) names `gitlet`, or none, since an undeclared node has every role) builds Gitlet and serves `/git/`. It needs **git 2.55 or newer** (the engine's image builds the newest) and a cache place sized by its Tier A [`gitlet` block](../concepts/configuration.md#gitlet); the [deployment guide](deployment.md#gitlet) says how to size both.
- **A node that has run no company yet** starts Gitlet, with no restart, when the activation handing it a company sets `code.backend: gitlet`.
- **Every other node** takes Gitlet on or off at **its restart**: a node builds its native runtime once, from the first company it runs, whatever backends that company names. It logs `native_backend_needs_restart`. Turned on, it answers git and `/gitlet/` `503` naming `native_backend_needs_restart` until restarted, with no `Retry-After` (waiting changes nothing; another node serves). Turned off, it keeps serving git under the last revision that hosted code until restarted. Restart nodes one at a time.
- **A node whose apply fails** (cache directory is another program's, git older than 2.55) answers git and `/gitlet/` `503` naming `config_apply_failed`, never that the company hosts no code; `GET /fleet` and the node's log say why. A node still applying answers `503 applying` with `Retry-After: 5`. A node that failed before it could read the revision (fetch from the coordination store, or opening it with its key) answers `503 code_unknown` (it cannot tell whether the company hosts code), with no `Retry-After`. On `/gitlet/` each is the refusal envelope's error code; on a git route the sentence ends with the code in brackets.

Turning Gitlet off deletes nothing: repositories, rows and packs stay, served again when a revision turns it back on.

## Declare repositories

Each entry under `code.repos` is one repository; every activation makes the fleet match the list:

```yaml
code:
  backend: gitlet
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

A name is lowercase letters, digits and hyphens, at most 63. It is the address, so **renaming a repository deletes it and creates another**. A rule protects the branches or tags its pattern matches: without `allow_force` a branch cannot move to a commit that does not descend from it, without `allow_delete` it cannot be deleted, `direct_push: none` freezes it; the default, `persons`, takes a person's push and never a seat's. `crewlet validate` warns about every repository whose default branch no rule protects (anybody who may push could force-push it). No push deletes the default branch, protected or not: make another branch the default first ([What git prints](../concepts/gitlet.md#what-git-prints-and-what-it-means)). Every field, the pattern grammar and each change's effect at activation: [Declaring repositories](../concepts/gitlet.md#declaring-repositories).

`visibility` decides which **seats** read a repository: `company` (default) is every seat; `unit` only seats the chart relates to the owner (the unit's members and the leads above). It never narrows a person: you, everyone holding the Gitlet permission, and a token carrying it read every repository ([Visibility](../concepts/gitlet.md#visibility)). What keeps a person off the code is the permission itself (below).

`path_owners` names a unit (its lead is asked) or a seat, and the globs it owns. A glob starts at the repository root: `*.go` is Go files at the root only, `**/*.go` every one, `web/**` everything inside `web/`; a glob with no `*` names one file. A path owner is **whom a change touching its paths asks for a review**, and nothing more: owning `deploy/**` lets no seat push there that could not before ([Path owners](../concepts/gitlet.md#path-owners)).

### Or make one in the dashboard

**Gitlet › Repositories** lists the most recently pushed first, with open changes, the default branch's checks and two weeks of pushes; Projects and Scratch narrow it.

**New repository** (`code_create_repo`) makes a project the document does not declare: name, owning unit and optionally a description, default branch and which seats read it; it opens the page. Its **Code** tab adds the first file with **Add a file** (the root commit making the default branch), so no clone is needed; beside it are the clone address and the two commands that push an existing repository in. The tab lists each entry with its last commit; press `t`, or **Go to file**, to find a file by name. Pick a branch or tag from the button before the files; **Find a branch** matches any part of a name. README and LICENSE are tabs under the files. Press a line's number to pick it, Shift for a run, **…** to copy the lines or a link. The rail shows owner and read scope, open changes, who pushed over the last 30 days and pushes per day, mirrors and the newest tag. The page writes from the browser, each write one commit or one ref moved, as you:

- **Code**, on a branch: **Edit** a text file of up to 256 KiB (its pencil, the README's too), **Delete file** (under a file's **More**), or **New file** in a directory (or the plus at the head of a file's tree) (`code_commit_files`), each at the commit the page read, so a push made since is refused rather than written over;
- **Branches**: **New branch** at a branch, tag or commit (`code_create_branch`); **Delete** at the head the row shows (`code_delete_branch`), after confirmation, its toast's **Undo** remaking it at that head. On **Tags**: **New tag** at a branch or commit, annotated where you give a message (`code_create_tag`); a tag's **Delete** at the value shown (`code_delete_tag`), held where a protection rule keeps the tag;
- a commit's page bar: **Browse files**, **Revert** and **Cherry-pick** onto a branch you choose (`code_revert`, `code_cherry_pick`; a conflict commits nothing), **Tag** (`code_create_tag`). The page says how the commit was pushed and landed, and the gate it merged through.

The **Settings** tab sets protection rules ([Protecting a branch](#protecting-a-branch)) and deletes and restores the repository ([Delete and restore a repository](#delete-and-restore-a-repository)). It also changes details (description, default branch, owning unit, which seats read it) with **Edit details**, the name with **Rename** ([Renaming a repository](#renaming-a-repository)), path owners with **Add a path owner** and each owner's **Edit** and **Remove**, and read-only with **Archive repository** (back with **Unarchive repository**), each one `code_update_repo` made as you.

## Give each person a token

**First, the Gitlet permission.** Reaching any repository takes it: the Gitlet workspace, git and a person's assistant's code tools alike ([Who may read and push](../concepts/gitlet.md#who-may-read-and-push)). An administrator holds it through Admin. Give it to everybody else on the code: tick **Gitlet** in their access (Settings › People), or restate what they hold with `gitlet` added (`crewlet iam grant ID -grants worklet,gitlet`; the flag is the whole set). **Invite** and **Add person** open with every app ticked, and `crewlet iam invite` or `create` without `-grants` gives every app. Without it a person is refused every repository, and git prints `remote: You don't have the access this needs (Gitlet). Ask an admin for it (unauthorized)`.

A person pushes with a token that acts as the human seat they hold from joining ([Humans in the Org Chart](../concepts/humans-in-the-org.md)), so nothing is bound. Their **machine token** they mint themselves, signed in as themselves, from the Account page (`#/account`, **Personal access tokens › New token**) or a terminal:

```sh
crewlet iam token -login
```

which asks for login, password and, where held, second factor, and prints the token once ([`crewlet iam`](../reference/cli.md#crewlet-iam)). Revoking it ends it; removing its owner ends every token they minted.

**The token needs Gitlet too.** It carries the permissions it was minted with, cut to what its owner still holds: `crewlet iam token -login` carries everything you hold unless `-grants` says less; the dashboard's **New token** opens with nothing ticked, so tick **Gitlet**. A token minted without it (including one minted before you were given Gitlet, unless it carries Admin) stays without it: mint a new one.

A **Tier A token** pushes too when the identity directory binds it to a human seat: it acts under the login `token:<id>`, and

```sh
crewlet iam create -kind machine -login token:<id> -seat <handle> -grants none
```

binds it in one record, on a human seat nobody holds (a person's seat is theirs alone), so its pushes are that seat's, not any person's ([Acting as your seat](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)). What it may do is its own `grants` entry in `api.auth.tokens`, which must name `gitlet`.

Either way it is the principal the dashboard's writes and an assistant's operator tools act as: each push is one `operator_acted` event with transport `git`, naming the seat, its kind, the credential (`pat:<credential id>` for a machine token, `token:<id>` for a Tier A token), the repository and the outcome; the push history (below) shows the seat as `actor_seat`.

**A credential bound to no seat fetches only** (a Tier A token or service account the directory binds to none). It acts as no person, so a push is refused at its first request: `fatal: remote error: this token is not bound to a person: …`. A pipeline that only clones needs no seat but needs the Gitlet permission; a Tier A entry reads

```yaml
api:
  auth:
    tokens:
      - id: ci-clone
        token: "${CREWLET_API_TOKEN_CI}"
        grants: [gitlet]
```

and a service account is created with it (`crewlet iam create -kind machine
-login ci:clone -grants gitlet`) before its token is minted.

## Clone, fetch and push

The address is the API's own, on `api.port`, never the public listener. A repository's **Code** page offers it at the end of the bar above its files, with a button that copies the whole `git clone` command:

```sh
git clone https://crewlet.example.com/git/web.git
```

The repository's `clone_url` (`GET /gitlet/repos/{name}`) is built from `api.external_url`, so a dashboard reached through another address still shows the one git must use. With no `api.external_url`, none is shown.

git asks for a user name (ignored) and a password (the token). To stop it asking, use the engine's credential helper: it reads the token from your environment, keeps it nowhere else, and is scoped to Gitlet's address and configured **from empty**, so that address has this helper alone:

```sh
export CREWLET_API_TOKEN=…
git config --global credential.https://crewlet.example.com.helper ''
git config --global --add credential.https://crewlet.example.com.helper '!crewlet gitlet credential'
```

The empty value clears other helpers: git hands an answered credential to every listed helper to store, so a storing helper your system configures (osxkeychain on macOS, the credential manager with Git for Windows) would keep the token, answer before this helper, and keep sending a rotated token's old value until a refusal erased it. The helper answers only over HTTPS (or plain http to your own machine); `store` and `erase` do nothing, so the variable is the token's one home ([`crewlet gitlet credential`](../reference/cli.md#crewlet-gitlet-credential)). Your platform's keychain works too (`git config --global credential.helper
osxkeychain`, `libsecret` or `manager`); `credential.helper store` keeps the
token in plain text in `~/.git-credentials`.

**In CI**, give each runner its own token carrying the Gitlet permission (a Tier A entry with `grants: [gitlet]`, or a service account's token minted with it, above). A shared token works while its runners' blobless checkouts' lazy fetches fit together in a quarter of a node's room for large requests, 256 MiB on a four-core node. Checkouts past that wait their turn, up to 55 s, holding their room until the node has synced the repository and git has read them; on a node that has not served the repository lately, whose cold sync can outlast the wait, those past the quarter fail: thirteen lazy fetches of 20 MB fill a four-core node's quarter, and the fourteenth and every later one fail. A token per runner lifts that to the node's whole room, and routing each repository to the nodes that hold it keeps syncs warm ([Deployment](deployment.md)).

A runner's token is also its own turn when the node's transfer slots are all busy: a credential bound to no seat (a Tier A token, or a service account's machine token) is a `machine` in the node's [fair share](../concepts/gitlet.md#fair-share), one principal per credential. With people and machines both waiting, people get about two thirds of what the slots spend and machines one third; among machines the credential that has asked least goes first. So a burst of CI clones waits behind runners that asked less lately, and behind people's clones until people have spent more than twice what the machines have. A token shared by every runner is one principal, so its whole burst waits behind every machine credential that has asked less, on a busy node for as long as ten seconds, when a request still without a slot is answered `503`. A pipeline that pushes acts as the human seat it is bound to, so it counts among the people: its burst waits behind every person who has asked less lately.

To pass the token without a prompt, use the same helper for one command through `-c`:

```sh
CREWLET_API_TOKEN=$CREWLET_TOKEN git -c credential.helper= \
  -c 'credential.helper=!crewlet gitlet credential' \
  clone https://crewlet.example.com/git/web.git
```

(the empty `credential.helper=` first sets aside any helper the runner's configuration lists). Or as a bearer header, which every route accepts:

```sh
git -c "http.extraHeader=Authorization: Bearer $CREWLET_TOKEN" \
  clone https://crewlet.example.com/git/web.git
```

or as the password through `GIT_ASKPASS` (a script git runs to answer prompts):

```sh
printf '#!/bin/sh\necho "$CREWLET_TOKEN"\n' > /tmp/askpass && chmod +x /tmp/askpass
GIT_ASKPASS=/tmp/askpass git clone https://crewlet.example.com/git/web.git
```

Fetch and push as anywhere. A blobless clone (`git clone --filter=blob:none`) needs protocol v2 (git's default since 2.28). A partial clone may ask for `blob:none`, `blob:limit=<n>`, `tree:0`, `object:type=<type>` or a `combine:` of them, the filters git can answer from a repository's bitmap where the node holds one. `sparse:oid` and `tree:<n>` past 0 are refused; a blobless clone with a sparse checkout reaches the same files ([the filters served](../concepts/gitlet.md#a-push-end-to-end)).

Repositories use git's default SHA-1 object format; a local repository created with `--object-format=sha256` cannot push to one.

### What git tells you, and what to do

Every refusal names its reason in the line git prints, except a fetch's request refused mid-transfer, where git prints only the HTTP status and the node's log keeps the reason. The ones a person meets most:

| git prints | What to do |
|---|---|
| `repository "<name>" not found` | Check the name in `code.repos`. |
| `You don't have the access this needs (Gitlet). Ask an admin for it (unauthorized)` | You or your token lack the Gitlet permission. Ask an administrator for **Gitlet** (Settings › People); a token minted without it stays without it, so mint a new one. A CI runner's Tier A entry needs `gitlet` in `grants`. |
| `this token is not bound to a person` | Push with a token that acts as your seat: your machine token (`crewlet iam token -login`, or the Account page). |
| `(stale: refs/heads/main is at <id>; fetch first)` | Fetch, integrate, push again. |
| `(protected_force: …)`, `(protected_delete: …)`, `(protected_direct: …)` | The named rule says what it allows; change it in `code.repos` for a declared repository, by a person's `code_protect_branch` for one a tool made ([Who may change a protection](../concepts/gitlet.md#who-may-change-a-protection)). |
| `(ref_conflict: refs/heads/feature is in the way: …)` | A branch and a directory of branches never share a name (`feature` beside `feature/x`). Delete the one in the way in the same push (`git push origin :feature HEAD:feature/x`) or rename; a clone holding the old one runs `git fetch --prune`. |
| `(pending: durable at <position>, …)` | Recorded, but not applied on the answering node within a minute. Run the same `git push` again: if it landed, git says `Everything up-to-date`. |
| `(unknown: outcome unknown (op …); …)` | The answer was lost; it may or may not have landed. Fetch, then push again. |
| `(not_applied: …)` | Recorded and every node dropped it: nothing landed. Push again. |
| `(compaction_behind: <repo> holds <n> live packs, …)` | The repository holds as many packs as one may until its maintainer (the Gitlet node that merges its packs and publishes its layers) merges them, every minute: push again in a minute. If it persists, the node's `code_compaction_behind` alarm is up and its log says why ([A busy repository's packs are merged](#a-busy-repositorys-packs-are-merged)). |
| `… retry in 30 s …` (a `503`) | The node is draining or stopping: retry; a balancer sends you elsewhere. |
| `error: RPC failed; HTTP 503 …` then `fatal: could not fetch <id> from promisor remote` (the checkout of a blobless clone) | The checkout's one fetch for missing files found the node's room for large fetch requests in use (it waited 55 s, or found no place to wait), and git does not retry: run the checkout again. git prints no reason for a request answered mid-transfer; the node's `code_fetch_spool_unserved` line names the token and what held it back. If the token's own requests did, it is shared by several CI runners: give each its own. |

An `ok` from the engine means the push's own record **has been applied**, never a push merely on its way. Every other condition, the exact line git prints and what to do about it is in [What git prints, and what it means](../concepts/gitlet.md#what-git-prints-and-what-it-means).

## Download a file or a commit

**Code** › **Download ZIP** downloads every file of the commit on show, as `/git/{name}/-/archive.zip` serves it.

Anybody who may fetch a repository may download one file of a commit, or the commit as a ZIP, without git: signed in on the dashboard, from a file's **Raw** and **Download** or **Code** › **Download ZIP**, or from a script with a token. Name the commit by its whole id:

```sh
curl -fsS -H "Authorization: Bearer $CREWLET_TOKEN" -o web.zip \
  "https://crewlet.example.com/git/web/-/archive.zip?rev=$(git rev-parse HEAD)"
curl -fsS -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/git/web/-/raw?rev=$(git rev-parse HEAD)&path=docs%2FREADME.md"
```

- **The token goes with the request.** A download never asks for a password, so a browser never prompts: send a bearer, or the Basic password up front (`curl -u :$CREWLET_TOKEN`). A client that sends Basic only after a challenge, such as `wget` without `--auth-no-challenge`, sends nothing and is refused `401`.
- **Percent-encode the path**: `/` as `%2F` or itself, `+` as `%2B`, a non-UTF-8 name byte by byte. Add `&download=1` to save even a text file rather than show it.
- **A ZIP is every file as committed**: no `.gitattributes` rule applies, so `export-ignore`d files are in it and `export-subst` placeholders stay as written. It is byte for byte what `git archive --worktree-attributes
  --format=zip --prefix=web-<first seven digits>/ <commit>` writes in a bare
  clone on the same git, with no global, system or `info/attributes` file of
  your own.
- **Limits.** A commit past a million entries, 4 GiB of files or 64 MiB of paths, or holding a path too long for a ZIP, is refused `400`: `git clone --depth 1` instead. One ZIP at a time per person per node (a second is `429`); a large one waits for one of the node's places for large ZIPs, and a `503` with `Retry-After` says to retry shortly or on another node. Costs: [Downloading a file or a commit](../concepts/gitlet.md#downloading-a-file-or-a-commit).

## Open a change from a push

A push opens a [change](../concepts/gitlet.md#changes) (`web!42`, proposing its branch into another) when it asks with a push option:

```sh
git push -o change origin feature
```

The change proposes `feature` into the default branch, titled by the subject of the pushed commit; git prints which one it opened, with its dashboard link on `api.external_url`:

```text
remote: crewlet: change web!42 opened: https://crewlet.example.com/dashboard#/gitlet/repos/web/changes/42
```

It opens **in the push's own record**, so commit and change land together or not at all. More options shape it:

| Option | What it does |
|---|---|
| `-o change` | Asks for a change. The options below do nothing without it, so a target kept in your `push.pushOption` configuration waits for a push that asks. |
| `-o change.target=<branch>` | The branch it proposes into, by name (`main`) or in full (`refs/heads/main`); default branch unless named. |
| `-o change.title=<title>` | One line, at most 256 bytes; default the pushed commit's subject (a commit with no message: the branch's name). |
| `-o change.description=<text>` | At most 32 KiB, one line (git takes no line break in a push option). |
| `-o change.draft` | Opens it as a draft, not yet asking to be merged. |

- **Push the change's branch alone.** A change proposes one branch, so a push that asks for one sets exactly one; tags beside it are fine.
- **A branch heads one live change.** A push to a branch already heading an open change revises it (`change web!42 updated`, same link) and opens no other; the options shape only a change a push opens.
- **The push lands whatever becomes of its change.** An option the engine does not read, a target that is no branch, or a branch already at its target's commit lands the push with no change, and git says why in the same line, `change not opened: <code>: <why>` ([every line it prints](../concepts/gitlet.md#when-a-push-asked-for-a-change)). Pushing the same commit again moves nothing: fix what the line says and push the branch's next commit with `-o change`.
- **Every other option is read by nobody**: `ci.skip`, or any other a forge takes.

Whoever may push the branch may open a change from it: the push's own update is the check ([A change a push opens](../concepts/gitlet.md#a-change-a-push-opens)).

**Or with a tool.** A seat, or your assistant as you, opens a change of an already pushed branch with `code_open_change`, naming the branch, a title and, if not the default branch, its target. It carries the work item the seat's turn is charged to unless the call names another (a native key (`ENG-42`), a tracker's item as `<backend>:<id>`, or `none`), and asks the `reviewers` it names once open. `code_update_change` changes one thing per call (what it says, draft or not, target); `code_close_change` closes one or, with `reopen`, reopens it. Each names the `version` its caller read; a change that moved since is refused `stale_version`. Who may is the [change's rule](../concepts/gitlet.md#opening-editing-and-closing-a-change): whoever may push its branch opens one; its owner, a maintainer or any person steers it ([the tools](tools-and-mcp.md#the-companys-code)).

## Review a change

A change is reviewed on its page in the dashboard's **Gitlet** workspace (`g g`), or through the code tools by a seat or your assistant as you. Each ask, review, comment and answer is one record of the change's **conversation** ([Reviews and findings](../concepts/gitlet.md#reviews-and-findings)), and each tells the seats and people it concerns.

**Find what waits on you.** **Gitlet › Changes** lists open changes. **Search changes** finds by title and description words, narrowed by qualifiers such as `is:draft`, `author:@me` or `waiting-on:review`; **Readiness** lists open changes ready to merge or waiting on a review, checks, findings or a person. **Waiting on you**, above the list, names each change waiting on your review or fix and why; **Review** opens its files. A record that concerns you leaves an **Inbox** notice (the change, why you hear of it ("asked for your review", "asked to look again"), where its merge stands), whose **Open the diff** opens its files. A seat is woken instead, told the change, why and the tool that answers ([Who a change wakes](../concepts/gitlet.md#who-a-change-wakes)).

**Ask for a review.** **Request review** (the change page's **Review** menu, or **Request** at the head of the rail's Reviewers) names reviewers by handle, with a note; a seat or your assistant uses `code_request_review`, which also withdraws an ask. An author of the change, anybody who reviews the repository and any person may ask, at most 32 reviewers at once. A person may be asked only where they hold the Gitlet permission: an ask naming somebody without it is refused `not_permitted`, saying an administrator gives them Gitlet first. A seat may be asked only where its grant lets it review the repository, and never an author of the change, whose review counts for nothing ([Asking for review](../concepts/gitlet.md#asking-for-review)). A change whose revision touches paths the repository's `path_owners` name asks their owners itself, once its home has read the revision ([Path owners](../concepts/gitlet.md#path-owners)).

**Read it.** **Conversation** is the timeline: description, then every revision, review, ask and thread in the order its records landed, then the merge box (a section per condition to merge, where it stands, and the merge controls) and a comment box (markdown, **Mod+Enter** sends), beside a rail of whom it asked and where each stands, readiness, branches, checks, participants and whether it notified you. **Revisions** lists revisions, **Checks** the checks that judge it, and **Files changed** is every file a revision changes on one page, beside its tree (the head unless you pick another, or what changed since an earlier revision), with each finding under its lines; a finding whose lines have since changed is drawn above the diff, never dropped. **More** is where it came from and whom it notified. A seat reads the same with `code_get_change`, a part at a time ([Reading a conversation](../concepts/gitlet.md#reading-a-conversation)).

**Edit it.** **Edit**, in the description's menu, changes the change's title and description; a seat or your assistant uses `code_update_change`.

**Review it.** On the head revision, pick lines by number and draft a finding: its weight (`blocker`, `major`, `minor`, `nit`), what it is about, a title, a body and, on the new side, the lines you suggest instead. Drafts stay in the browser tab until **Approve**, **Request changes** or **Comment** (the page bar's **Review**, or **Finish review** on Files changed) sends them all as ONE review with your summary: a review and its findings are one record, never half there. A seat uses `code_review_change`. Anybody who reviews the repository, and any person, may review, never an author of the change. Edit a draft where it stands; filter files by name, extension or findings, and press `t` to go to a file. Mark a file **Viewed** to fold it until a revision changes it, and filter to the Unviewed ones.

- **A request for changes stands until its reviewer reviews again.** A push answers none: the reviewer's next review, approving or asking again, does.
- **A review of a later revision carries what still stands.** It names your findings that still stand and should (the page offers each, checked); every other finding you raised on an earlier revision is superseded. A second review of the same revision adds to the first.
- **An approval carries to a later revision only where nothing it approved changed**: the same code, or the same diff (a rebase that moved nothing near the change keeps it) ([Merge readiness](../concepts/gitlet.md#merge-readiness)).

**Answer the findings.** An author answers a finding `resolved`, `wont_fix` or `disputed` with `code_resolve_finding` (a seat, or your assistant as you) and says why in a reply on its thread, which the page offers under every finding. An answered blocker still stands, for its raiser to judge: only its raiser's dismissal, a maintainer's or any person's (each `dismissed` through the same tool), or the raiser's review of a later revision not carrying it, clears it ([Findings](../concepts/gitlet.md#findings)). **Apply suggestion** (`code_apply_suggestions`) commits a finding's suggested lines onto the change's branch as one commit of yours, against the head on screen, and answers the finding resolved; it makes you an author of the change, so your approval of it no longer counts ([A review's suggestions applied](../concepts/gitlet.md#a-reviews-suggestions-applied)).

**Discuss it.** A comment (`code_comment_on_change`) starts a thread (on a line of a revision, or the whole change) or replies to a thread's first comment or a finding: threads are one level deep. Whoever started a thread, an author, a maintainer or any person resolves it. A comment's menu edits it (its author alone) and deletes it (`code_delete_comment`; author, maintainer or any person): a deleted comment keeps its place with its words cleared, and replies stay ([Comments and threads](../concepts/gitlet.md#comments-and-threads)).

**See whether it is ready.** Readiness is one judgement on one snapshot: head's facts read, no draft, an approval counted where the branch's gate asks one, no blocker standing, a clean merge into the target, and whatever else the gate adds; it gives the first unmet precondition in one sentence naming who can clear it ([Merge readiness](../concepts/gitlet.md#merge-readiness)). Authors whom the gate lets merge it hear `ready_to_merge` once, when it turns ready (where the gate lets none, the lead of the repository's owner; [Who a change wakes](../concepts/gitlet.md#who-a-change-wakes)), and merge as [Merging a change](#merging-a-change) says.

**Who heard about it.** The **More** tab shows whom each record reached, newest first, each with the one reason; the same answer is `GET /gitlet/repos/{name}/changes/{n}/routing` ([A change's questions](../reference/api-endpoints.md#a-changes-questions)).

### The Inbox card

Code notices are listed only while you hold the Gitlet permission, as work notices are while you hold Worklet's: without it none is listed, counted or pushed, and those written meanwhile appear once an administrator gives you Gitlet. A change notice opens as a card leading with what the change is FOR: title, the work item it implements and the opening of its description, then what the record said, then **Where it stands** (the engine's sentence on its merge and the open agent findings, by weight) and **Open the diff**, one press to its files.

While the change is live and its branch has a merge gate, the card also says **Its gate**, read for the one card you have open (the list asks nothing per card):

- **What its checks measured**: each required check on the change's head at its latest attempt (a rerun stands in for the attempt it reran), with state and duration (`unit failed in 1m 4s`, `e2e running`); durations, never tokens. A branch requiring no check draws no list, not "none measured".
- **Its change classes**: how many touched paths change only whitespace or only comments and, where every path was classified, that the rest change code ([Change classes](../concepts/gitlet.md#change-classes)).
- **The human paths that asked for you**: where the gate holds a touched path to a person's approval and waits on you, the engine's sentence naming the globs and branch. Only the person it waits on sees it.

A merged or closed change has no gate left, and its card asks for none.

## Merging a change

A ready change is merged with `code_merge_change`, naming the change and the head you read (`expected_head`), by a seat, or by your assistant or the dashboard as you. Gitlet builds the merge on the repository's home Gitlet node onto the target's tip and lands it, the target moved and the change merged in **one record** its committer decides against the target's merge gate. It answers one of three things:

- **merged**: the commit the target moved to;
- **queued at place n**: a target whose gate requires a check queues the change instead, and the queue's landing is the merge: its people hear `merged` when it lands. `code_dequeue_change` takes it out, at its enqueuer's, owner's, a maintainer's or a person's ask;
- **`not_ready`**: the gate does not allow it yet. The answer names **every** unmet precondition and who each waits on; nothing is merged or queued, and its authors who may merge it hear `ready_to_merge` once the last is met (where none may, the lead of the repository's owner). A seat is told to end its turn rather than ask again.

`method` is `merge` (a merge commit, the default), `squash` or `fast_forward`. `delete_branch` deletes the change's branch as it merges; unsaid, a seat's branch (`seats/<handle>/…`) is deleted unless a rule over the target keeps it (`delete_source: none`) and any other branch is kept. Gitlet decides this with the merge and says it in the answer (`deleted_branch`). `close_task` closes the change's work item once it merges: a second write of the same operation (the same call again is the same close), and the tracker's write, decided as `update_work_item` is: a person who does not use the tracker has the change merged and the item left as it was. A close that fails or is refused leaves the change merged and says why (`merged; ENG-42 not closed: …`). A queued change's landing closes it as whoever queued it (where a seat queued it in a turn it worked for somebody, as that somebody, within the apps they use, as the seat's own close would be in that turn) once the landing has applied. The close stays owed on the queue's rows until made, so a home that stops or a tracker that does not answer delays it to the next turn rather than losing it. A close that will never be made (a vendor's item, a queuer who may no longer update it, somebody it was queued for who has left, may not act now or does not use the tracker, an item Worklet no longer holds or will not set done) is told to the change's authors and the queuer under `merged`. `override_reason` is a person's reason to merge past the gate, merged directly and recorded on the change; a seat cannot override.
### From the change's page

A live change's page (**Gitlet › Changes**, then the change) has the same
controls in its **Conversation**'s merge box, after the timeline. Each acts as
you and never ahead of the engine's answer:

- **Where its gate stands**: sections for reviews, checks (each check's latest
  attempt, how long it ran, whether it is required, with **Details**), findings,
  facts and whether it merges, each judged with *you* as the merger, then the count and what it
  still waits on. **Re-request** in the reviews section asks again everyone
  whose review is not an approval of the head.
- **Merge**, or **Add to queue** where the target's gate requires a check, with
  the method (**Merge commit**, **Squash** or **Fast-forward**), **Delete its
  branch** (unticked, the rule above decides) and **Close ENG-42 once it
  merges** where the change names a work item. It sends the head the page shows
  as `expected_head`, so a push that landed since you opened the page is
  refused, not merged unseen. While the gate is unmet the button is held with
  the gate's own sentence, and a line beside it says what the press will do.
- **Override**, a person's alone: a dialog asks your reason, says what the gate
  still waits on, and merges directly past it, even into a branch whose merges
  land through its queue. The reason is kept with the merge (the change's More
  tab shows it) and the override is audited.
- **Re-run failed checks**, in the checks section where one failed: every check
  of the head whose latest attempt did not pass, each at its next attempt
  (`code_rerun_checks` with `all_failed`).
- **Rebase**, in the merges section: replays the change's commits onto its
  target's tip ([Rebasing a change](#rebasing-a-change)), after a confirmation
  that its branch moves by one forced push from the head the page shows.
- **Take out of the queue**, in the queue's section while queued: the change
  leaves its target's merge queue and stays open (`code_dequeue_change`), after
  a confirmation. **Gitlet › Queue** offers the same on each entry.

A merged change shows **Revert** in their place ([Reverting a
change](#reverting-a-change)): a confirmation says what it opens, with a box to
merge the revert once open. A standing finding is answered under its review or
on the **Files changed** tab: **Resolve** in one press, for an author of the
change, or **Answer** with Resolved, Won't fix or Disputed by an author, or
Dismiss by its raiser, a maintainer or a person (`code_resolve_finding`).

A refusal is said beside the controls: a head that moved ("somebody pushed to
it since you loaded it"), a target that moved under every rebuild of the merge,
a change that does not merge cleanly onto the target's tip, a full queue; and,
in the engine's words where only it knows what to say, a gate the change does
not meet yet (`not_ready`, naming what it waits for and who can move it), a
merge that would delete a branch a protection rule keeps or one you may not
delete, a repository at the most it may hold, and a merge asked for somebody
who may not make it.

## Protecting a branch

A branch's protection is a rule over the branches a pattern matches: what a
push may do to them ([Protection
rules](../concepts/gitlet.md#protection-rules)) and, with a **merge gate**,
what a change must meet before it merges into one. Unset, a rule's
`direct_push` lets only a person push to the branch, so a seat's work reaches it
as a change. A rule is declared with the repository in the company document, or
set on a repository the document does not declare with `code_protect_branch`:
by a seat, by your own assistant, or by you on the repository's **Settings** tab
in the dashboard, where **Protect a branch** adds a rule, each rule has **Edit**
and **Remove**, and the row of an unprotected branch on the **Branches** tab has
**Protect** ([Who may change a
protection](../concepts/gitlet.md#who-may-change-a-protection)):

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

One change through it: a seat pushes `seats/dev/retry` and opens `web!42` into
`main` ([Open a change from a push](#open-a-change-from-a-push)). The head's
checks run, from `.crewlet/checks.yaml` as `main` holds it ([Adding
checks](#adding-checks)); a reviewer approves; a person approves the `infra/`
paths. Readiness, on its page and in `code_get_change`, lists each condition and
whom it waits on; its authors who may merge into `main` hear `ready_to_merge`
when the last is met. Because the gate requires checks, a merge **adds it to
`main`'s queue**, which tests it merged on the branch before it lands ([The
merge queue](../concepts/gitlet.md#the-merge-queue)); `main`'s tip is checked
again once it lands, `code_green` advancing on green ([From a push to a green
branch](../concepts/gitlet.md#from-a-push-to-a-green-branch)).

**Settings › Gitlet** lists every protected repository, a page at a time with
**Load more**, from `GET /gitlet/repos?protected=true` (an index of protected
repositories alone). Each rule shows what it lets a push do and, in its fold,
what its gate asks and **who set it and when**: the seat, person or credential
whose request last changed the rule, or the company document for a declared one.
A change to something else (the description, another rule) leaves a rule naming
whoever last set it. The card also says where the rules are set: the company
document, or the code tools. A repository's own page shows the same under its
**Settings** tab. A seat may tighten a gate and never loosen it ([Who may change
a protection](../concepts/gitlet.md#who-may-change-a-protection)); every gate
field is in [Merge gates](../concepts/gitlet.md#merge-gates).

## Rebasing a change

A change whose target has moved on is rebased with `code_rebase_change`, naming
the change and the head you read (`expected_head`): by a seat, or by your own
assistant or the dashboard's **Rebase** on the change's page, as you. Gitlet
replays the change's commits the target does not hold onto the target's tip,
oldest first, and moves the change's branch to the last with **one forced push**
of yours: the branch's protections and your right to force-push it apply as to a
`git push --force`, and the change records a new revision at its new head.

- **Each commit stays its author's.** A replayed commit keeps its author's name,
  address and date (zone included) and its message byte for byte; only its
  committer is the engine's, `Crewlet <engine@<code.identity_domain>>`, dated
  when you asked.
- **The first conflict stops it.** A commit that does not apply ends the rebase
  there, nothing changed: the answer names the commit (`2 of 5`) and each
  conflicted path with its stages' ids and the start of the file as git marked
  it. Resolve it in a coding box, or run `git rebase` yourself and push.
- **A commit already in the target is dropped**, as `git rebase` drops it.
- **What it refuses:** more than 1 000 commits the target does not hold
  (`too_many_commits`); a history holding a merge commit (`merge_commit`, a
  merge needs a mainline to replay); a commit git would not write back as it was,
  such as an author in a form git trims or a message not in UTF-8
  (`rejected_object`). Each is a rebase for a coding box or your own clone.

The rebase is built on the repository's [home Gitlet
node](../concepts/gitlet.md#the-home-gitlet-node) under forty seconds of git
([How a rebase is built](../concepts/gitlet.md#a-change-rebased)).

## Reverting a change

A merged change that turns out wrong is undone by **a change of its own**, never
by moving its target back: `code_revert_change`, naming the change, by a seat,
or by your own assistant or the dashboard's **Revert** on the merged change's
page, as you. Gitlet undoes what the merge brought in, onto the target's current
tip, as one commit of yours on a **new branch**, `seats/<handle>/revert-<n>` for
a seat and `revert-<n>` for a person. Pushing it opens the change **"Revert
web!42"** into the same target, in the same record, gated, reviewed and merged
like any other. Pass `merge` to merge it as the call's second write, as far as
the target's gate allows; a merge it could not make yet is said beside the
opened change, and the revert stays open. The merged change's page links its
revert in place of **Revert** while the revert is open or merged, and offers
Revert again once it is closed.

- **Every way it was merged reverts the same way.** A merge commit is reverted
  against its first parent (git's `revert -m 1`), a squash against its one
  parent, and a fast-forward over the change's whole range from the base its
  head revision was measured against: the revert is the whole change, as one
  commit.
- **A conflict stops it.** Where the target has since changed what the change
  changed, nothing is created: the answer names each conflicted path with its
  stages' ids and the start of the file as git marked it. Resolve it in a coding
  box, or run `git revert` yourself and push.
- **What it refuses:** a change that is not merged (`not_merged`), and a revert
  branch already there (`exists`: a revert is under way, so find its change, or
  delete the branch to revert again).

The revert is built on the repository's [home Gitlet
node](../concepts/gitlet.md#the-home-gitlet-node) as any engine write is ([How a
revert is built](../concepts/gitlet.md#a-change-reverted)).

## Adding checks

A repository's checks (the builds, tests and linters a change is measured by)
live in `.crewlet/checks.yaml` at the root of its tree:

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

Required keys: `name`, the command to `run`, and `timeout_seconds`. The rest
narrow it: `paths` runs a check only for a change touching one of its globs,
`report` names the JUnit or SARIF file it writes, `checkout` checks out a cone
of directories (or `none`), and `env` names the company's check secrets it is
given. Every check runs in a container made from the company's
`providers.sandbox.local.image` on a Gitlet node; a company that sets none runs
no check. Every key,
bound and reason is in [The checks
file](../concepts/gitlet.md#the-checks-file).

1. **Write the file, and check it before you push it.** It is read strictly, as
   a config document is: an unknown key, a key written twice, or a number where
   text belongs (`name: 1.10` is a number: quote it) is refused. `crewlet
   validate` reads it exactly as Gitlet does, with no node, network or
   credential:

   ```bash
   crewlet validate .crewlet/checks.yaml
   # .crewlet/checks.yaml: 3 checks (unit, lint, web)
   ```

   A broken file names every fault at once, each at its field and line: the
   first 128, and how many more there are. `-json` gives them as a fix loop
   reads them:

   ```bash
   crewlet validate .crewlet/checks.yaml -json
   ```

   ```json
   { "valid": false, "tier": "checks", "file": ".crewlet/checks.yaml",
     "problems": [ { "path": "checks[0].timeout_seconds", "kind": "out_of_range", "line": 4,
       "message": "checks[0].timeout_seconds: value out of range: 99999 seconds: a check runs for 1 to 21600 seconds (gitlet.CheckTimeoutCeiling, six hours) (line 4)" } ],
     "warnings": [] }
   ```

2. **Land it on the branch it governs.** A change's checks are the ones its
   *target* branch defines, never its own: a change that edits `checks.yaml` is
   checked by the file it edits, and its edit governs the changes after it
   lands.

Once the file is on a branch, the repository's home Gitlet node queues its
checks at the head of every live change into that branch ([How a head's checks
are
dispatched](../concepts/gitlet.md#how-a-heads-checks-are-dispatched)) and, on a
protected branch, the required ones at its newest landing ([Tip checks and
code_green](../concepts/gitlet.md#tip-checks-and-code_green)). Every Gitlet node
runs a queued check in a box of the company's sandbox catalogue ([Who runs a
check](../concepts/gitlet.md#who-runs-a-check)). A change's readiness counts the
checks its branch's gate requires, and a merge into that branch puts the change
in its merge queue.

**A branch whose gate requires a check lands through its merge queue.** The home
Gitlet node tests waiting changes merged on the branch before any lands: up to
`depth` candidates at once, each one change while the queue keeps up and up to
`batch` as it falls behind, every tip checked under the branch's own
`checks.yaml`. The longest run of candidates whose newest tip is green lands in
one go, each change merged as whoever queued it (a work item they asked closed
is closed as them). A red candidate is bisected until the breaking change is
found; only it leaves the queue (its `code_queue_ejected` event names the
checks) and the rest are tested again. A change that conflicts with those ahead
leaves the queue naming the paths, and a push to the branch restarts testing
candidates on the new tip. [What the home does with a
queue](../concepts/gitlet.md#what-the-home-does-with-a-queue) has the detail.

### Choosing a queue's depth and batch

A queue lands at most `depth` × `batch` changes a round, and a round lasts as
long as the slowest required check: at ten-minute checks and one change a
candidate, the default depth of four lands 576 changes a day and the most, eight,
1 152. Size it for how many changes the branch must land:

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
change a day for every seat that may merge into the branch, priced at a round as
long as `code.checks.max_timeout_seconds` (the longest a check may run), so set
that to what your checks take. A running queue asked past what it lands raises
`code_merge_rate_over_capacity` on its home Gitlet node, priced at the rounds
its trains took over the last hour. Either way, raise `depth` or `batch`, make
the required checks faster or fewer, or split the work over more branches or
repositories ([How fast a queue
lands](../concepts/gitlet.md#how-fast-a-queue-lands)).

## Reading a failed check

A check that fails says why in four places, each on its attempt, read whole with
`code.read`'s `check` ([Reading a repository's
checks](../concepts/gitlet.md#reading-a-repositorys-checks)):

1. **Its summary**, the line you read first. Write it yourself: the check's
   command is told a file in `$CREWLET_CHECK_SUMMARY`, and whatever it writes
   there (up to 4 KiB) is the summary, over anything the engine could say.
   Otherwise the summary is what the check's reports count (`12 of 340 tests
   failed`, `5 findings at error or warning`), and failing that its exit status
   and duration (`exited 2 after 3m12s`). A report named and not read is said
   after it.

   ```bash
   # in the check's command
   go test ./... 2>&1 | tee out/test.log
   echo "$(grep -c '^--- FAIL' out/test.log) tests failed" > "$CREWLET_CHECK_SUMMARY"
   ```

2. **Its failures**: what the reports its file names (`report: {junit: …,
   sarif: …}`, paths in its checkout) say failed, read once its command ends:
   each a test id, a file and a line, and a message of up to 2 KiB (a JUnit
   case's `<failure>` or `<error>` with its classname and name; a SARIF result
   at level `error` or `warning` with its rule). At most **200**, in the
   reports' order, within 64 KiB together; the rest are counted, not listed. A
   report is read up to 32 MiB.

3. **Its tail**: the last 16 KiB of its output, where a build that failed before
   any test ran says why.

4. **Its log**: the last 64 MiB of its output, kept as one object in the fleet's
   object store and read a page at a time, never in a row. A passed check keeps
   its summary alone, so nothing of a green run is stored. Where the object
   store has no room, the check ends without its log and its summary says so;
   failures and tail are kept either way. Only those 64 MiB are read from the
   box, however much more was written, and reports and log are read within a
   minute of its end: a slower box ends its check without what was still
   unread, its summary saying so, rather than leaving it running.

A check that ran past its `timeout_seconds` is **failed** `timed_out` (the time
is the check's own, so it measured the commit), with its tail and log. One that
ended **errored** measured nothing: its box or setup failed (`box_failed`,
`setup_failed`) or its runner was lost (`runner_lost`), and the engine queues it
again, up to its third attempt; a file naming a cell or secret the company does
not allow is `defs_invalid` and is not queued again.

A seat reads all four with `code_get_check_failures`, naming the commit by
`change` (its head) or `sha` and the check by `name`: failures a page at a time,
the log 32 KiB a page, every page from the attempt the first read even once a
rerun ends or the head moves, and every word fenced as text nobody in the
company wrote ([Text somebody else
wrote](../concepts/agent-runtime.md#text-somebody-else-wrote)).
`code_get_change`'s `checks` part lists a change's head's checks, a line each.
**Running a check again** is `code_rerun_checks`, naming each check and the
attempt you read as its latest (or `all_failed`), so two people who read the
same red check queue one attempt, never two. A person reruns any ended attempt;
a seat only one its infrastructure or a stop ended (`errored` or `cancelled`),
and only on a repository it may write or a change it wrote: a failure measures
the code, and a seat able to rerun one would re-roll a flaky test until it
passed. A check runs at most ten attempts.

## The Queue and Checks

The dashboard's Gitlet workspace draws both, over the same questions a seat's
tools ask.

**Gitlet › Queue** (`#/gitlet/queue`) is the merge queues of your gated
branches: a box per repository and branch with its train base and its entries in
landing order, each with its change's title, who enqueued it, where it stands
and a meter of its train's **required checks** (how long they have run against
the slowest declared `timeout_seconds`, since the queue waits for its slowest
required check). Under each box, in the danger tone, is what the queue **put out
in the last day**: the change, why (`a required check failed`, `its merge
conflicted`, `it no longer met the branch's gate`, `its head moved`, `the branch
moved under the queue`) and the required checks that failed on what it was
tested in or the paths its merge conflicted on. **View the run** opens the check
that failed on the train, and **Re-queue** adds the change again as it was. A
branch with nothing queued and nothing put out lately draws no box, and a
company with nothing queued says so. Queues are read fifty entries at a time;
**Load more** reads the next fifty.

**Gitlet › Checks** (`#/gitlet/checks`) is where every check stands: each
check's latest run on a live change, a queue's candidate or a protected
branch's tip, over the last day, week or month, with tabs counting each state,
failing runs first (or **Newest first**), narrowed by check name and repository.
Press a run to open it beside the list; the address keeps it, so a link or
reload reopens it. A repository's page has its own **Checks** tab, every attempt
it keeps with no window, and a change's page has one too: the attempts that
judge it, its head's, checked for its target.

An open attempt shows: how many tests failed of how many ran, and its exit code;
what it was checking (a change's head, a queue's candidate merged onto its
branch, a branch's tip, a landing bisected), the commit and branch, how long it
ran and why it ended; **Re-run**, which runs that check again once ended;
**Open in CI** for a run another CI reported; its **failures**, each `path:line`
a link into the diff it was checked on (its base against its commit), opened on
that file with the failing line marked and scrolled to however long the patch
is: the page reads the patch only as far as the page holding the line. Where the
line is outside what the diff changes, the page says so as soon as the patch
reaches past it and offers the file at the attempt's commit, opened on that
line; an attempt that records no base opens the file there directly. Also **the
end of its output**, numbered from its place in the whole output, failing lines
marked; and **Read its log**, which reads the log a failed run kept a page at a
time, only when you ask. A check measures durations and exit codes, never
tokens: it runs no model.

## External CI

A check the engine does not run (your own CI's, on a runner of yours) is
reported onto the same rows, so a change's readiness, its checks part and the
dashboard read it beside the engine's own. A report is a **person's**: your CI
calls `code_report_check` over [your own
assistant's](../reference/api-endpoints.md#operatormcp--your-own-assistant)
surface with a token of your own (`crewlet iam token -login`), and the report
names you. You and the token both need the Gitlet permission. A token bound to
no seat only reads, and no seat may report: a check result is a measurement, and
a seat runs no CI whose result it could send.

```json
{"name": "code_report_check", "arguments": {
  "repo": "web", "sha": "<the commit's whole id>", "name": "e2e",
  "status": "running", "url": "https://ci.example.com/runs/812"}}
```

Report `running` when the run starts, then how it ended (`passed`, `failed`,
`errored` or `cancelled`) with its `summary`, `tail` and `failures` (each a
`path`, `line`, `test_id` and `message`, at the bounds an engine check keeps),
your JUnit report's `tests` (`total`, `failed`, `skipped`), `log_lines` (how
many lines the run's whole output has), `tail_cut` where the `tail` you send
begins inside a line (so a read numbers the tail's lines), and the run's `url`.
Name the commit by `sha` and the branch it is checked for by `checked_for` (the
default branch when left out), or by `change`, which reports for the change's
head and target. An ending sent again is answered from the row, so a CI that
retries a lost answer records nothing twice; a run after an ended one is the
check's next attempt. One check of a commit is one runner's or one CI's, never
both: a name the engine has queued at that commit for that branch is refused
`native_check`, so the engine's result is never overwritten, and a name your CI
reported first is left to it (the engine queues nothing of a name the commit
already holds an attempt of). So name your CI's checks apart from the ones
`.crewlet/checks.yaml` defines. During a rolling upgrade a data node of an
earlier build refuses `tests`, `log_lines` and `tail_cut`; send the report again
without them.

## Finding code

A code search reads lines, not files: an expression matched one line at a time
with Go's regular expressions, over a commit of each repository searched (its
default branch's tip unless the search names another). It is written as the
knowledge base's search is, filters first:

```text
repo:billing repo:ledger path:src/**/*.go ref:main  retryBudget
```

- `repo:<name>` searches that repository (repeat for more); with none, a search
  covers every repository you may read that is searched by index.
- `path:<pattern>` keeps the files a `.gitignore`-style pattern matches.
- `ref:<ref>` searches a branch, a whole ref or a commit's id.
- The text after them is searched as written; a search may instead be a regular
  expression or a whole word, and may ignore case. It needs one run of at least
  three characters to look up, matches within a line, and takes at most 400
  bytes (the knowledge search's bound).

**Indexed or grep, per repository.** A repository declared in the company file
is searched by index unless its entry says `search: grep`; one a seat's tool
makes, and every scratch repository, by grep. An indexed repository's default
branch is kept as a trigram index on two of your Gitlet nodes, so a company-wide
search reads only files that can match. A grep repository is read with git grep
whenever searched, costing a large repository a second or two of a Gitlet node's
CPU, so a search reaches it only when a `repo:` filter names it, at most eight
at once.

**A search says what it did not search.** Several repositories are searched in
one question to every Gitlet node at once, each repository answered by the node
first in its order and, where that one does not answer, once more by the next.
Every answer carries a coverage block naming each repository it could not search
and why (a node not answering, an index still being built, a node behind, a
grant its chart refused, a ref the repository does not hold) and the grep
repositories a search with no `repo:` left out. An empty answer with an empty
coverage means nothing matched; with a coverage naming repositories, those were
not looked at: search them again by name, or ask after the node named. At most
500 lines come back, in the order of repository, path and line, and `truncated`
says more may have matched; repositories named after the last line, none of
whose lines could come back, are listed as past the limit (a common word reads
the first repositories that fill the answer, not every one), so narrow with
`repo:` or `path:` to reach them. See [Code
search](../concepts/gitlet.md#code-search).

**Searching from the dashboard.** **Gitlet › Search** (`#/gitlet/search`) takes
the search as written above, with mode, repository, path and ref as controls,
and draws each file's matching lines in context, by repository and file, each
line opening its file at the commit searched; facets narrow by repository or
directory, and **Show N more** reads the rest of a file's matches. What was not
searched is said above them. A repository searched only with git grep is named
with `repo:` to search it, as the line above the results says. In ⌘K, the
**Code** tab searches the same way and lists the first eight lines.

**Searching from a script.** The same search is the question `code_search`, on
[`GET /gitlet/search`](../reference/api-endpoints.md#get-gitletsearch): `?q=` is
the search as written above, with `mode`, `ignore_case` and one `repo`, `path`
and `ref` beside it where a script would rather not write them into the text.
Its answer names the commit each repository was searched at, so read a hit's
file at that commit, not at the branch, which may have moved.

**A seat searches with a tool.** The same search is `code_search`, a seat's and
your own assistant's ([The company's
code](tools-and-mcp.md#the-companys-code)): `query` in the grammar above, with
the same `mode`, `ignore_case`, `repo`, `path` and `ref` beside it. Each line
arrives fenced as a file's text nobody vouches for, a page at a time, each page
under one tool answer. A page asks the Gitlet nodes for at most 315 lines, the
most one answer holds, starting after the last line of the page before (at the
commit that line was read at), so a page costs the lines it reads from its place
on, never the whole search again, and paging reaches every matching line.

## Seats and code

Nothing to set up: once the company hosts code, your seats work in its
repositories through thirty-eight of the [code
tools](tools-and-mcp.md#the-companys-code), in their own names: every one but
`code_report_check`, `code_purge_unreachable` and `code_import_repo`, each a
person's. A seat holds no git credential and needs none. Every seat reads with
thirteen and comments on a change; a `review` seat also asks for reviews,
reviews and dismisses the findings it raised; a seat whose `role.code.access` is
`full_access` changes repositories with the other twenty-six; its executor
checks a repository out into its box with `code_checkout` and works in that clone
besides. What each may do is
its grant, derived from the chart ([Who may read and
push](../concepts/gitlet.md#who-may-read-and-push)).

**For whom.** Every agent seat holds the Gitlet permission, as it holds every
app, and in a turn nobody woke it works on the code by its grant alone. In a turn
a person woke (their message, comment, or an assigned work item) it reads and
changes code only where that person holds the Gitlet permission too: for a
person without it, every code tool is refused, its executor is given no
repository, and the seat tells them an administrator can give them Gitlet. Not
even the seat's own branches are changed for them. A colleague seat's turn is
held to that colleague's grant as well; a turn woken by a change, review,
comment or push a colleague made while it worked for somebody, or by what
Gitlet records later about that push or about a change the colleague queued, is
held to that somebody instead, as on Worklet, Doclet and Chatlet ([Reads made
for somebody else](../concepts/gitlet.md#reads-made-for-somebody-else), [Writes
made for somebody
else](../concepts/gitlet.md#writes-made-for-somebody-else)).

### What your seats read

Every seat reads the repositories with thirteen of the code tools, with no clone
and no token: a file's lines, a directory, what changed between two commits, how
two commits relate, whether a fix reached a branch and which push put it there,
a path's history and who last changed each line of a file, a repository's
changes (what each changes, its findings placed in the revision it reads, what
changed since an earlier revision), and what a check of a commit said. It finds
where something is with `code_search`, the [search](#finding-code) a person
makes, across the repositories it reads that keep a search index or those its
`repo:` filters name; its prompt tells it to search before reading whole trees.

What each seat may read is its grant, from the chart: its
[`role.code.access`](../getting-started/configuration.md#role-fields-summary)
caps it, and a `visibility: unit` repository is read only by the owning unit's
seats and the leads above them ([Who may read and
push](../concepts/gitlet.md#who-may-read-and-push)), so a seat outside the unit
finds it absent from its listing and every read refused. Your own assistant
reads through the same tools on
[`/operator/mcp`](../reference/api-endpoints.md#operatormcp--your-own-assistant),
as you, so it reads every repository while you hold the Gitlet permission, and
none without it; asked by you, `code_get_repo` also hands out the URL to clone
from, on `api.external_url`.

### What your seats change

A seat at `full_access` changes the repositories with the other twenty-six code
tools, one change a call: a commit of up to 1 000 file operations to a branch
(`code_commit_files`), a commit reverted or picked onto one (`code_revert`,
`code_cherry_pick`), a branch created or deleted (`code_create_branch`,
`code_delete_branch`), a tag created or deleted (`code_create_tag`,
`code_delete_tag`), a repository created, changed, renamed, archived, deleted,
restored or protected ([below](#scratch-repositories)), and a change opened,
edited, closed, reviewed and discussed ([Or with a
tool](#open-a-change-from-a-push)). A `read_only` seat (what a seat that sets
none runs at) is offered one of the twenty-six, `code_comment_on_change`, and a
`review` seat also `code_request_review`, `code_review_change` and
`code_resolve_finding`, to dismiss the findings it raised: none changes a
repository. Your assistant has the same twenty-six and `code_report_check`,
`code_purge_unreachable` and `code_import_repo` besides, each a person's alone,
as you.

**A commit is built on the server and lands as one push** in the seat's name
([Engine-built commits](../concepts/gitlet.md#engine-built-commits)). The Gitlet
node holding the repository builds it over exactly the directories it touches,
never a checkout (a commit to a repository of a hundred thousand files costs
what it changes), checks its objects as it checks a `git push`, and lands it
through the committer, protections and limits a person's push meets. A call
names the head it read (`expected_head`), so a branch that moved meanwhile is
refused `stale` rather than overwritten: the seat reads again and decides. A
call asked again (retried after a timeout, or made on another Gitlet node when
the first went silent) is the same push carrying the same commit, landed once.

**Who a commit names.** Its author is the seat: its role's `name`, and the
`email` the role states or, where it states none a commit can name (a `${VAR}`
reference is never written into history), `<handle>@` Gitlet's identity domain.
Set [`code.identity_domain`](../getting-started/configuration.md#code) to a
domain of yours so those addresses read as your company's in every clone; unset,
they end `@seats.invalid`, a name nobody can own:

```yaml
code:
  backend: gitlet
  identity_domain: example.com   # docs-writer, stating no email, commits as docs-writer@example.com
```

An administrator changes it on **Settings › Gitlet**, under **Scratch
repositories and commit identity**, where its row shows the domain in force
beside the default; the engine checks a typed domain before it is stored.

Its committer is the engine, `Crewlet <engine@example.com>`. That marks the
commits Gitlet built in your history and proves nothing, as any git client can
write the same committer: who made a commit is its push's record, never the
commit ([Reading a seat's pushes](#reading-a-seats-pushes)). Nothing is added to
the message.

**Where a seat may commit** is its grant: a seat in the owning unit writes the
repository's branches, its unit's lead also forces and deletes them, and every
`full_access` seat owns `seats/<its handle>/` on every repository it reads,
where a seat outside the unit puts its work in place of a fork. Beyond its
grant, a seat's push is held to what a person's never is ([A seat's
push](../concepts/gitlet.md#a-seats-push)):

- **A protected branch takes no seat's push**, its unit's lead's included
  (`protected_direct`): `direct_push: persons`, what a rule says unless it says
  `none`, is a person's push alone. A seat's work reaches `main` when a person
  pushes it: the seat commits to a branch of its own and opens a change of it
  for you to read.
- **A seat outside the owning unit fills at most half** of the repository's refs
  and half its bytes, so the owners keep room to push (`repo_refs`,
  `repo_size`).
- **A seat makes at most `code.limits.pushes_per_seat_day` pushes a day**, a
  push a rule refused whole among them ([The daily quota](#the-daily-quota),
  `push_quota`).
- **A data node behind the company's configuration decides no seat's push**: the
  revision it failed to apply may be the one that took the seat's grant away, so
  the repository's next committer decides it.

What each refusal means is in [What a write is
refused](../concepts/gitlet.md#what-a-write-is-refused); the seat's tool answers
with the same code and sentence.

### Scratch repositories

A seat that wants somewhere to try something makes a **scratch repository** of
its own with `code_create_repo`, named `<handle>.<name>`
(`docs-writer.parser-spike`) and owned by it. Only that seat creates one, at
`full_access`. It takes no protection rule, is searched with `grep` and never
indexed, and the seat changes, renames, archives, deletes and restores it as any
repository it maintains. Two settings bound them:

```yaml
code:
  scratch:
    max_per_seat: 50   # 0..1000; 0 lets no seat make one. Unset is 50
    idle_days: 7       # 1..365; untouched this long, one is deleted. Unset is 7; 0 is refused
```

- **`max_per_seat`** counts **every** scratch repository a seat owns until it is
  purged, a deleted one included, so creating and deleting in a loop holds no
  more; past it a create is refused `scratch_limit`.
- **`idle_days`**: one with no push, rename, settings change or restore for that
  long is **deleted** by the `code-retention` duty, and restorable for
  `code.deleted_retention_days` before it is purged. Reading is no activity; an
  archived one is kept; one a live turn has named is never taken from under it,
  however long the turn waits ([A scratch repository's idle
  expiry](../concepts/gitlet.md#a-scratch-repositorys-idle-expiry)). The duty's
  `code_retention_pass` line counts the ones it `expired` and the idle ones a
  turn kept (`scratch_held`).

An administrator changes both on **Settings › Gitlet**, under **Scratch
repositories and commit identity**: each row's pencil writes its key, held to
its range as typed and checked against the whole company before it is stored.
The identity domain beside them is changed the same way.

You never create a scratch repository (it is a seat's) and you maintain every
one: archive one to keep it, or restore a deleted one before its purge with
`code_restore_repo`, from your assistant.

A unit's effective lead at `full_access` may also create a **project**
repository for its unit with the same tool: one label, like a declared one, and
counted with the declared ones against the 10 000 Gitlet holds (`repo_limit`).
On a repository it maintains a seat may add or tighten a protection with
`code_protect_branch`, never remove or loosen one: that is yours ([Who may
change a protection](../concepts/gitlet.md#who-may-change-a-protection)). A
repository `code.repos` declares refuses every tool's request
(`config_managed`): change its declaration instead. Everything a tool may do to
a repository is in [Repositories made by
tools](../concepts/gitlet.md#repositories-made-by-tools).
### Renaming a repository

A repository a tool made is renamed with `code_update_repo`'s `rename_to`, by
its maintaining seat, or by you through your assistant or with **Rename** on
its **Settings** tab. The new name keeps its kind's form: a scratch repository
keeps its seat's handle (`docs-writer.parser`), a project stays one label.
**The old name redirects for 30 days**: clone and fetch of the old URL still
work (git prints `warning: redirecting to` the new one), a push to it is
refused naming the new name and when the old one stops serving. Point every
clone at the new name:

```sh
git remote set-url origin https://crewlet.example.com/git/docs-writer.parser.git
```

Nothing else takes the old name meanwhile, renaming back takes it back, and
after 30 days it is free. A **declared** repository is not renamed this way: its
name in `code.repos` is its address, so a new name deletes it and creates
another ([Declare repositories](#declare-repositories)).

### The daily quota

A seat makes at most `code.limits.pushes_per_seat_day` pushes a day on the
company's [calendar](../getting-started/configuration.md#the-companys-clock),
all repositories together, counting every push it lands (by tools or from its
coding box) and every push a rule refused whole, as an attempt. Default 1 000,
about thirty-three times a reference-company seat's thirty. It bounds a seat
pushing in a loop; your pushes are never counted:

```yaml
code:
  limits:
    pushes_per_seat_day: 1000   # 1..100000; one seat's pushes a company day
```

Past it, a push is refused whole as `push_quota`, naming when the count
restarts (the company's next midnight), and **nothing records it**: recording
each refusal of a runaway seat would cost the code log what the quota exists to
bound, so it is not in the push history. An attempt past the day is told its own
rule, not `push_quota`, and is not recorded either. A seat that meets it is
looping: read what its turns do before raising the bound. The pack it built is
stored, named by nothing, and deleted by the object collector a day later.

### Reading a seat's pushes

Every push a seat lands names the seat and **the turn that made it**: its turn,
work item and model, stamped by the engine from the call, never read from the
commit. `crewlet gitlet status -repo` prints the turn and work item in `BY`,
and for a coding box's push its launch and harness
([`crewlet gitlet status`](../reference/cli.md#crewlet-gitlet-status)):

```text
PUSHED AT             BY                                                                NODE    REFS  RESULT
2026-10-12 09:31 UTC  docs-writer (turn 0192f6a1-…, native ENG-12)                      data-a  1     ok refs/heads/seats/docs-writer/eng-12 4c1d2e3..8a7b6c5
2026-10-12 09:02 UTC  swe (claude-code run 7f3e9c2a-…, turn 0192f69c-…, native ENG-42)  data-b  1     ok refs/heads/seats/swe/eng-42 0000000..1f2e3d4
```

`GET /gitlet/repos/{name}/pushes` answers each with `actor_kind` `agent`, the
seat's handle as `actor`, how the pack was built (`via`: `engine` for a commit
its tools built, `git` for its box's push) and the run under `run`: `turn_id`,
`work_backend` and `work_key`, `model`, and for a box's push `run_id` and
`harness` ([API
Endpoints](../reference/api-endpoints.md#gitlet-git-and-gitlet)). From a commit
to who landed it, `code_landed_in`, `code_log` and `code_blame` name the push
behind each commit ([Which push landed a
commit](../concepts/gitlet.md#which-push-landed-a-commit)). A repository's
**Commits** tab narrows history to one seat's pushes and recent days and
searches commit messages; each commit shows its checks and the change it
landed with.

**Gitlet › Pushes** (`#/gitlet/pushes?repo=web`) reads pushes as sentences by
day, each saying how many are shown; narrow by who pushed and when; its rail
shows who pushes there and the branches' protection.

### Working in a clone

A seat's executor runs in a [box](../concepts/code-sandbox.md) and works in a
clone as a git user of Gitlet, with a credential of its run's own, not yours or
any token's. The engine clones each repository the run is given into the box
before its harness starts
([Repositories on Gitlet](../concepts/code-sandbox.md#repositories-on-gitlet)):

- **on the seat's own branch** for its turn's work item,
  `seats/<handle>/<the item's key>`, unless the run names another, so runs on
  one item continue each other. A new key starts a new branch;
- **from that branch's head**, or the default branch's if it does not exist
  yet, unless the run names a `base`;
- **to push, where the seat may**, else to read. The run's credential pushes
  only what the run was given and its seat may push, and ends with the run.

The box reaches Gitlet nodes at `/box/git/`, through the address every node
that runs seats is given in `CREWLET_BOX_GIT_URL`
([Deployment](deployment.md#exposing-webhooks-without-the-admin-api)); every
request is admitted by its run's record, never by its token alone
([Coding boxes](../concepts/gitlet.md#coding-boxes)). Its commits carry the
`Crewlet-Seat`, `Crewlet-Run` and `Crewlet-Work-Item` trailers of [commit
attribution](../concepts/code-sandbox.md#commit-attribution) unless
`providers.sandbox.commit_attribution` is `false`: a convention a box can drop
or forge and Gitlet does not read; the push's record says who pushed
([Commit trailers](../concepts/code-sandbox.md#commit-trailers)). Its pushes
are held to the seat's rules above, and when the run ends the seat is told what
it pushed from Gitlet's own record, not the run's word: each branch moved, from
where to where, and the change it heads
([What a run pushed is reported for it](../concepts/code-sandbox.md#what-a-run-pushed-is-reported-for-it)).
`change: web!42` puts a run on that change's branch, from its head;
`open_change` opens a change from the run's branch once the run has succeeded,
once, carrying the seat's work item
([A run's changes](../concepts/code-sandbox.md#a-runs-changes)).

### A seat's idle branches

Nobody else deletes a seat's `seats/<handle>/` branches, so Gitlet does: a
branch **a seat set in its own namespace and nobody has set for 14 days** is
deleted by the `code-retention` duty, as a push on the engine's own account
([The ref lifecycle](../concepts/gitlet.md#the-ref-lifecycle)). Never deleted:
a branch a person pushed last, the default branch, a live change's source or
target, a protected branch, and a branch a seat's work still depends on (one a
live turn read or wrote with a code tool, one an executor's launch gave its
box, or one its box pushed) until that turn, or the run it waits on, ends,
however long it takes. A turn reading a branch idle nearly 14 days records its
hold on the repository, ordering the read against a deletion under way: if the
deletion lands first, the call is refused, naming the commit the branch was at,
and nothing about the branch is sent.

**A deleted branch is restored** with `code_create_branch` at the commit it was
at, by the seat or by you through your assistant. The deletion's row in the
push history (`GET /gitlet/repos/{name}/pushes`, `crewlet gitlet status
-repo` — names every branch it deleted beside that commit (`via` `expiry`,
`actor_kind` `system`) for the 181 days the history keeps it. The row gives the
commit; the repository keeps it only for the deletion's **14-day** prune grace
([ref drops](../concepts/gitlet.md#retention-and-history)), and not even that
once a person purges its unreachable objects. Restore a branch you need within
two weeks of its deletion: after that, it comes back only until the next
[expunge](../concepts/gitlet.md#expunge) drops the commit. A repository setting
refs faster than they go idle raises `code_refs_near_limit` before the 100 000
live refs it may hold ([alarms](../reference/alarms.md)).

On a repository's **Branches** tab, Stale lists branches nobody has moved in 90
days, each with who moved it last, while the push history still names them.

## Who read a unit's repository

Every clone, fetch and download that sends a `visibility: unit` repository's
contents, and every server-side read of one, is recorded, whoever made it. An
admin asks who read one, from a script with a token that carries `admin`, or
with `code_repo_readers`, a page at a time:

```sh
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/gitlet/repos/payments/readers"
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/gitlet/repos/payments/readers?from=2026-07-01&to=2026-09-30"
```

Each reader (a credential and the person it acted as, or a seat) comes with how
it read (`git` or `tool`; a download is `git`), how often, bytes sent and last
read, summed over every node and day of the range: the thirty company days
ending today unless you name `from` and `to`, at most 181. Hand a page's `next`
back as `?cursor=`. A `visibility: company` repository's readers are every seat
and are not recorded; the answer says so. What counts as a read, retention and
why only an admin may ask: [Who read it](../concepts/gitlet.md#who-read-it).

## Moving a repository in

A repository at another forge comes in three ways, each a person's to start:
**imported once** by its home Gitlet node, which then holds it like any other;
imported and kept in step as a **pull mirror**, which moves only when its
source does; or **pushed from your machine** into a repository that exists, for
a source only your machine reaches. The first two dial the source from the node,
over HTTPS alone and only at a public address unless Tier A says otherwise
([Reaching other forges](deployment.md#reaching-other-forges)); what each
moves, and how often: [Mirrors and
imports](../concepts/gitlet.md#mirrors-and-imports).

### Import from GitHub or GitLab

**The node's** import creates the repository and has its home node fetch the
source; with `-mirror` it stays in step for good. **Your machine's** (`-local`)
has your git clone the source and push it into a repository that exists. Run
either with your own token exported ([Give each person a
token](#give-each-person-a-token)).

#### The node imports it

An administrator first stores the source's token and declares which host it may
be sent to, in the company document:

```yaml
code:
  remote_credentials:
    GITHUB_TOKEN: { hosts: [github.com] }
```

```sh
crewlet secrets set GITHUB_TOKEN          # the source's token, where it is private
crewlet gitlet import https://github.com/acme/web.git web -owner unit:eng -credential GITHUB_TOKEN
crewlet gitlet import https://github.com/acme/api.git api -owner unit:eng -mirror   # a public source, mirrored
```

The repository is created at once, owned as `-owner` says, and reads
`importing` (also in `crewlet gitlet status -repo web`) until its home node has
landed every branch and tag, in chunks no push limit bounds, so
`code.limits.max_push_bytes` caps none of it; it takes no push meanwhile. The
home dials only a public address unless Tier A's `gitlet.import_allow_private`
lets it reach your network.

`-credential` names the variable holding the source's token, never the token
(settings would carry it on every data node and in every backup); the node
resolves it from the secret store, then its environment, at each dial. It must
be declared for the source's host in
[`code.remote_credentials`](../getting-started/configuration.md#coderemote_credentials),
or the import is refused `not_permitted`, naming the host and the variables
declared for it: only an administrator decides which token may leave for which
forge.

A blob past `code.limits.max_blob_bytes` or a history past
`code.limits.repo_max_bytes` ends the import early: the repository turns
`active` holding what landed and is not retried. Either way the event log keeps
one `code_imported`, naming the source, what landed and, if early, why.

A **pull mirror** (`-mirror`) then moves only by its home's syncs, so every push
to it is refused. So is a repository that exists already when made a pull
mirror (its `code.repos` entry's `mirror_from`, or its Settings tab) or moved
to another source: it reads `importing` until the source has landed; every
branch and tag it holds but a seat's own is made the source's, and one the
source lacks is deleted.

`code_import_repo` does the same, as does **Import a repository** on the
dashboard's Gitlet › Repositories, where the About rail shows progress; no seat
is offered it. Flags:
[`crewlet gitlet import`](../reference/cli.md#crewlet-gitlet-import).

#### Your machine pushes it

Create or declare the repository first (`-local` creates none), then:

```sh
crewlet gitlet import https://github.com/acme/web.git web -local
crewlet gitlet import git@gitlab.com:acme/web.git web -local   # SSH works as well
```

**Your git does the work, with your access**: it clones with your credential
helper, SSH key and proxy and pushes to Gitlet as you, so the engine reaches no
other host and the push history names you. Where the source needs a password
no helper holds, or ssh a host key or passphrase, git asks on the terminal.
Your token must act as your seat ([Give each person a
token](#give-each-person-a-token)): a token bound to nobody fetches only, and
the import says so before downloading. The clone goes in the system's
temporary directory, so point `TMPDIR` at a volume with room for it and two
steps' packs: up to three times `code.limits.max_push_bytes`.

**Takes and skips.** Every branch and tag. A forge's own refs (GitHub's
`refs/pull/*`, GitLab's `refs/merge-requests/*`) are listed as skipped, and
history only they reach is never downloaded. Git LFS files stay on their LFS
server; the pointer files come along.

**What it refuses**, before pushing anything where the source shows it (all
findings at once), else before the step that meets it:

| The import says | What to do |
|---|---|
| `blob <id> (<path>) is 812.0 MiB, past code.limits.max_blob_bytes (50.0 MiB)` | Large binaries belong in Worklet's project files. Rewrite the history without them (`git filter-repo --strip-blobs-bigger-than 50M` on a copy) and import that, or raise `code.limits.max_blob_bytes`. |
| `the source's history packs to … as one pack, past code.limits.repo_max_bytes` | Raise `code.limits.repo_max_bytes` (with room for the steps, below) or import a smaller history. |
| `web would hold … of packs once the next step lands … past code.limits.repo_max_bytes` | The steps store more than the source's own pack (below). Raise `code.limits.repo_max_bytes` and rerun: it resumes. |
| `commit … alone packs to …: it brings in the history of …, and no other branch of the source can carry it` | A merge of a history larger than one step that no other branch reaches. Mirror the source (`git clone --mirror`), give the named commit a branch there (`git branch`), import from the mirror (that history is stepped on the branch first), then delete the branch if unwanted. |
| `the source is a shallow clone` | Import from a full clone. |
| `1 branch Gitlet keeps for its seats' own branches, which live under refs/heads/seats/<handle>/ — refs/heads/seats, and refs/heads/seats/<name> as a branch itself, would block them, since no ref is created beside a directory of its name: "refs/heads/seats/ana" — rename them in the source` | Rename the branch in the source (`git branch -m seats/ana seats-ana` in a clone you import from, or on the forge) and rerun; nothing was imported. A branch inside a namespace, `seats/ana/x`, is ordinary and kept. |
| `refs/heads/main is at … on web and … in the source — this is not a resumed import of the same history` | Something else was pushed to the repository: import into an empty one, or delete the ref there first. |

**Large histories go in steps.** One push carries at most
`code.limits.max_push_bytes` (1 GiB at most), so the default branch is pushed
along its history in steps that each fit (each a fast-forward of the branch),
then the other branches and tags in batches. Each step is measured before it is
sent, so none is refused for size; a single commit or tag past the limit alone
is refused naming it, and a merge bringing in more than one step is stepped
from the other side first. `crewlet gitlet import … -local -dry-run` prints the
plan and pushes nothing.

**A stepped import stores more than the source.** Each step keeps whole the
files it touches whose earlier versions an earlier step stored (34 MB as one
pack, about twice that in eleven steps, on this project's own history). The
object store keeps the steps' packs as pushed, and every Gitlet node serving
the repository caches them, until its home merges them ([A busy repository's
packs are merged](#a-busy-repositorys-packs-are-merged)); a merge may delta
them again, by an amount that depends on the history. So size
`code.limits.repo_max_bytes`, the object store and node caches for the
repository as pushed, not the source's pack, and run a large import with
`code.limits.max_push_bytes` at its 1 GiB ceiling, the fewest steps.

**Resuming.** A failed step (network drop, node restart, a rule refusing it)
stops the import with git's reason; every step before it has landed whole. Run
**the same command again**: it reads what the repository holds and pushes the
rest. A branch protected with `direct_push: none` refuses its own import, as it
refuses every push: lift the rule for the import and restore it after.

Every step, flag and message:
[`crewlet gitlet import -local`](../reference/cli.md#crewlet-gitlet-import--local).

## Mirroring to another forge

A repository kept here can be published to another forge (GitHub, GitLab, one
on your network) as a **push mirror**: its home Gitlet node pushes there within
a minute of a covered ref moving, and reads it back every 15 minutes, restoring
what anybody else changed. Declare it on the repository:

```yaml
code:
  backend: gitlet
  remote_credentials:
    GITHUB_MIRROR_TOKEN: {hosts: [github.com]}  # the one host its value may be sent to
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
protects is pushed at its **green** commit (the newest landed on it whose
required checks passed), so a commit that has not passed here never appears
there, and a branch with no green commit yet is left as the other forge has it.
Other covered branches and tags are pushed as they are. Unset, `refs` covers
the default branch and every tag; seats' branches (`refs/heads/seats/…`) are
never covered. The other forge's copy is the repository's: the mirror
force-pushes and deletes a covered branch the repository no longer has, so use
a repository nobody pushes to and allow force-pushes on its protected
branches. Commit messages are kept, a coding box's `Crewlet-Seat`,
`Crewlet-Run` and `Crewlet-Work-Item` trailers with them.

**It pushes as the seat `as` names, never as a bot**, with that seat's own token
there: what appears is that person's or agent's push. The seat must read the
repository (a person's seat, such as `jane`, only while that person has the
Gitlet permission) and is checked before every push, so a chart change removing
its read, or Gitlet taken from the person, stops the mirror. The token is never
in the company file: `credential` names the variable, resolved from the secret
store, then the node's environment, at each dial, and only where
`code.remote_credentials` declares it for the mirror's host (above). A forge
wanting a user name with its token gets it in the variable, as `user:token`.

**When it cannot push**, its home logs `code_mirror_attempt_failed` with the
reason and retries a minute later, longer after each failure in a row; a mirror
not level for half an hour raises [`code_mirror_failing`](../reference/alarms.md)
on its home, cleared by its next push. Every push that moved something is in
the event log as `code_mirror_pushed`, naming what it set, deleted and held
back. Rules: [Mirrors and imports](../concepts/gitlet.md#mirrors-and-imports).

## Status and verify

**`crewlet gitlet status`** lists what is hosted (status, owner, default branch
and head, refs, packs, size, last push); `-repo web` details one with its
newest pushes: who made each and what became of every ref, `ng` with git's
reason where refused. Run it beside a node's Tier A file, or anywhere with
`-api` and the token in `CREWLET_API_TOKEN`, needing no Tier A file; it reads
with the same token git uses, as the principal it acts as:

```sh
crewlet gitlet status
crewlet gitlet status -repo web -pushes 20
```

**`crewlet gitlet verify`** checks a Gitlet node's **cache** (its disposable
copy of each repository) against the repositories' rows: every pack's bytes,
the refs, and git's connectivity check. A damaged copy is one node's, but it is
what that node serves. Run it:

- **on a schedule on each Gitlet node** (`-api` names the node): it exits
  non-zero when any copy is not clean, the hook for cron. A repository the node
  holds no copy of is passed over;
- **when git errors about a repository on one node and not the others**, with
  `-repo` naming it;
- and with **`-repair`** to repair what it found, then recheck: a copy with only
  its own directory out of place (a file nothing in the engine writes, an
  edited config, a stray pack) is fixed in place, packs and indexes kept,
  nothing downloaded; a damaged one is rebuilt, packs found whole kept and the
  rest downloaded again.

```sh
crewlet gitlet verify -api https://git-1.internal.example.com:8080
crewlet gitlet verify -api https://git-1.internal.example.com:8080 -repo web -repair
```

Verify is a deployment control, like a backup: the token must carry
**`admin`**, else it is refused naming it before anything is checked. The check
runs on the node, reading every byte of the copy (about nine seconds a
gigabyte); the command waits for it, past any proxy timeout, by the deadline
the node states from what the copy holds. A sweep checks every repository at
the position it listed them at, costing the code log one barrier. Flags and
lines: [`crewlet gitlet verify`](../reference/cli.md#crewlet-gitlet-verify).

Both read the API's `/gitlet/` routes, which a script reads too with the same
token; `/gitlet/` is always guarded:

```sh
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/gitlet/repos"
curl -s -H "Authorization: Bearer $CREWLET_TOKEN" \
  "https://crewlet.example.com/gitlet/repos/web/pushes?limit=10"
```

A listing is read from the answering node's copy by default; a script that must
not conclude a repository is missing from an old copy asks
`read_level=linearizable`. Routes, parameters and answers: [API
Endpoints](../reference/api-endpoints.md#gitlet-git-and-gitlet).

## Export

**`crewlet gitlet export`** writes one repository (every branch, tag and object
behind them) to a single [git bundle](https://git-scm.com/docs/git-bundle) on
your machine, through your git (2.40 or newer) and the same token:

```sh
crewlet gitlet export web web.bundle
git clone web.bundle web          # anywhere, with no engine at all
```

It is how a repository leaves the engine (archive, audit, another host), not a
backup of Gitlet: a [backup](backup.md) of a data node holds every repository's
rows and packs and restores Gitlet itself. It needs room for the repository
twice beside the bundle, since it clones a mirror and bundles it; see
[`crewlet gitlet export`](../reference/cli.md#crewlet-gitlet-export).

## A busy repository's packs are merged

Every push carrying objects stores one pack. The repository's
[maintainer](../concepts/gitlet.md#who-maintains-a-repository) (the Gitlet node
that also publishes its bitmapped index; its home on a fleet of alike nodes)
merges small packs into a few with no step of yours: it looks every minute and
merges once sixteen have gathered, or a busy repository's minute of pushes at
every look ([Compaction](../concepts/gitlet.md#compaction)). Replaced packs are
kept a day for clones still reading them, then deleted. A repository taking
pushes faster than they merge can reach the 2 048 live packs one may hold: a
push carrying objects is then refused `compaction_behind`, while one only
moving or deleting refs still lands; rerun it a minute later. The maintainer
raises the `code_compaction_behind` alarm while any repository it maintains
stands there, and its log names what blocks the merge: a node whose memory
cannot hold it (`code_cache_compaction_refused`; more memory, or fewer git
transfers, holds it), an object store with no room (`code_compaction_refused`,
`store_full`), or a failed merge (`code_compaction_failed`).

## Delete and restore a repository

Remove its entry from `code.repos` and apply: the repository is **deleted** (no
push, not served) and its refs and packs are kept for
`code.deleted_retention_days` (unset is 7). **Declaring it again under the same
name within that window restores it** as it was, with the settings you declare
now. Past the window it is purged, in bounded steps, and its name is free. A
purge cannot be undone, hence the window; see [Deletion and
purge](../concepts/gitlet.md#deletion-and-purge).

A repository the document does not declare (made with **New repository** on the
dashboard's Gitlet › Repositories, by an import or by a tool) is deleted with
`code_delete_repo` and restored with `code_restore_repo`, by whoever maintains
it: a seat, your assistant, or the dashboard, where **Delete repository** on its
**Settings** tab asks you to type its name and takes a reason, and a deleted
repository (the **Deleted** filter) offers **Restore repository**. The same
window and purge apply.

## A leaked secret is rotated, then purged

A force-push moves a branch; it does not remove a commit's objects. What no
branch or tag reaches stays in the packs, fetchable by id, until an
[expunge](../concepts/gitlet.md#expunge) drops it: on the maintainer
every week once a quarter of the repository, and at least 64 MiB, is
unreachable, keeping what a force-push or a deletion left for
two weeks. A credential ever pushed is in every clone that fetched it, so:

1. **Rotate it** at whatever issued it. This is what makes you safe; the rest is
   tidiness.
2. **Take it out of the history you keep**: rewrite the branches and tags that
   reach it (`git filter-repo`; a revert keeps the commit) and force-push. A
   protected branch takes the push only where its rule's `direct_push` lets a
   person push and its `allow_force` lets it be rewritten, so loosen the rule
   and tighten it again ([Protecting a branch](#protecting-a-branch)). An
   expunge also keeps what work in flight names: every revision of a draft or
   open change (close a change that carried the secret and open a new one from
   the rewritten branch), a queued change's head (take it out of its queue), and
   a protected branch's last green commit until a newer commit's checks pass.
3. **Purge the repository's unreachable objects**, so the commit goes now
   rather than two weeks on:

   ```sh
   crewlet gitlet purge-unreachable web
   ```

   or `code_purge_unreachable` from your assistant ([`crewlet gitlet
   purge-unreachable`](../reference/cli.md#crewlet-gitlet-purge-unreachable)),
   or **Purge unreachable history** on the repository's Settings tab, which
   names who still keeps copies before you confirm. It is a person's alone, on
   your own token, with no grace: every unreachable object goes, however
   recently left. The answer comes once the request is recorded;
   `crewlet gitlet status -repo web` shows it until the maintainer has served
   it, then when it committed and what it dropped.

The answer also lists what still holds the secret: each Gitlet node's cache
until it next syncs, the object store for a day, every backup taken before the
purge (on the `nats` store, until the store deletes it; [A purge and the
backups before it](backup.md#a-purge-and-the-backups-before-it)) and, on a
versioned S3 bucket, the old version its rules keep. Nothing on the server
reaches a clone that already fetched it, so rotation comes first ([What still
holds a purged object](../concepts/gitlet.md#expunge)).

## See also

- [Gitlet](../concepts/gitlet.md) — what a repository is, its changes
  and their reviews, the committer, the cache on a Gitlet node, and a push end to
  end
- [Deployment: Gitlet](deployment.md#gitlet) — git on the host,
  the cache's volume, the balancer in front of `/git/`
- [Running a Fleet](fleet.md) — which nodes to give the `gitlet` role
- [Scaling Out: Gitlet nodes](../concepts/scaling.md#gitlet-nodes) — what Gitlet nodes
  and data nodes each scale, and a Gitlet node's memory, cores and disk
- [Read Consistency](consistency.md) — what level a git answer and a `/gitlet/`
  read are given
