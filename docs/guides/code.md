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
  the code host and starts serving `/git/`. A git node needs **git 2.40 or
  newer** on its host (the engine's image ships 2.47) and a place for its cache,
  sized by its Tier A [`git` block](../concepts/configuration.md#git); the
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
  built: the cache directory is another program's, git is older than 2.40 —
  answers git and `/code/` with a `503` naming `config_apply_failed`, never that
  the company hosts no code; `GET /fleet` and the node's log say why, and a
  node still applying it answers `503` with `Retry-After: 5`. A node whose
  apply failed before it could even read the revision — it could not fetch it
  from the coordination store, or open it with its key — answers the same
  `503 config_apply_failed`, saying it cannot tell whether the company hosts
  code rather than that it does.

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
      default_branch: main           # what a clone checks out; unset is main
      description: The customer-facing web app.
      protect:
        - branch: main               # nobody force-pushes or deletes main
        - branch: release/**         # every branch inside release/ ...
          allow_delete: true         # ... may be deleted, never force-pushed
        - tag: "v*"                  # a release tag never moves
    - name: legacy
      owner: { seat: founder }
      archived: true                 # read-only: takes no push
```

A name is lowercase letters, digits and hyphens, at most 63 — it is the
address, so **renaming a repository deletes it and creates another**. A rule
protects the branches or tags its pattern matches: without `allow_force` a
branch cannot be moved to a commit that does not descend from it, without
`allow_delete` it cannot be deleted, and `direct_push: none` freezes it.
`crewlet validate` warns about every repository whose default branch no rule
protects, since anybody who may push could then force-push or delete it. Every
field, the pattern grammar and what each change does at its activation are in
[Declaring repositories](../concepts/code-host.md#declaring-repositories).

## Give each person a token

A person pushes with their own Tier A API token, and the engine knows whose it
is from the org chart. Add the token to every node's Tier A:

```yaml
# crewlet.yaml (Tier A, on every node)
api:
  auth:
    tokens:
      - id: sarah
        token: ${SARAH_API_TOKEN}
```

and bind it to the person's human seat in the company document:

```yaml
roles:
  - name: Sarah Chen
    handle: sarah-chen
    kind: human
    contact:
      crewlet_operator_id: sarah
```

That binding is the same one the dashboard's writes and an assistant's
operator tools go by ([Humans in the Org
Chart](../concepts/humans-in-the-org.md)). With it, Sarah's pushes are hers:
each is recorded as one `operator_acted` event with transport `git`, naming
the token, her seat, the repository and what became of the push, and the
repository's push history (below) shows her as `actor_seat`.

**A token bound to nobody fetches only**, and so does every caller when
`api.auth.disabled` is set — such a caller is nobody, so a push with it is
refused at its first request, before anything is sent:
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

**In CI**, hand the token over without a prompt. The same helper, for one
command, through `-c`:

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
is what a blobless clone (`git clone --filter=blob:none`) needs.

Repositories use git's default SHA-1 object format; a local repository created
with `--object-format=sha256` cannot push to one.

### What git tells you, and what to do

Every refusal names its reason in the line git prints. The ones a person meets
most:

| git prints | What to do |
|---|---|
| `repository "<name>" not found` | Check the name in `code.repos`. |
| `this token is not bound to a person` | Push with your own token — the one your seat's `contact.crewlet_operator_id` names. |
| `(stale: refs/heads/main is at <id>; fetch first)` | Fetch, integrate, push again. |
| `(protected_force: …)`, `(protected_delete: …)`, `(protected_direct: …)` | The rule named says what it allows; a change to it is a change to `code.repos`. |
| `(pending: durable at <position>, …)` | The push is recorded and was not applied on the node that answered within a minute. Run the same `git push` again: if it landed, git says `Everything up-to-date`. |
| `(unknown: outcome unknown (op …); …)` | The answer was lost — it may or may not have landed. Fetch, then push again. |
| `(not_applied: …)` | The push was recorded and every node dropped it: nothing landed. Push again. |
| `… retry in 30 s …` (a `503`) | The node is draining or stopping: retry, and a balancer sends you to another. |

An `ok` from the engine means the push's own record **has been applied** — it
is never said of a push that is merely on its way. Every other condition, the
exact line git prints and what to do about it is in [What git prints, and what
it means](../concepts/code-host.md#what-git-prints-and-what-it-means).

## Status and verify

**`crewlet code status`** lists what is hosted — each repository's status,
owner, default branch and its head, refs, packs, size and last push — and
`-repo web` shows one in detail with its newest pushes: who made each and what
became of every ref, `ng` with git's reason where a push was refused. Run it
beside a node's Tier A file, or anywhere with `-url` and the token exported in
`CREWLET_API_TOKEN`, which needs no Tier A file at all; it reads with the same
token git uses, as the person it is bound to:

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
- and with **`-repair`** to rebuild a copy it found damaged: the packs it found
  whole are kept, the rest downloaded again, and the rebuilt copy checked.

```sh
crewlet code verify -url https://git-1.internal.example.com:8080
crewlet code verify -url https://git-1.internal.example.com:8080 -repo web -repair
```

The check runs on the node, not on your connection: it reads every byte of the
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
- [Read Consistency](consistency.md) — what level a git answer and a `/code/`
  read are given
