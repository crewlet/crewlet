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
user name is ignored. To be asked once, give git a credential helper — your
platform's keychain is the safe one (`git config --global credential.helper
osxkeychain`, `libsecret` or `manager`); `credential.helper store` works too,
and keeps the token in plain text in `~/.git-credentials`.

**In CI**, hand the token over without a prompt. Either as a bearer header,
which every route accepts:

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

## See what is hosted

The repositories and their history are read over the API with the same token
— `/code/` is always guarded:

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
