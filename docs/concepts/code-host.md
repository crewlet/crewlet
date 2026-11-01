# Code Host

A company that sets [`code.native`](../getting-started/configuration.md#code)
hosts its git repositories in the engine itself, beside its own tracker and
knowledge base, and on the same footing: what a repository *is* — its
settings, its protection rules, its refs and the packs it holds — is
replicated state every data node keeps whole, and the bytes of each push are
one object in the [object store](object-store.md) the whole fleet shares.

It is not exclusive. The [GitHub](../integrations/github.md) and
[GitLab](../integrations/gitlab.md) integrations keep working beside it, and a
company may host some repositories natively and keep others on a vendor.

---

## What it is

A repository is **rows on the code log** (`CREWLET_CODE_LOG`), the state
log's eighth domain (see [Replication](../guides/replication.md)): every change
to a repository is one record on that ordered log, arbitrated at the broker,
and every data node applies it into its own identical copy of the rows. There
is no primary repository anywhere — no node's disk holds "the" repository and
no node's loss loses one.

The **bytes are not rows.** Every push stores **one object**: its pack
together with the index and reverse index the receiving node's git wrote for
it, back to back, named by a `code_packs` row. So a node that has to bring a
repository up to date reads one object a push and indexes nothing — the
index arrived with the pack — and a backup, the collector and the audit treat
a repository's packs exactly as they treat a project's files: a required
reference, kept while a live row names it (see
[One object per upload](object-store.md#one-object-per-upload)).

A git repository on a node's disk is never the record. It is a
[cache](#the-cache-on-a-git-node) derived from the rows, disposable and rebuilt
from them.

How it scales — git nodes adding reads, data nodes committing pushes, and what
sizes each — is [Scaling Out: Git nodes](scaling.md#git-nodes); turning it on,
declaring a repository and pushing to it are the steps of the
[Hosting Code](../guides/code.md) guide.

### What it does not do yet

This is the product's current shape, not a configuration choice:

- **A seat has no git client of its own.** A person clones, fetches and
  pushes over HTTPS with their own API token ([Persons and their
  tokens](#persons-and-their-tokens)), and a seat's coding box with its run's
  own credential, in the repositories its run was given at launch ([Coding
  boxes](#coding-boxes)). A seat itself reads and changes the company's
  repositories through its code tools
  ([The company's code](../guides/tools-and-mcp.md#the-companys-code)): its
  reads are answered on the server ([Server-side reads](#server-side-reads)),
  and each change it asks — files committed, a commit reverted or picked, a
  branch created or deleted — is built on the repository's home git node and
  landed as a push ([Engine-built commits](#engine-built-commits)), under the
  rule a person's push is judged by too ([Who may read and
  push](#who-may-read-and-push)). A seat and a person's assistant create,
  rename, archive, delete, restore and protect repositories through the same
  tools, a seat's own scratch repositories among them
  ([Repositories made by tools](#repositories-made-by-tools)).
- **Nothing queues a check yet.** A repository defines its checks in
  `.crewlet/checks.yaml`, `crewlet validate` reads that file as the code host
  does, the code log has the records a check's attempts take
  ([Checks](#checks)), and every git node runs whatever attempt is queued
  ([Who runs a check](#who-runs-a-check)); nothing queues one in this release,
  and nothing gates on one, so no check runs but one a record queued.
- **Only a push opens a change, and only its home asks for its review.** A
  push opens a [change](#changes) from its options
  ([A change a push opens](#a-change-a-push-opens)); a repository's committer
  decides a change's opening, its edits and its close, a push to a change's
  branches writes its revisions, merges it or closes it, the repository's home
  records each revision's facts — its [classes](#change-classes) among them,
  asking a review of the [path owners](#path-owners) whose paths it touches —
  and whether it merges cleanly
  into its target ([Mergeability](#mergeability)) with the change's
  [readiness](#merge-readiness) judged beside it, and lands a change whose
  head its target holds ([The home's change work](#the-homes-change-work)), the
  [change writer](#reviews-and-findings) beside the committer decides who is
  asked to review it and what is said on it, and a data node answers a listing
  of changes and one change in detail ([Reading changes](#reading-changes)),
  which a seat and a person's assistant read through two code tools,
  `code_list_changes` and `code_get_change` — but no tool or route opens,
  edits or discusses a change in this release, no route reads one, and no
  change is merged in front of a protected branch. A protection
  rule decides what a push may do to a ref, and nothing else stands in front
  of one. So a rule's [merge gate](#merge-gates) is recorded and read,
  every change into its branch is judged under it ([The merge
  gate](#the-merge-gate)), a change into a branch whose gate requires a
  check is queued for its landing by its gate ([The merge
  queue](#the-merge-queue)), and a repository's committer decides a change's
  merge as one record its gate proves ([A merge, end to
  end](#a-merge-end-to-end)) — and nothing builds or asks for a merge in this
  release, so nothing merges by the gate yet, nor lands from a queue.
- **Nothing searches a repository's code yet.** Every repository names how it
  is to be searched — `search: indexed` or `grep`
  ([Declaring repositories](#declaring-repositories)) — and the mode is
  recorded, answered on every read of the repository and changed like any
  other setting; no git node keeps an index or answers a search in this
  release.
- **Nothing drops an object a push stored.** A force-push or a deletion
  leaves the history behind it in the repository's packs, fetchable by its
  id, for as long as the repository lives. The object each such push left is
  recorded for two weeks ([ref drops](#retention-and-history)): the grace
  that history keeps once a repository's unreachable objects can be dropped.

---

## Declaring repositories

The repositories are declared in the company document, under `code.repos`,
and every activation makes the fleet match the declaration:

```yaml
code:
  native: true
  deleted_retention_days: 7        # 1..365; unset is 7
  repos:
    - name: nimbus-web             # the address it is cloned by
      owner: { unit: Engineering } # a unit (id or name) or a seat (handle) — exactly one
      visibility: unit             # company (unset) or unit: which seats read it
      search: indexed              # indexed (unset) or grep: how code search reads it
      default_branch: main         # unset is main
      description: The customer-facing web app.
      protect:
        - branch: main             # refs/heads/main, and nothing below it
          approvals: one           # a merge into it waits for an approval…
          checks: [unit]           # …and for its unit check (a merge gate)
        - branch: release/**       # every branch inside release/, never release itself
          allow_delete: true
        - tag: "v*"                # a tag rule: a tag never moves
      path_owners:
        - paths: ["web/**"]        # everything inside web/
          unit: Engineering        # a unit (id or name) or a seat (handle) — exactly one
        - paths: ["**/*.sql"]      # every SQL file, at any depth
          seat: agent-dba
    - name: infra
      owner: { seat: agent-cto }
      archived: true               # read-only: takes no push
```

- **`name`** — one host-name label: lowercase letters, digits and hyphens,
  starting and ending with a letter or a digit, at most 63. It is the address a
  repository is cloned by, so **renaming one deletes it and creates another**
  rather than moving it; declared twice, it is refused at the later entry.
- **`owner`** — exactly one of `unit` (its id, or its name where it has none)
  or `seat` (a handle), resolved against the same document's chart. An owner
  the chart does not have refuses the revision, naming the units or seats it
  does have.
- **`visibility`** — which **seats** may read it: `company` (unset), every
  seat, or `unit`, only the seats the chart relates to its owner (see
  [Visibility](#visibility)). It narrows seats and nobody else: a person reads
  every repository whatever it says. Any other value refuses the revision.
- **`search`** — how code search reads it: `indexed` (unset), from an index of
  its default branch, which a search across the company's repositories reads
  as well as one naming the repository; or `grep`, with git grep over a copy,
  and only by a search that names it. An index costs each git node keeping one
  its disk and a build at every new tip, and a `grep` repository costs nothing
  until somebody asks it. A repository a tool makes starts at `grep`
  ([Repositories made by tools](#repositories-made-by-tools)). Any other value
  refuses the revision.
- **`default_branch`** — what a clone checks out; any branch name git would
  take but `seats` and `seats/<one name>`, which hold the seats' own
  namespaces (see [Who may read and push](#who-may-read-and-push)), and unset
  is `main`.
- **`description`** — one paragraph, at most 1 024 bytes of UTF-8.
- **`archived`** — read-only. A push to an archived repository is refused as a
  whole, and reading it is not; unarchiving it is setting it back to `false`.
- **`protect`** — at most 64 rules, each naming exactly one of a `branch` or a
  `tag` pattern, each pattern once; a `branch` rule may also carry a [merge
  gate](#merge-gates).
- **`path_owners`** — at most 64 owners, each exactly one of `unit` or `seat`
  resolved as `owner` is, owning 1 to 32 globs of the repository's paths, each
  glob once and each owner in one entry, and at most 256 KiB in all as the code
  log records them ([Path owners](#path-owners)).
- **`mirror_from`** `{url, credential}` — makes it a **pull mirror** of a
  repository at another host: it is imported from there when it is created,
  and then moves only by the code host's syncs from it, so it takes no push
  and no change of its own — each refused `repo_mirrored` ([What git
  prints](#what-git-prints-and-what-it-means)). Its `protect` list is warned
  about, since no push reaches a rule, and an archived one is refused: an
  archived repository takes no sync.
- **`mirror_to`** `{url, credential, as, refs}` — makes it a **push mirror**:
  the code host pushes the refs it names to a repository at another host, as
  the seat `as` names — that seat's own identity there, never a bot — and so
  `credential` is required, and the seat must read this repository: a person's
  seat reads every one, an agent's what its grant reads ([Who may read and
  push](#who-may-read-and-push)). `refs` are at most 64 patterns of
  `refs/heads/<glob>` and `refs/tags/<glob>`, each once; unset is the default
  branch and `refs/tags/*`. No pattern may reach a seat's own branches under
  `refs/heads/seats/` — `refs/heads/**` among them — since a mirror publishes
  what the company merged, never what a seat has yet to offer.
- **`import`** `{url, credential}` — fills it, when it is created, from a
  repository at another host: it is `importing` until the import is done, and
  takes no write meanwhile — no push, no change, no archive and no purge —
  each refused `repo_importing`. It is read by the create alone: one declared
  for a repository that exists already fetches nothing, which the reconcile
  says once a run (`code_import_ignored`), and beside `mirror_from` it is
  refused, since a pull mirror imports from its own source.

Each `url` is `https` with no user, query or fragment, at most 1 024 bytes, in
the one form the code host dials it by. Each `credential` is **one whole
`${VAR}` reference and nothing else**, at most 128 bytes — never the token,
which every record of the repository's settings would carry to every data node
and every backup. The code log records the reference as written, and the code
host resolves it — from the [secret store](secret-store.md), then the
environment — only when it dials ([`${VAR}`
references](configuration.md#var-references)); unset fetches anonymously.
Only a person or this document sets a mirror or an import: a seat asking to
create a repository with one, or to set or remove either mirror of one, is
refused `not_permitted` whatever its grant, since a pull mirror and an import
reach another host for the company and a push mirror sends its code to one.
A scratch repository takes none of the three. This release records each and
refuses the writes it names; **nothing fetches from or pushes to another host
yet**, so a repository created with `mirror_from` or `import` stays
`importing` until a release that runs the import.

At most 10 000 repositories one document — a typo guard two orders of
magnitude past a large company's, not a limit anybody is expected to meet. It
is an [admission rule](configuration.md#what-a-stored-revision-is-held-to), as
the retention's one-year bound is: a document written past either is refused,
and a revision already stored past one — admitted by a later build that raised
it — still applies, with a warning, because nothing the reconcile or the purge
does depends on either.

### What each change does at its activation

| You change | At its activation |
|---|---|
| Add a repository | **Created**, empty, with its settings. Two nodes creating one name land one repository: a create arbitrates on the name. |
| Change a repository's settings | **Settled**: one record carrying the new settings, stamped with the activation that wrote it. |
| Change nothing | **Nothing** but one fleet-wide record, the activation's fence (below) — never a record a repository. |
| Remove a repository | **Deleted**: it takes no push, and its rows and packs are kept. Declaring it again under its name within `code.deleted_retention_days` **restores** it: its refs and packs as they were, with the settings it is declared with now. |
| Leave it removed past `code.deleted_retention_days` | **Purged** by the `code-retention` duty, in bounded steps, and its name is free again (see [Deletion and purge](#deletion-and-purge)). |
| Turn `code.native` off | **Nothing is deleted.** A node whose native runtime is already running keeps serving the code host until its restart (see [A native backend changes at a restart](control-plane.md#a-native-backend-changes-at-a-restart)), and a revision without the host reconciles no repository at all. |

Only repositories the **configuration** manages are reconciled. A repository
somebody else created is never deleted for being absent from `code.repos`.

### Protection rules

A rule's pattern is a branch or tag name in git's own grammar, in which a
component of `*` matches one component, `*` inside a component matches any run
of characters but `/`, and a component of `**` matches any number of
components — zero or more in the middle of a pattern, one or more as its last.
A pattern with no `*` matches **only itself**: a rule on `main` protects
`refs/heads/main` and not `refs/heads/main/x`, which git's own `for-each-ref`
would also match. So `release/**` protects `release/1.0` and
`release/2/hotfix` but not a branch named `release` — a rule meant for both is
the two rules `release` and `release/**`. A glob has at most eight components
counting `refs/` and its namespace, which is what bounds the cost of asking it
about a ref.

What a rule does to a push of a ref it matches:

| Field | Unset | Set |
|---|---|---|
| `allow_force` | A push that moves the branch to a commit not descending from it is refused | Allowed. Refused on a tag rule: a tag never moves |
| `allow_delete` | Deleting the ref is refused | Allowed |
| `direct_push` | `persons`: a person may push to it, and nobody else — a seat's push is refused `protected_direct` ([A seat's push](#a-seats-push)) | `none`: nothing pushes to it — the ref is frozen |

**Every rule that matches holds**, because two rules written for two reasons
both mean something: a ref matched by a rule allowing deletion and another
that does not cannot be deleted. A ref **no** rule matches may be force-pushed
or deleted by anybody who may push to the repository, so `crewlet validate`
warns about every repository whose default branch no rule protects — at the
repository's `protect`, naming the rule to add — and never fails on it. An
archived repository is not warned about: it takes no push at all, so nothing
can move or delete its branch until it is unarchived, and the revision that
unarchives it brings the warning back.

A declared repository's rules change only with its declaration. Those of a
repository a tool made change one pattern at a time through a lifecycle
request, where a seat may add a rule or tighten one and only a person removes
or loosens one, a rule's [merge gate](#merge-gates) included, field by field
([Who may change a protection](#who-may-change-a-protection)).

### Merge gates

A rule over branches can also say what a **merge** into them needs: its
**merge gate**. Any of these fields set on a `branch` rule forms one, and every
field it leaves unset takes its default:

```yaml
protect:
  - branch: main
    merge_by: maintainers            # writers (unset), maintainers or persons
    approvals: one                   # none (unset) or one counted approval
    approvals_unless: [comment_only] # path classes whose changes need no approval
    human_paths:                     # paths a change touching waits for a person
      - glob: ".crewlet/**"
      - glob: "deploy/**"
        unless: [comment_only, whitespace_only]
    checks: [lint, unit]             # the checks a merge waits for, by name
    landing: {policy: pre_land, depth: 4, batch: 4}
    owners_approve: true             # one approval eligible for every owned glob touched
    delete_source: seats             # seats (unset) or none
```

| Field | Unset | Set |
|---|---|---|
| `merge_by` | `writers`: anybody who may write to the repository merges | `maintainers`: only those who maintain it; `persons`: a person, never a seat |
| `approvals` | `none` | `one`: a merge waits for one counted approval |
| `approvals_unless` | none | The [path classes](#change-classes) — `whitespace_only`, `comment_only` — that exempt a change from those approvals: one whose every touched path is of a class listed waits for none |
| `human_paths` | none | At most 32 globs, in the [path owners'](#path-owners) grammar: a change touching a path one matches waits for a **person's** approval, whatever else approved it — unless every path that glob matched is of a class its `unless` lists |
| `checks` | none | At most 32 checks a merge waits for, each by the name a repository's [checks file](#the-checks-file) gives it: a lowercase letter or digit, then up to 63 more of them, `.`, `_` and `-` — one rule for both, so a rule never requires a check no repository can define |
| `landing` | `pre_land`, `depth: 4`, `batch: 4` | `policy`: `pre_land` tests each change merged on the branch before it lands, and `post_land` lands changes ahead of their checks and checks the branch's newest tip; `depth`: how many candidates the queue tests at once, 1 to 8; `batch`: how many waiting changes one candidate takes, 1 to 8 |
| `owners_approve` | off | A change waits for one counted approval from somebody eligible for every [path owner](#path-owners)'s glob it touches |
| `delete_source` | `seats`: a merge deletes a source branch in a seat's own namespace, in the record that merges it | `none`: it keeps every source branch |
| `auto_revert` | off | Whoever queues a change may consent, when they queue it, to the revert of a landing that breaks the branch |
| `agents_edit_checks` | off: on a rule requiring checks, a change to `.crewlet/**` — where a repository's checks are defined — waits for a person | A seat's approval stands for it too |

A merge gate on a `tag` rule is refused, at the first gate field it sets: a tag
never moves, so no merge reaches one. `no_code_change` is refused in
`approvals_unless` and in an `unless`: it is a revision's class, never a
path's, and an approval already carries across a revision that changed
nothing ([Merge readiness](#merge-readiness)). Each check, each human path's
glob and each class is listed once. A
gate is recorded with every field resolved and every list sorted, whatever
order the document wrote them in, so one declaration is one record on every
node, and an activation that changes nothing writes nothing for it. A gate
weighs at most 8 KiB as the code log records it — reached only with both
lists near their bounds at once, or with globs JSON spells long (`&`, `<` and
`>` are six bytes each) — which keeps a repository's 64 rules' gates within
512 KiB of its settings.

**`code.merge_by_floor`** — `writers` (unset), `maintainers` or `persons` — is
the least a merge into **any** protected branch needs: a merge is held to the
stricter of the floor and the `merge_by` of every rule matching its branch, a
rule with no gate reading as `writers`.

A repository a tool made takes its gates through `code_protect_branch`, a
field at a time, each field a call leaves out kept as the branch's gate has it
— or defaulted as above, on a branch whose rule has none — and only a person
loosens one ([Who may change a protection](#who-may-change-a-protection)).

A gate is **recorded, answered and judged, and nothing merges by it yet**:
every change into a gated branch is judged under it, its readiness listing
each term the gate holds it to ([The merge gate](#the-merge-gate)); a read of
one repository in detail answers every gated rule's gate beside its pattern
(`GET /code/repos/{name}`'s `gates`, and `code_get_repo`), and a push is held
to the rule's `allow_force`, `allow_delete` and `direct_push` alone, as before
([What it does not do yet](#what-it-does-not-do-yet)). No other read carries
a gate: a listing of repositories answers each rule as a push is held to it,
and so does the row every git request, every git node's sync and every push's
decision reads — at its bounds a repository's gates are half a mebibyte,
which none of them needs, and a git node rewrites the row it keeps at every
push.
`crewlet validate` warns — never fails — about a gate field that would do
nothing: `owners_approve` on a repository that names no path owners, `landing`
or `auto_revert` on a rule that requires no checks (only a branch whose merges
wait for checks has a queue), and `approvals_unless` beside `approvals: none`.

### Path owners

A repository's **path owners** say who owns which part of its tree: each a
unit — whose [effective lead](organization-model.md#lead-inheritance) a review
is asked of — or a seat, and the globs of the paths it owns. What a path owner
is for is **review routing**: a change whose revision touches an owner's paths
asks it for a review once the revision's facts are recorded ([Recorded on the
change](#recorded-on-the-change)), which its repository's home does for every
live change ([The home's change work](#the-homes-change-work)) — and today
only a push opens one ([A change a push opens](#a-change-a-push-opens)). It
**grants nothing**. A seat named a path owner reads
and pushes the repository by its [grant](#who-may-read-and-push) exactly as it
did before — owning `deploy/**` lets no seat push there that could not, and a
`visibility: unit` repository still shuts out a seat outside its unit,
whatever paths it owns — because a seat's standing is derived from the chart
and its tier, and no path owner is read there.

A glob is **a [protection rule](#protection-rules)'s pattern grammar, read
over a tree's paths**: it starts at the repository's root, a component of `*` matches one component,
`*` inside a component any run of characters but `/`, and a component of `**`
any number of whole components — zero or more in the middle, one or more as
the last. So `*.go` is the Go files at the root and no deeper, `**/*.go` every
Go file of the tree, `docs/**` everything inside `docs` but never a file named
`docs`, and `web/**/test/*` every file directly inside a `test` directory
anywhere under `web`. A glob with no `*` matches **only** that path, where git's
own pathspec would also match everything below a directory of that name. It
is at most 128 bytes, and one holding a `*` at most eight components, which is
what bounds what asking it about a path can cost. `*` is the one
metacharacter: `?`, `[`, `\` and a leading `!` — read by another glob dialect
as a single character, a class, an escape and a negation — are refused rather
than read as themselves.

A repository names at most **64** owners, each owning at most **32** globs —
sixty-four teams or seats on one repository, and a handful of directories
each — and an owner is named once: everything one owner owns is one entry, a
unit named by its name and by its key being one owner. The whole weighs at most
**256 KiB** as the code log records it, each owner by its key or handle:
every create, settings change and restore of the repository carries its path
owners whole, and a record is at most about a mebibyte beside the widest
[protection rules](#protection-rules) and their gates. Only the counts near
their bounds with long globs reach it — 2 048 globs of a hundred plain
characters are about 214 KB — or globs whose `&`, `<` and `>` JSON writes as
six bytes each. A scratch repository takes none; it is its seat's own. Path owners are part of the settings, carried
whole by the records that carry them and settled at an activation like every
other field ([What each change does at its activation](#what-each-change-does-at-its-activation)).
For a repository a tool made, a settings change adds globs to an owner and
removes them, applied to the owners the deciding node's rows hold — never a
whole list the asker read elsewhere — so two requests giving one owner a glob
each both land; an owner left with no glob is no path owner any more, and an
owner a change gives globs to must be one the chart holds, while one it only
removes them from need not be. A repository's detail answers its path owners
(`GET /code/repos/{name}`), in their one order: by kind, then reference, each
one's globs sorted.

Where a rule's [merge gate](#merge-gates) sets `owners_approve`, a merge into
its branches waits for an approval eligible for every owned glob it touches, so
the path owners are no longer a review's alone: taking a glob from one, or an
owner whole, lets a merge past an approval it needed, and that is a person's
([Who may change a protection](#who-may-change-a-protection)). Giving an owner
globs only asks a merge for more, and a seat that maintains the repository
still does it.

### The reconcile and the activation fence

Every data node reconciles `code.repos` at **every boot and every
activation** — a node in a [maintenance mode](../guides/retention.md#the-three-modes)
excepted, since it publishes nothing. Only a repository that changed costs a
write: one that is created, settled, deleted or restored. Nodes reconciling one
activation at the same moment race on each of those, and the log lands one
record on each — the others decide it again from that record and write
nothing. A refusal for one repository is logged and the rest are still
applied.

The reconcile runs **beside** the apply, not inside it: the apply reports the
revision applied as soon as it has asked for the reconcile, and the
repositories follow. At the most a document may declare, an activation that
creates, deletes or restores every repository takes 16–19 seconds on one node
before the last has its record — and a node still applying a revision
45 seconds after a peer applied it is [shed](control-plane.md#posture-what-a-lagging-node-does),
so how many repositories a company declares must never decide that. A newer
activation's reconcile ends an older one's still running, since the newer one
decides every repository again; a node that stops mid-reconcile logs
`code_repos_interrupted`, and its next boot reconciles.

Because the revision is already reported applied, a reconcile that leaves
something undone — an outcome the log did not answer, decisions refused while
the node was still catching up on the fence, a listing it could not read — is
**run again by the node**: five seconds later, then waiting twice as long after
each run in a row that leaves something undone, up to every five minutes, until
one applies, a newer activation replaces it or the node stops. Nothing else
would run it again, and no peer covers for a fleet of one. A run again costs a
batch read of the rows where nothing changed, and writes only what is still
missing. A name held by a repository the configuration does not manage, and a
declaration the code log refuses as written, are said at every run but never
run again for: no run changes them, and only a revision that does applies them.

Without an order between activations, a node applying an *older* activation
late would walk a newer one's settings back. So a reconcile first publishes
one **fence** record for its activation, and every node's copy of the rows
raises a single fence to it and never lowers it; every configuration record
an older activation decides afterwards is refused by every node, identically,
because the fence is derived from the log. Every decision a reconcile takes
waits for its own node to have applied its own fence first. So whatever an
older activation decided is either *before* the fence — in the rows the newer
activation decides from, where any difference it made is settled back — or
*after* it, and refused. That is what lets an unchanged repository cost
nothing: an activation that changes nothing writes its one fence record, and
reads every declared repository's row in one batch.

A node that finds a later activation already fenced logs
`code_repos_superseded` and decides nothing: it is applying a configuration
the fleet has moved past, and reconciles again when it applies the later one.

---

## Repositories made by tools

Beside the projects `code.repos` declares, the code host keeps repositories a
**tool** made, and decides what a person, a unit's lead or a seat asks of
them — create, rename, change the settings of, archive, unarchive, delete,
restore, or protect a branch of — as one **lifecycle request** on any data
node, in one snapshot of the rows. What a request sends for a repository that
exists is a **change** — one field, one protection rule, the archive flag,
globs added to or removed from a [path owner](#path-owners) — which the
deciding node applies to the settings its own rows hold, never a whole set the
asker read elsewhere: two people adding two protection rules at once each keep
theirs. A request names its operation, so asking it again is
one write, answered from the first; and a create's repository id is derived
from its operation, so a retry long after still finds the repository it made.
A repository `code.repos` declares refuses every tool's request
(`config_managed`, naming the declaration to change instead), and a
configuration never adopts one a tool made.

**Two kinds, and the name says which.** A **project** repository's name is
one host-name label, as a declared one's is: a person creates one for any unit
or seat of the chart, and a seat only for the unit whose effective lead it is,
at `full_access`. A **scratch** repository is a seat's own, named
`<handle>.<slug>` after it — two labels and one dot, 63 bytes in all, the slug
never `git` — created only by that seat, at `full_access`, owned by it, taking
no protection, and deleted once it has sat idle (below). A person never
creates one. A rename keeps the kind's form for everybody, a person included:
a scratch repository keeps its seat's handle, and a project stays one label,
since a project renamed to `alice.web` would read as seat `alice`'s own and
squat the name `alice` would create.

**Who may.** A person maintains every repository a tool made. A seat may what
its [grant](#who-may-read-and-push) on the repository maintains: the owning
unit's effective lead at `full_access` a project, and the seat a scratch
repository belongs to its own. A settings change is judged as the repository
stands **and** as the change would leave it, so a seat cannot hand a
repository to an owner, or narrow it to a visibility, under which it would no
longer maintain it — it could not take it back. Nor does a seat ever leave a
project owned by a seat, its own included, by a create or a settings change,
though it maintains one a person gave it: a seat's own repositories are its
scratch repositories, which `code.scratch.max_per_seat` caps and an idle
expiry deletes, while a project owned by a seat never expires and counts only
against the 10 000 every project in the company shares — so only a person
makes one. **A protection binds every seat, its unit's lead included**: a
seat that maintains a repository may add a [protection
rule](#protection-rules) to it or tighten one, and never remove one or loosen
one — its [merge gate](#merge-gates) weighed field by field, and its [path
owners](#path-owners) too where the gate makes them gating — which is a
person's ([Who may change a protection](#who-may-change-a-protection)).
Everything else is refused
`not_permitted`, with the reason, and nothing is written. Every request reads
the chart, so a node [behind the company's
configuration](#who-may-read-and-push) decides none of them and the next data
node does.

**Two caps.** A tool's create of a project is refused `repo_limit` when the
code host already holds 10 000 project repositories — declared and tool-made
alike, every one short of purged — the bound `code.repos` is held to. A seat's
scratch repositories never count there; each seat holds at most
`code.scratch.max_per_seat` of its own (50 unless set; 0 for none), **every
one it owns counting until it is purged**, a deleted one included, so a
create-and-delete loop cannot hold more (`scratch_limit`). A restore adds no
repository, so neither cap refuses one. A scratch repository costs the code
log one create record, once: under a kibibyte of signed frame at the longest
name, under two with the longest description.

**A rename leaves the old name redirecting** for a month (`RedirectWindow`):
the old name resolves to the repository under its new one, so
[a clone or a fetch by the old URL still works](#what-git-prints-and-what-it-means)
— the advertisement it begins with is answered `302 Found` to the new name,
and the fetch git sends after it is served at either name, since git moves to
the new name only when the redirect answered its very first request: one that
presented its credential up front, which git then says (`redirecting to`),
never one whose credential helper answered a challenge first. A push of the
old name is refused, naming the new one, so nothing is pushed to a name the
repository no longer holds. Nothing else may take the old name while it
redirects. Renaming back takes it back. The `code-retention` duty
releases an old name at its window's end, one record a name, after which it
names nothing and is free; an old name whose repository was purged meanwhile
stays reserved until its window ends, like every other.

**A tool's repository is searched with `grep`** until somebody who maintains
it asks for an index: a create makes it `search: grep`, and a settings change
sets its [search mode](#declaring-repositories) — a person's on any repository
a tool made, a seat's on what it maintains. A **scratch** repository is `grep`
for good: asked for an index, by its seat or by a person, it is refused
`not_permitted`, since an index of a seat's experiment would be built and kept
on the git nodes for a repository deleted once it sits idle — and a search
that names it still reads it.

**Archive, delete and restore** work as they do for a declared repository: an
archived repository is read-only, a deleted one takes no push and keeps its
refs and packs, restorable as it was until its [purge](#deletion-and-purge),
and a restore through the tools brings it back.

### Who may change a protection

A repository a tool made changes its [protection rules](#protection-rules) one
pattern at a time, with `code_protect_branch`. A call sets the rule's push
controls — `direct_push`, `allow_force` and `allow_delete` — whole, each it
leaves out taking its least (`persons`, off, off), and changes the branch's
[merge gate](#merge-gates) **a field at a time**: each gate field it names
replaces that field, every field it leaves out stays as the gate has it — or
takes its default, on a branch whose rule has no gate yet — and `remove_gate`
takes the gate away. The deciding data node puts the change in the gate its
own rows hold, never a gate the caller read elsewhere, so two calls changing
two fields of one gate at once both keep theirs; and it refuses a gate the
change leaves weighing past the 8 KiB a gate may. The call answers the rule as
it now stands, its gate whole.

**A protection binds every seat, its unit's lead included.** A seat whose
grant maintains the repository may add a rule or tighten one, and never remove
or loosen one: that is a person's. A change is weighed against the rule the
repository holds for its pattern when the request is decided, so two requests
cannot loosen a rule between them, and one that tightens one control and opens
another is refused for the one it opens, `not_permitted`, its sentence naming
what it opens. Otherwise a lead steered by what it read could lift `main`'s
protection and then force it, or lower its gate and then merge past it: the
protections would bind every seat but the one with the most reach.

| Control | A seat may | Only a person may |
|---|---|---|
| The rule | Add one | Remove one |
| `direct_push` | Make it `none` | Make it `persons` where it was `none` |
| `allow_force`, `allow_delete` | Turn either off | Turn either on |
| The merge gate | Add one to a rule with none | Remove one |
| `merge_by` | Raise it: `writers`, `maintainers`, `persons` | Lower it |
| `approvals` | Raise it to `one` | Lower it to `none` |
| `approvals_unless` | Take a class out | Add a class |
| `human_paths` | Add a glob, or take a class from one's `unless` | Drop a glob — a broader one put in its place included — or add a class to one's `unless` |
| `checks` | Require one more | Drop one |
| `landing.policy` | Move it to `pre_land` | Move it to `post_land` |
| `owners_approve` | Turn it on | Turn it off |
| `auto_revert`, `agents_edit_checks` | Turn either off | Turn either on |
| `landing.depth`, `landing.batch`, `delete_source` | Change them either way: how fast a queue lands and what a merge tidies after it, never what a merge is held to | — |

A value the deciding node's build does not know — one a newer build wrote —
counts as stricter than every value it knows, so a seat moves `merge_by`,
`approvals` or `landing.policy` neither away from one nor to one, and
`direct_push` away from one only to `none`, which admits nobody. And since
`owners_approve` makes the repository's [path
owners](#path-owners) whom a merge waits for, a seat may not take a glob from
a path owner, or remove an owner, while any rule's gate sets it: that settings
change is refused `not_permitted`, naming `path_owners`, what it takes and the
rule, while giving an owner globs lands. A person's request is weighed by none
of this, and a repository `code.repos` declares takes no tool's request at
all.

### A scratch repository's idle expiry

A scratch repository with no push, rename, settings change or restore for
`code.scratch.idle_days` (7 unless set) is **deleted** by the `code-retention`
duty — restorable for `code.deleted_retention_days` like any deletion, then
purged. Reading a repository is no activity, and an archived one is kept: its
seat archived it on purpose.

**A scratch repository a live turn has named is never expired.** Every request
a seat's turn makes about a repository — a read, a write, a lifecycle request,
through its own tools, a delegated worker's or an agent-mode run's bridge —
takes the turn's **hold** on it first, and a turn holds what it named until it
ends: while it runs, in the memory of the node running it; once it has a
[coding run](code-sandbox.md)'s row, on the row as well, unioned at every
launch and released with the row when the run ends. So a turn parked for days
on a question keeps its experiment. A hold moves no clock: once the turn ends,
the repository is expired at the duty's next pass if it is still idle.

Each pass fixes its cutoff, and the position its page of due repositories was
read at, **before** it reads the holds — every run row the fleet holds,
whatever its status, and the running turns of every node holding a seat
lease, each asked and each required to answer within two seconds, whichever
other nodes answer too; the rows are read again once the nodes have answered,
since a turn's first coding run moves its holds from its node's memory onto
the run's new row meanwhile — and
decides each expiry against that page: a repository with any record on it
since, or active since the cutoff, is kept. A turn taking its hold on a
repository idle 22 hours or more also records it on the repository, which the
broker orders against the expiry: recorded first, the expiry is refused;
expired first, the turn's request is refused before it reads anything, and the
repository can be restored. Under 22 hours no pass in flight can have
listed it, since `idle_days` is a day at least and two nodes' clocks may
disagree by two hours. Such a record is one a **turn**, not one a repository:
a hold moves no clock, so a seat that keeps reading an experiment it no longer
pushes to appends one small record to the code log in every turn that names
it, from the repository's 22nd idle hour until a push moves its clock or the
expiry takes it — and one more in a turn resumed so long after its work began
that the engine mints the writes of its resumed half anew (past 29 days, a day
inside the 30 the code log's operation ledger remembers), which records its
holds at that instant too: recorded at the run's start, a hold would be one no
node could confirm, and every call the turn makes about the repository
refused. **A hold that cannot be read expires nothing**: a run
store that cannot be listed, or a seat holder that does not answer, ends that
pass's expiry, says so on its `code_retention_pass` line
(`scratch_expiry_unread`) and logs `code_scratch_expiry_skipped` at warn; the
next pass asks again.

A pass's line counts `released` old names, `expired` scratch repositories and
`scratch_held`, the idle ones a live turn kept.

---

## One subject per repository

Everything that moves a repository's refs or packs, or governs them — a push,
a settings change, a deletion, a restore, a purge step — is a record on **one
subject a repository**, `crewlet.code.log.repo.<id>`. That subject is the
**arbitration unit**: two pushes to one repository contend at the broker and
exactly one record lands per round, and the loser decides again against the
winner's rows. One subject a repository rather than one a ref, because every
write on a repository changes its row too — its ref count, its bytes, its
version — so a subject a ref would never narrow a write's contention.

A **name** is claimed on its own subject, `crewlet.code.log.name.<token>` (a
bounded digest of the name), so two creates of one name contend there, where
two freshly minted repository ids never would. The activation fence has the
company's one subject.

```mermaid
flowchart LR
    subgraph log["CREWLET_CODE_LOG"]
        N["name.#lt;token#gt;<br/>create (claims the name)"]
        R["repo.#lt;id#gt;<br/>push · settings · delete · restore · purge step"]
        C["company.activation<br/>the activation fence"]
    end
    log --> A[node-a rows]
    log --> B[node-b rows]
    log --> D[node-c rows]
    R -. names .-> O[(object store:<br/>one bundle a push)]
```

---

## The committer

Because every push to a repository contends on its one subject, a
repository's pushes are **committed by one node**: its **committer**, the
first live data node in a rendezvous order of the repository's id, which every
node computes alike. A push received anywhere is asked of that node, which
takes the repository's pushes in order and puts every push waiting behind a
record in flight into the next one — **group commit**, without lingering for
more, up to 64 pushes a record. An idle repository pays one record a push and
a burst one record a batch. A batch is decided in one snapshot of the rows,
each push against what the ones before it did, and a push is answered `ok`
only from the row its record applied, never from the decision. A push whose
[changes](#revisions) would take the record past the rows one record may
write, beside the pushes before it, is decided first in the next record
instead, with the pushes behind it after it.

The live data nodes are the ones each node's view of the fleet's presence
names. The view hears a presence claim or release as it lands and lists the
fleet again then — see
[Coordination](coordination.md#who-is-in-the-fleet-asked-per-request) — so a
data node that joins is in every repository's order within about a second of
its claim, and a fleet started all at once agrees on every committer within
about a second of the last node's claim. A push received inside that second
may be committed by a node that has not yet heard its committer join: correct,
since the pushes still contend on the repository's one subject and the broker
decides between them, and short only of that push's group commit. A node
without `data` that has heard of no data node yet has nobody to ask, and
refuses a push, a fetch or a read as unserved until one claims. A committer
that stops without releasing its presence stays in the order until its lease
lapses, asked after the rest once it has gone silent — see
[Scaling](scaling.md#how-a-request-reaches-the-estate).

A push carries an id the receiving node minted, and a push id the repository's
history already holds is answered from its row — so a push asked twice, or of
a second node after the first stopped answering, is still one push. A
committer that stops, or answers nothing, hands each of its repositories to
that repository's runner-up in the same order, under the same push id. A node
in a maintenance mode commits nothing and refuses as a node that ran nothing,
so the next committer decides; while the whole fleet is in one, no push is
committed.

The path a push takes from the node that receives it:

```mermaid
sequenceDiagram
    participant G as git client
    participant RX as receiving node
    participant CM as committer (first in the repo's order)
    participant L as CREWLET_CODE_LOG
    participant AP as every data node's applier
    G->>RX: push
    RX->>RX: store the pack bundle (one object)
    RX->>CM: propose (push id, commands, bundle)
    CM->>CM: batch with the pushes queued behind it,<br/>decide in one snapshot
    CM->>L: one record on repo.#lt;id#gt;
    L->>AP: apply into the rows
    AP-->>CM: the push rows its record wrote
    CM-->>RX: ok / refused, per command
    RX-->>G: report
```

A queue holds at most 1 024 pushes, and a push waits in it only while its
asker's attempt still holds a batch — which has 30 seconds to land — and its
answer; past either bound a push is refused `busy` undecided and the client
pushes again. The `crewlet.code.commit.busy` counter, in the
[metrics reference](../reference/metrics.md), counts each by its `reason`:
`queue`, the repository's queue full; `node`, the committer's node holding as
many proposals across its repositories as it may; `waited`, a push that
queued with time to spare and lost it waiting behind the repository's earlier
pushes; `late`, a push that arrived with too little of its asker's attempt
left; and `contended`, a record that lost every round at the broker. The
first three are a **backlog** — this committer turning pushes away, at its
queue's count or at its time: a committer that commits a repository's pushes
more slowly than they arrive holds fewer of them than the count by the time
the oldest has waited out its asker's attempt, and turns the rest away
`waited` — and the committer's node raises the
[`code_commit_busy`](../reference/alarms.md) alarm for fifteen minutes after
one, naming the repository with the most, whichever node received the pushes.
Its log says each repository's refusals once a minute, by reason
([`code_commit_busy`](#what-the-node-logs)). A push that arrived late or a
lost race raises nothing: the first spent its asker's attempt before it
reached this committer, on the nodes it crossed, and the second is a second
committer deciding the repository during a handoff.

---

## Limits

[`code.limits`](../getting-started/configuration.md#code) — the largest pack
one push may send, the largest file it may carry, the most one repository
may hold, and how many pushes one seat may land in a day ([A seat's
push](#a-seats-push)) — is read off the **deciding node's current revision**
at every decision, so a change holds from its activation with no restart. A
seat outside a repository's owning unit is held to half of its live refs and
half of `repo_max_bytes` ([A seat's push](#a-seats-push)). A repository
also holds at most 100 000 live refs, and a push moves at most 1 000 of them;
the refs it deleted within 48 hours are kept apart, up to another 100 000, the
oldest forgotten sooner past that — a node last synced before them reads the
repository whole, and so does a node whose last sync is behind more changes
than the repository now holds, since what is left is then fewer rows to read.
A deletion never counts against the live refs, and never refuses a push: a
push that deletes refs and creates others is judged on what it leaves, so at
the limit `git push origin :old new` — or a `--prune` push — goes through.

A repository also holds at most **2 048 live packs**: every push that carries
objects stores one, and they stay until a compaction merges them. The bound is
a cold read's: a git node that has never served the repository downloads one
object for every live pack before it answers, sixteen at a time, and from an
S3 fake answering each request 200 ms late a cold read downloaded 2 048 of
them in 26.4 s, 77 a second — within 27 s, half of 55 s, the minute of quiet a
balancer allows a git request ([Deployment](../guides/deployment.md)) less a
keepalive, with the other half left for indexing what arrived — and ended its
whole read at 26.6 s. A push
carrying a pack into a repository that holds that many is refused
**`compaction_behind`**, whole: nothing of it is decided, no row names
anything it uploaded, and it lands when it is run again once a compaction has
merged the repository's packs. A push that carries no pack — deletions, or
refs moved onto commits the repository already holds — adds none, and is
never refused for it. The node that received the push refuses it before
uploading anything, once it has read the repository's row again; the
repository's [committer](#the-committer) decides it for good, counting the
packs of the pushes it commits in the same batch ahead of it. So a push whose
node read the repository below the bound, where pushes that landed since have
taken it, is refused by its committer only after its pack was uploaded, and
the [object collector](object-store.md#collection-and-audit) deletes that
pack a day later ([a push, end to end](#a-push-end-to-end)).

A push's ref updates must also fit the one record its committer writes for
it: about 1 MiB of them, as the code log spells them. A thousand plain refs of
the longest, 255 bytes, take under half of that. A ref whose characters the
log spells in six bytes each (`&`, `<`, `>`) costs up to six times its
length, so a push of some six hundred such refs of the longest reaches the
bound first. The receiving node measures a push's ref updates as it reads
them, with everything the rest of the push could add at its largest, and
refuses one past the bound before anything of it is run or stored.
[`crewlet code import`](../reference/cli.md#crewlet-code-import) cuts each
batch of refs it pushes by the same rule, so an import never meets it.

What a repository holds is its packs **as the pushes stored them** until its
maintainer merges them ([Compaction](#compaction)) — which matters for a history
that arrived in many pushes rather than grew by them. A history [imported](../guides/code.md#import-from-github-or-gitlab)
in steps stores more than one pack of it: every step keeps whole the files it
touches whose earlier versions an earlier step stored. Measured on this
project's own history, 34 MB as one pack stored 68.5 MB in eleven steps and
78.6 MB in forty-two. Those bytes are what the object store holds, what every
git node serving the repository caches and what a cold sync downloads, so
size `repo_max_bytes`, the object store and the git nodes' caches for the
repository as it was pushed, not for the source it came from. A merge writes
each object of the packs it takes once, and git searches for deltas among the
objects it merges, so a file a step kept whole may be stored as a delta again
once its maintainer merges the steps' packs; how much that saves depends on the
history, which is why the sizing above is for the repository as it was
pushed.

### A ref and a directory of refs

A ref is never created beside a directory of its own name. With a branch
`feature`, a push creating `feature/x` is refused `ref_conflict`, and so is one
creating `feature` while any `feature/…` branch is live; the sentence names the
ref in the way. git keeps a ref as a file under `.git/refs`, and no clone can
hold a file and a directory of one name: a repository that took both was
cloned whole — a clone writes its refs into one packed file — but every
existing clone's `git fetch` then failed (`'refs/remotes/origin/feature'
exists; cannot create 'refs/remotes/origin/feature/x'`) until somebody deleted
one of the two. Branches and tags never meet (`refs/heads/v1` and
`refs/tags/v1/x` are different directories), and siblings never do
(`feature-x`, `feature.x`).

Like the ref limit, the rule is judged on what the push leaves: a deleted ref
blocks nothing, so renaming `feature` to `feature/v2` in one push —
`git push origin :feature HEAD:feature/v2` — goes through. A clone that
already holds `origin/feature` fetches the rename with `git fetch --prune`,
which removes the old remote-tracking ref before it writes the new one. Of two
creates in one push, or in one batch of the [committer](#the-committer), that
would sit beside each other, the first stands and the second is refused.

---

## Compaction

Every push stores one pack, so a busy repository's pack count grows with its
pushes — and a git node that reads it whole pays a GET for every pack, and
every request a lookup in every pack its index does not cover. The
repository's **maintainer** merges them — the git node that also publishes its
layers ([Who maintains a repository](#who-maintains-a-repository)), on a fleet
of alike git nodes its [home git node](#the-home-git-node) — and its committer
decides the merge as it decides a push
([ADR-0060](https://github.com/crewlet/crewlet/blob/main/adr/0060-a-compaction-merges-packs-without-judging-reachability.md)).

**The roll.** Once a minute every git node lists the repositories holding
sixteen live packs or more, most packs first, judges which of them it
maintains against one listing of the live git nodes, and for each it maintains
it
plans git's own geometric split — the one `git repack --geometric=2` makes —
over the packs that are not FULL and that no [published
layer](#published-layers) names: a layer is valid only over every pack it
names, so a published pack is merged only by the publication that replaces its
layer ([When a repository is published](#when-a-repository-is-published)). The
lightest are merged until every pack left weighs at least twice the objects of
all the lighter ones together, and only where that takes sixteen packs or more,
so a repository pushed a pack at a time is merged sixteen pushes at a time
rather than at every poll. A
pack is full once its pack section, or its whole bundle, is at seven eighths of
the 1 GiB a merge cuts its outputs at, and a full pack is never merged again.
A repository holding more than 50 packs that are not full is merged two packs
at a time even when they already stand in that progression, so it shrinks at
every poll. One merge takes at most 1 024 packs and 8 GiB of pack bytes, the
lightest first. A repository whose plan finds nothing worth merging is not
planned again until its rows move, so an idle repository costs its
maintainer's minute a lookup, and every other git node's the one judgment of
who maintains it.

**The build.** One `git pack-objects --stdin-packs` over the packs merged,
run in a scratch repository — never in the copy — whose one alternate holds
exactly those packs, hard links of the copy's own files, so the walk git makes
from the merged packs' commits to sort their objects for delta compression
ends where they do: over the whole copy it ran the repository's history, and a
merge of a few small pushes into a large repository cost time and memory in
proportion to that history. It runs in the node's maintenance lane: at the
background indexing's own limit, never the slot kept for
`crewlet code verify`, with its memory charged against the node's background
budget beside the indexing's (550 bytes an object merged, 240 more an object
merged for a transfer's room beside them, and six times
`code.limits.max_blob_bytes` for its delta search, as a build was measured to
hold). It follows the merge and never the repository, where every other step
keeps room for a clone of the whole repository: a maintainer that refused a
large repository's every merge would refuse its pushes too, once its packs
reached the bound. It cuts its outputs at 1 GiB. **The outputs hold every object of the
packs merged** — an object a pack outside the merge also holds included —
because a merge judges no
reachability: no push decided against the packs it replaces can land a ref
whose history it dropped, and two merges planned from different moments never
each drop what the other relied on. Each output is one bundle in the stored
format, every one held to the bundle rule before a byte is uploaded and all of
them uploaded at once, since an object's key is minted as its upload begins
and a record may name only keys of the last twelve hours. The maintenance slot
and the memory go back as soon as the build's last git process ends, before
the uploads.

**The decision.** The maintainer proposes ONE compaction to the repository's
[committer](#the-committer) through the estate router, as a push is proposed,
under an id of its own — so the runner-up asked after a committer went silent
decides the same compaction, never a second. The committer decides it ALONE,
in one snapshot: every pack it merges still live, not full and named by no
published layer, or it is refused `stale` and the next poll plans again from
the rows; and ONE record adds the outputs' rows and moves the merged packs'
rows into a table of retired packs. Two git nodes that both take themselves
for the maintainer for a moment both build, and the second is refused `stale`;
what it stored is named by no row, and the object collector deletes it.

```mermaid
sequenceDiagram
    participant H as Maintainer
    participant S as Object store
    participant R as Estate router
    participant C as Committer
    participant L as Code log
    participant G as Every other git node
    H->>H: plan the roll, build in a scratch
    H->>S: store every output, at once
    H->>R: propose one compaction
    R->>C: the repository's committer
    C->>L: one record: outputs added, merged packs retired
    L-->>C: applied on every data node
    C-->>H: applied
    H->>H: link its outputs into its copy
    G->>G: next sync: the copy's next directory, of links
```

**Every copy follows.** The maintainer links the outputs it built into its
copy, so it downloads none of them; every other git node downloads them at its next
sync of the repository, and drops the merged packs by moving its copy into a
next directory of hard links, keeping the commit-graph, since a merge drops no
commit ([When packs leave a copy](#the-cache-on-a-git-node)). A request
already reading the old directory finishes on it.

**A day's grace.** A retired pack keeps its object for a day — longer than the
longest read a clone of the largest bundle a row can name may take (21 hours
21 minutes), by more than the two hours the clocks of the broker that stamped
the retirement and the node sweeping it may disagree by while no node raises
[`clock_skew`](../reference/alarms.md) — so a clone that began on it before the
merge finishes
([Live, retired and derived references](object-store.md#live-retired-and-derived-references)).
Then the `code-retention` duty removes its row, and the object collector
deletes the object at its next pass ([Deletion and purge](#deletion-and-purge)).

**When it cannot keep up.** A repository at the 2 048 live packs one may hold
refuses a push carrying objects `compaction_behind` ([Limits](#limits)), and
its maintainer raises the [`code_compaction_behind`](../reference/alarms.md)
alarm for every repository it maintains at that bound, from its own poll, for
as long as one is there — its log says why the merge has not landed:
`code_cache_compaction_refused` for a build the node's memory cannot hold,
`code_compaction_refused` for a refusal (`store_full` among them: an object
store with no room, which holds the repository back until the store has the
room for that merge, never retried as a blip, and lets it go once that merge's
outputs are stored, so a later merge is held back only by a refusal of its
own),
`code_compaction_failed` for a build, an
upload or an answer that failed, each tried again at the next poll. A
repository that takes pushes faster than one node merges them shows as
`crewlet.code.compact.duration` against the pushes' rate
([metrics reference](../reference/metrics.md)).

**What it costs.** The build holds about 550 bytes of memory an object it
merges — 139 MiB for a 100 MB source history's 295 680 objects pushed through
a host, and 147.5 MiB for the same history's 64 pushes as a publication
repacks them, measured on git 2.56.0 —
and more where its delta search meets large files (eight revisions of a
50 MiB file held 270 MiB), and takes 28 to 48 seconds for every gibibyte of
packs it merges, in the node's maintenance lane. Every other git node holding
the repository downloads each output once, so over a repository's life a git
node downloads, and writes, at most about 16 times what is pushed to it at
20 KB pushes, fewer the more pushes a merge takes at once
([Scaling Out](scaling.md#git-nodes) has what that bills); a merge's record
takes 43 to 108 bytes a push on the code log, and the record that sweeps its
retired rows a day later up to 44 more; a merged pack's row is kept a
day, about 608 bytes; and a git node drops the merged packs by a move into a
next directory that links three files a pack it keeps, whatever the packs'
bytes (over the in-memory store, whose download waits on no first byte, a
sync that downloaded a merge's output and moved a copy of 50 packs took
7 ms, and one of 2 048 took 119 ms; from a bucket the output's GET comes on
top). What it buys is the cold read: from an S3 fake answering each request
200 ms late — S3's slow first byte — the fifty or so packs a compacted
repository holds came up in under a second, where the 2 048 a repository may
hold took 27 seconds and the 10 000 of a history nothing merged over two
minutes ([The cache on a git node](#the-cache-on-a-git-node)).

**What it does not do.** It removes no object: what no ref reaches stays in
the repository's packs for as long as it lives. And it never touches a
published chain: a merge takes only the tail's packs, so a copy serving from
the chain keeps it, and its tip, through the swap. On a copy serving in own
mode a merge drops the copy's own multi-pack index and its bitmap — the
commit-graph survives — which the background indexing writes again at its own
pace, so a clone between the two counts its objects by walking.

### Who maintains a repository

A repository's background work — merging its packs and
[publishing its layers](#when-a-repository-is-published) — runs on ONE git
node, its **maintainer**: the first git node of the repository's rendezvous
order (the order its reads walk, [The home git node](#the-home-git-node))
whose **advertised background budget** holds every step of its maintenance —
the largest charge any of them is admitted at: its publication's bitmap over
its whole history, 600 bytes an object of the repository with room beside it
for a clone, or a merge of every object with its delta window, the larger
under some six million objects — and its home where no git node's does. A node
whose budget holds the publication but not a whole merge would refuse the
merge for memory, and a merge refused costs the repository its pushes once its
packs reach the bound, so the placement weighs the merge as well. Every git
node advertises its budget on its presence heartbeat, so every node reads the
placement from memory. On a fleet of alike git nodes every repository's
maintainer is its home; on a fleet with one large git node, every repository
only that node can hold is maintained there, and every other git node
installs what it publishes ([Sizing git
nodes](../guides/deployment.md#sizing-git-nodes)).

The git nodes weighed are the ones the fleet's presence listing carries, so a
node is a maintainer only once its peers can see it: a git node that is
starting maintains nothing until its first heartbeat is listed, about a second
after it starts, and its loops wait for that rather than run a pass before it;
a node that is draining has left the listing, and every repository it
maintained is placed among the rest.

Both of a maintainer's loops ask by that one charge, so the two
writers of a repository's packs run on one node, and there a merge never takes
a pack a publication of the repository in flight has planned over. Once the
publication has planned, it reserves every pack its layer will name, the packs
it repacks among them, and the repository's merges go on beside it with the
packs pushed since — which the layer neither names nor needs, so both land, in
either order — rather than wait out a build of minutes while a busy repository
climbs to its pack bound. Only while the publication syncs and plans, seconds,
does a merge of its repository wait; and a publication asked while a merge of
its repository is running waits for it, since it would cover that merge's
packs: either way round, one of the two would otherwise be refused `stale`
after its whole build. Nothing
leases a repository to its maintainer: two nodes whose presence views disagree
for a moment may both build, and the committer refuses the second `stale`.
Each pass judges every repository it lists against one reading of the
listing, so a repository a git node does not maintain costs that node's pass
the one judgment of who does, never a read of the presence view of its own. A
node that cannot read who serves git does none of a maintainer's work that
pass rather than guess (`code_compaction_maintainer_unknown`,
`code_publication_maintainer_unknown`). **The maintainer holds what it
maintains**: each merge and publication syncs the repository into its cache,
so a maintainer's cache budget holds the repositories it maintains, or each of
its publications begins with a whole download of its repository.

---

## Changes

A **change** — `web!42`, the 42nd change opened on the repository `web` —
proposes that one branch of a repository, its **source**, be merged into
another, its **target**. It is rows on the code log like the refs it names,
and every record that moves it is on the repository's own subject, beside the
pushes that move its branches: the repository's one
[committer](#the-committer) decides them in order and the broker arbitrates
them together, so a change is never both merged and closed.

Four tables hold a change, on every data node:

| Table | A row for | What it holds |
|---|---|---|
| `code_changes` | each change | Its number and title; its state; its source and target branches; the head of its latest revision; who opened it — a seat, or a person by the human seat they hold, with the credential they used; the work item it carries; when it was opened, closed and merged, and how; and how many bytes its description holds |
| `code_change_bodies` | each change with a description | Its description, up to 32 KiB |
| `code_change_authors` | each of its authors | Who wrote the change: whoever opened it, and everyone whose push moved its head, each from the revision they first wrote. An author's approval of their own change counts for nothing |
| `code_revisions` | each time its head moved while it was live | The new head, who pushed it and the push that did. Revision 1 is the source branch's head when the change was opened — made by the push that opened it, for a change a push opened, and otherwise by whoever opened it, by no push — and a reopen's revision its head when the change was reopened, made by whoever reopened it |

- **A number is the repository's own**, counted from 1 in the order the code
  log applies the records that open changes, so two changes of a repository
  never share one and none is skipped.
- **A state is one of four.** A `draft` or `open` change is **live**: the
  moves of its source branch's head are its revisions, and the branch is the
  source of no other live change. A `closed` change was closed without being
  merged, and may be opened again while its source branch is live. A `merged`
  change reached its target and stays merged; it says how (`push`: a push set
  its target to its head, or moved the target past it) and the commit its
  target was merged at.
- **A revision is a row, never a ref.** Nothing a client fetches advertises
  one, so a clone holds the repository's branches and tags and nothing more.
- **A description is a row of its own.** Every move of a change's head reads
  and rewrites the change's row on every data node, and the store reads a row
  whole: a description of up to 32 KiB on that row more than doubled what each
  revision cost. Kept beside it, a description costs a head move nothing, and
  the row still says how long it is, so a purge weighs it without reading it.
- **A change has no horizon of its own.** It is kept as long as its
  repository, and the repository's [purge](#deletion-and-purge) removes it:
  each change whole, its revisions, authors, description and
  [conversation](#reviews-and-findings) before its row, in the same bounded
  steps as the rest of the repository's rows.

### Opening, editing and closing a change

Every write of a change's lifecycle is a record of its own on the
repository's subject, decided by the repository's
[committer](#the-committer): it waits in the same queue as the repository's
pushes, in the order it arrived, and is decided alone, never batched with
them. Six writes:

| Write | What it does | Who may |
|---|---|---|
| Open | Proposes a source branch into a target branch: the change's row, its owner as its first author, and its first revision, the source branch's head | Whoever may push the source branch: a person, a seat that writes the repository, and a `full_access` seat from its own namespace (`refs/heads/seats/<handle>/`) — see [Who may read and push](#who-may-read-and-push) |
| Edit | Sets its title (one line, at most 256 bytes), its description (at most 32 KiB) or the work item it carries | Its owner, a principal that maintains the repository, and any person |
| Draft | Makes a live change a draft, or a draft ask to be merged | The same |
| Retarget | Moves its target to another live branch | The same |
| Close | Closes it without merging it, with a reason of at most 1 KiB | The same |
| Reopen | Opens a closed change again, at its source branch's head now — a revision of its own, made by whoever reopened it, where the branch moved while the change was closed | The same |

- **A number is the apply's.** The record that opens a change takes the
  repository's next number when it applies and moves it on, so numbers follow
  the order the code log applies the opens, and two opens decided at once
  never share one.
- **An open asked again is the same change.** A change's id is derived from
  the open's operation id, and the code log's ledger collapses the open asked
  again under it — of another data node after its committer stopped
  answering, or by an asker whose answer was lost — onto the record that
  landed, so no retry opens a second change.
- **A branch heads one live change at a time.** An open from a branch that
  already heads a draft or open change is refused, naming that change, and so
  is a reopen while another change is live on the branch: a push to the
  branch revises exactly one change.
- **Every write but an open names the version it was formed against.** A
  change's version counts the records that changed its row, 1 at its open; a
  write naming one the change has moved past — another write, or a push that
  moved its head, landed first — is refused `stale`, and its asker reads the
  change again.
- **A write that asks for what the change already says changes nothing.** An
  edit to its own values, the draft state it is in, the target it has, a
  close of a closed change and a reopen of a live one are answered from the
  change's row, at its version, and append nothing.
- **A seat is judged by the chart of the node that decides.** A node that
  holds no chart yet, or one [behind the company's
  configuration](#a-seats-push), decides none of a seat's writes and hands them
  to the repository's next committer, as it does a seat's push; a person's is
  decided as ever.
- **A seat and a person's assistant write them through tools** —
  `code_open_change`, `code_update_change` and `code_close_change`
  ([The company's code](../guides/tools-and-mcp.md#the-companys-code)) — each
  one write, named by one operation, so the same call made again is the same
  write. An open that names reviewers asks them in a second write once the
  change is open, a step of the open's own operation decided at or past the
  open's record ([Asking for review](#asking-for-review)): an open asked again
  asks them again under the same step, which the ledger collapses.

A write that is refused appends nothing, and says why in one sentence:

| Refusal | Means |
|---|---|
| `repo_deleted`, `repo_archived` | The repository is deleted, being purged or gone — in the sentence of the state it is in — or archived and read-only |
| `repo_importing` | The repository is being imported from another host, and takes no write until its import is done |
| `repo_mirrored` | The repository is a pull mirror, which moves only by its syncs from its source: every write but a close — and the landing its home records once a sync brings a merge in — is refused, so a repository that became a mirror with changes live can shed them |
| `change_not_found` | The repository holds no such change |
| `not_permitted` | The asker may not make the write: an open by one who may not push the source branch, saying what the asker holds and what would admit it; any other write by one who neither owns the change nor maintains the repository, nor is a person |
| `stale` | The change is at another version than the write was formed against |
| `same_ref`, `no_source`, `no_target`, `no_difference` | The source and target are one branch; the source or the target is not a live branch; the source is at its target's commit |
| `change_exists` | The source branch already heads a live change, which the sentence names |
| `change_closed`, `change_merged` | An edit, a draft's state or a retarget of a closed change, which is reopened first; any write but an open of a merged change, which stays merged |
| `busy`, `not_applied`, `unavailable` | The committer did not reach the write in time, or the code log refused it or dropped it on every node: nothing was decided, and the write is sent again |

`crewlet.code.change.proposals` counts every write a node's committer was
asked to decide, by its write and what became of it, and
`crewlet.code.commit.busy` counts one met busy under `kind` `change` (the
[metrics reference](../reference/metrics.md)).

### A change a push opens

A push opens a change from its options — `git push -o change` — **in the
push's own record**: the record that moves the branch opens the change on it,
so the commit and the change proposing it land together or not at all, on
every data node, and no second record is ever half of it ([how to open
one](../guides/code.md#open-a-change-from-a-push)).

- **From the one branch the push sets.** The change's source is the branch
  the push creates or moves, its head the commit the push set it to; a push
  that sets no branch, or several, opens none. Its target is the branch
  `change.target` names, or the repository's default branch; its title the
  one `change.title` names, or the subject of the commit the push set the
  branch to.
- **By its pusher, as an open is.** The pusher is the change's owner and
  first author, and the push its first revision. A seat's push names the turn
  that made it, and a push the seat's coding box made the coding run that
  pushed it too, and the change keeps both as the turn and the run that
  opened it. A coding box's push also names the work item its run was charged
  to, by its identity in its tracker, and the change carries that item, as
  the seat's own `code_open_change` would ([A run's
  changes](code-sandbox.md#a-runs-changes)); a person's push names none.
  Whoever may push the branch may open a change from it, so the push's own
  update is the check.
- **Numbered like any other.** The record takes the repository's next number
  when it applies, in the order of the pushes it carries, and a later push in
  the same record revises or merges the change as it would any live one.
- **A branch heading a live change names it.** A push to a branch that
  already heads a draft or open change revises that change, as every push to
  it does, and opens no other, whatever the options ask: they shape only a
  change a push opens.
- **Its options never fail the push.** Options that are no change's, a push of
  no branch or of several, and a change the rules of an open refuse — a target
  that is the source, no live target, a source at its target's commit — leave
  the push to land as one that asked for nothing. git prints one line of what
  became of the change, `remote: crewlet: change web!42 opened` or why none was
  ([What git prints](#when-a-push-asked-for-a-change)).

### Revisions

A change's revisions are the moves of its source branch's head while it is
live, and no record of the change's own says one happened: the push that moved
the branch does. Its record writes the revision — on every data node, from the
push's commands and the change's rows, in the same record as the refs it
moves — and each push of a record sees what the ones before it did.

- **A push to the source is the next revision.** A command that moves a live
  change's source branch to a new commit writes revision n + 1: the new head,
  who pushed it and the push that did. The change's head, revision and
  version move with it, so a write of the change formed before the push is
  refused `stale`. A forced update is a revision like any other.
- **Whoever pushes a revision becomes an author**, from the revision they first
  wrote; the change's owner is one from its first. An author's approval of
  their own change counts for nothing.
- **A push that sets the target to exactly the change's head merges it**:
  `merged`, by `push`, at that commit. The head is the one the same push's own
  moves left, so a push of one commit to both the source and the target
  merges the change whatever order the refs were sent in. One record merges at
  most 64 changes, the lowest numbers first; a change past it stays open with
  its head in its target. A push that moves the target past the head rather
  than onto it is no move of the head the push's own apply can see — whether
  one commit holds another is git's to say — so the repository's home finds
  it when it reads the revision's facts and lands the change: `merged`, by
  `push`, at the target's tip it found holding the head ([The home's change
  work](#the-homes-change-work)).
- **Deleting the source branch closes the change**: `closed`, its reason
  `source_deleted` — after the same push's merges, so a push that sets the
  target to the head and deletes the branch leaves the change merged.
  Reopening it needs the branch pushed again.
- **Every change one record moves counts one version**, however many of its
  pushes moved it.
- **A record holds the rows of the changes it moves.** The committer counts
  what a record will write about the changes its pushes move beside the
  record's own rows. A push whose changes would take the record past the rows
  one record may write, beside the pushes batched before it, waits for the
  next record and is decided first in it, the pushes behind it after it; one
  whose changes would not fit even a record of its own is refused
  `too_many_changes` — see [What git prints](#what-git-prints-and-what-it-means).

### Reading changes

The code host answers two reads of changes, each from a data node's own rows
at the read level its caller asks for, as every read of the rows is: stale
unless asked, `session` beside the position a write answered with, so a list
just changed is read back with the change in it, and linearizable for an
answer that holds every record the code log had committed when it was asked.

- **A listing of changes**, newest first, a page at a time. It names a
  **repository**, a **work item** — by its identity across trackers,
  `<backend>:<id>`, such as `native:<task id>` — a **state**, a **reviewer**
  or **what its changes wait on**, and may narrow further by an **owner** (a
  seat by its handle, or a person by the human seat they hold) and by
  whichever of the others it did not name. One that names none of them is
  refused: no index holds every change the company has, and an owner's
  changes are kept by state, so an owner's are listed in a state, or in a
  repository, or on a work item. A repository's changes are listed by
  number; a reviewer's by the change's id, which is minted as the change is
  opened; every other listing in the order the records that opened its
  changes landed on the code log.
- **The changes waiting on a reviewer** are a listing that names a
  `reviewer` — the changes whose ask of that reviewer still stands, a closed
  or merged change's among them, since nothing withdraws an ask when a change
  ends — or `needs=review`, the **live** ones alone, the asker's own where it
  names no reviewer: what waits on your review, read from the asks themselves
  rather than from every change the company holds. A credential bound to no
  seat is nobody's reviewer, and its `needs=review` is refused.
- **The changes waiting on their owner's fix** are `needs=fix`: an
  **owner**'s live changes — the asker's own where it names none — whose
  reviewer's standing verdict asks for changes, or that hold a finding none
  of their authors has answered (see [Reviews](#reviews)). It reads the
  owner's changes in one state, `open` unless it names `draft`. A pushed
  revision answers neither: a request for changes stands until its reviewer
  reviews again, and a finding until an author answers it. A blocker an author
  has answered waits on its raiser, not on the owner, and does not list the
  change here.
- **What waits on you** is `needs=me`: the changes whose ask of your review
  stands, and then your own changes waiting on your fix — one listing, read as
  those two one after the other, a change of both listed once. It names no
  reviewer and no owner, since it is yours.
- **What waits on a person** is `needs=person`: the live changes whose
  [readiness](#merge-readiness) waits on persons alone — an approval only a
  person was asked for, a blocker a person raised and an author answered —
  whoever asks, a seat included. It reads the changes whose stored readiness
  waits on a person, newest first, and keeps those whose readiness the read
  judges itself still does; a change that came to wait on one since its last
  record is listed once its home refreshes it, a minute later at the most.
- A need lists **live** changes alone, and is refused beside a closed or
  merged state.
- **One change**, by its number on its repository — `web!42` — with its row
  (title, state, branches, head and revision, version, owner, the work item it
  carries, when it was opened, closed or merged and how), its description and
  its authors, beside its revisions, newest first, each with the facts its
  home recorded of it, where it has ([Recorded on the
  change](#recorded-on-the-change)); a later page carries its revisions
  alone, after the revision the page before it ended at. A number
  the repository does not hold is "no such change", naming it (`web!43`).
- **A live change's readiness rides it**, on its own read and on every row of
  a listing: whether it may merge, judged in the read's own snapshot against
  the read's chart ([Merge readiness](#merge-readiness)). So a read of
  changes is judged by the chart whoever asks — a person's too — and a data
  node that holds none yet refuses it as a node that is not serving, which
  the next data node answers.

**A listing reads the one index its filters lead with** — the changes whose
stored readiness waits on a person; a reviewer's
standing asks; a work item's changes; an owner's in a state, which is also
what `needs=fix` reads; a repository's in a state; a repository's; the
company's in a state — so a repository's open changes are read without its
closed and merged history, however long that grows. A filter that index does
not hold — a repository beside a work item, an owner or a reviewer, a state
beside a work item or a reviewer, an owner beside a repository with no state,
a work item beside a reviewer, whether a change waits on its owner's fix,
which its conversation's counters say, and whether it waits on a person,
which its readiness says — is tested on each change the page
read, never by reading further: a page is the changes it
**read**, not the changes it kept, so it can be short, or empty with a cursor,
and a reader pages until no cursor comes back. A page reads 50 changes unless
asked for fewer or more, 200 at most; a change's revisions come 200 a page,
1 000 at most.

**Every read of changes is held to the reader's grant** on the change's
repository, as the repository's own reads are ([Who may read and
push](#who-may-read-and-push)):

- A listing that names no repository lists the changes of every repository
  the reader may read — and of an active or archived one only: a deleted
  repository's changes are no work anybody can do until it is restored.
- A listing that names a repository, and a read of one change, are refused
  `not_permitted` where the reader may not read the repository, naming the
  visibility and who does read it — a number in it too, which is never
  answered "no such change" — and are otherwise served whatever the
  repository's state, a deleted one's included, since it was named.
- A seat reading for a colleague is held to both grants, as every code read in
  such a turn is ([Reads made for somebody else](#reads-made-for-somebody-else)).

**Every row a read of changes serves names the cursor that resumes after
it** — a listed change by the walk its listing takes, a revision, a reviewer,
a comment, a review and a finding by its own — and a page's own cursor is its
last row's. So a reader whose answer is smaller than a page — a code tool's,
one tool answer, where a page may be four mebibytes — keeps a page's first
rows, resumes after the last it kept, and reads every row after it once.

**A seat reads changes with its code tools**, and a person's assistant with
the same ones ([The company's code](../guides/tools-and-mcp.md#the-companys-code)):
`code_list_changes` is the listing, and `code_get_change` one change a part at
a time — its summary, its [merge readiness](#merge-readiness), a revision's
files and a file's patch from the base its facts record or, since an earlier
revision, what changed between the two ([What changed between two
revisions](#what-changed-between-two-revisions)), its findings each placed in
the revision asked about ([Where lines are in another
revision](#where-lines-are-in-another-revision)), its reviews and its
comments — each page cut whole-row at one tool answer and resumed after the
last row it kept.

---

## Reviews and findings

What is said ABOUT a change — who is asked to review it, its comments and the
threads they form, its reviews and the findings they raise — is its
**conversation**: rows on the code log like the change itself, written by
records on a subject of the change's own (`crewlet.code.log.change.<id>`),
never on its repository's. A change's lifecycle stays where its pushes are,
in the repository's [committer](#the-committer); a comment or a review shares
nothing with those pushes, and decided in the committer's queue it would wait
behind their batches and contend with every push to the repository.

- **One node writes a change's conversation.** A write is asked of the
  repository's committing data node — the node its pushes are asked of — and
  decided there by the **change writer** beside the committer, in one
  snapshot, as a record of its own; a git node never publishes one. The writer
  takes ONE CHANGE'S WRITES ONE AT A TIME — two published at once would read
  one expectation, and one of them would lose a round to the other — and
  different changes' side by side. A write waits for its change's turn while
  its asker's attempt has room for it, and is refused `busy`, undecided, the
  moment it does not. A node that stops answering is replaced by the
  repository's next data node, under the same operation id, so a write asked
  again is the write that landed, never a second one.
- **A write is decided at or past what its asker read.** The asker names the
  position on the code log it read the change at, and the writer decides only
  once its own copy holds it, or refuses as a node that is behind, which the
  next node answers: a reply to a comment the asker was shown is never refused
  because the writer has not seen the comment yet.
- **A seat and a person's assistant write it through tools** —
  `code_request_review`, `code_review_change`, `code_comment_on_change` and
  `code_resolve_finding` ([The company's
  code](../guides/tools-and-mcp.md#the-companys-code)) — each one write, named
  by one operation and decided at or past the tool's own read of the change. A
  review is ONE record — its verdict, its summary, every finding it raises and
  every finding of the reviewer's own it carries — and the tool anchors each
  finding where its lines were read: in the revision's head on the new side,
  its recorded base on the old, the blob from one read of the revision's diff,
  on a file the revision changes. A reply that also marks its thread resolved
  is the reply, then the thread's state as a second write of the same
  operation.
- **A table is written by one subject's records.** The repository's records
  keep the change's row, its body, its authors and its revisions; the change's
  keep its conversation's tables:

| Table | A row for | What it holds |
|---|---|---|
| `code_conversations` | each change that has been discussed | Its counters — its comments, its reviews, its open findings, its standing blockers and how many reviewers' standing verdict asks for changes — what its comments, reviews and findings weigh, the version and position of its last record, and the version of what its [readiness](#merge-readiness) reads: one more for each ask for review, review, answer to findings and revision's facts |
| `code_reviewers` | each reviewer it has asked | A seat, or a person by the human seat they hold; why — asked by name, or as a [path owner](#recorded-on-the-change) a revision's facts asked — whether the ask stands or was withdrawn — a withdrawn ask is kept, so the change says who was once asked — who asked, nobody for a path owner, and the note they asked with, at most 1 KiB |
| `code_participants` | everybody who takes part in it | Each reviewer asked or reviewing and each author of a comment, by handle and kind, and a reviewer's **standing verdict**: the latest of their reviews that approves or asks for changes, and the revision it judged |
| `code_comments` | each comment | Its thread, its parent or the finding it replies to, who said it — a seat, or a person with the credential they used — the line it points at, whether its thread is resolved and by whom, the turn that said it, and its body, at most 32 KiB |
| `code_reviews` | each review | The revision it judged, who reviewed — a seat, or a person with the credential they used — its verdict, how many findings it raised, the turn and the model that reviewed, and its summary, at most 4 KiB |
| `code_findings` | each finding | The review and the revision that raised it, its raiser, where it points — a file, a side of the revision's diff, its lines, the commit and the blob they were read in — its severity and category, its state, and its last answer: the note, who gave it, the commit it names |
| `code_finding_texts` | each finding | Its title (one line, at most 200 bytes), its body (at most 4 KiB) and the lines it suggests in place of the ones it points at (at most 16 KiB) — a row of its own, so the finding's state is rewritten without its text |
| `code_revision_facts` | each revision its home read | What the revision is against its target, and the path owners it asked ([Recorded on the change](#recorded-on-the-change)) |
| `code_mergeability` | each change its home merged | Whether its head merges into its target, and the readiness its home's last record judged beside it ([Mergeability](#mergeability)) |

### Reading a conversation

A change's conversation is read with the change: one change's read names the
**part** it pages beside it — its revisions, which a read naming no part
pages and whose first page carries the change's detail, its **reviewers**,
its **comments**, its **reviews** or its **findings** — every page held to the
reader's grant on the repository as the change's own rows are ([Reading
changes](#reading-changes)).

- **The detail counts the conversation**: its comments, its reviews, its open
  findings, its standing blockers and its requests for changes, and the
  version and position of its last record — the floor a read of its parts is
  asked at, so a comment just written is read back with the change.
- **Comments come a thread at a time**: a thread's first comment and then its
  replies in the order they were said, threads in the order they started; a
  page holds 50 unless asked for fewer or more, 200 at most, and ends at the
  reply's byte budget first where comments are long. A comment names its
  thread, the root or the finding it replies to, who said it — a seat, or a
  person and the credential they used — the line a thread's first comment
  points at, and whether its thread is resolved and by whom.
- **Reviewers come by handle**, a withdrawn ask among them, 200 a page and
  1 000 at most: who was asked, by whom, with what note, and whether the ask
  stands.
- **Reviews come in the order they were given**, 50 a page and 200 at most:
  the revision each judged, who reviewed, its verdict, its summary and how
  many findings it raised.
- **Findings come by id** — the order the reviews that raised them were given
  — each with its text and its suggestion, its state and its last answer, 50
  a page and 200 at most, ending at the reply's byte budget first where
  suggestions are long.

### Asking for review

An **ask for review** names reviewers to ask, reviewers whose ask is
withdrawn, or both. A seat is named by its handle and a person by the human
seat they hold; which is which is read off the chart.

- **Who may ask:** an author of the change, a principal that reviews the
  repository (`review`, `write` or `maintain` — see [Who may read and
  push](#who-may-read-and-push)), and any person.
- **Who may be asked:** a person, always; a seat only where the chart lets it
  review the repository, and never an author of the change, whose review of
  their own change would count for nothing. A seat that may not is refused
  `not_permitted`, the sentence naming what it holds and what would admit it.
- **A change asks at most 32 reviewers at once.** A withdrawn ask no longer
  counts, and an ask past the bound is refused `too_many_reviewers`.
- **Only a live change is reviewed.** An ask for review of a closed change is
  refused `change_closed`, and of a merged one `change_merged`.
- **An ask that moves nothing changes nothing.** Asking reviewers who are
  asked already, and withdrawing nobody who is, is answered from the rows and
  appends nothing.
- **The chart judges every reviewer, whoever asks.** A node that holds no
  chart yet, or one [behind the company's configuration](#a-seats-push),
  decides no ask for review — and no seat's write of a conversation at all —
  and hands it to the repository's next data node.

### Comments and threads

A **comment** says something on a change, in any state — a change is
discussed after it merges too. It is anybody's who reads the repository, and a
credential bound to no seat says nothing: every comment names who said it.

- **A thread is one level deep.** A comment starts a thread of its own, or
  replies to the first comment of a thread on the same change, or to a
  finding on it, whose thread the finding names; a reply to a reply, to
  another change's comment or finding, or to nothing is refused
  `thread_gone`. A comment's id is derived from its operation id, so a
  comment asked again is the comment that landed.
- **A thread's first comment may point at a line**: a file, a revision of the
  change, a side of its diff and the line, with the blob read there. A
  revision the change has not reached is refused `unknown_revision`.
- **A thread is resolved, or opened again,** by whoever started it, an author
  of the change, a principal that maintains the repository or a person —
  anybody else is refused `not_permitted` — and its first comment names who
  resolved it. A finding's thread is answered by the finding's own answer
  ([Findings](#findings)), never resolved as a comment's is.

### Reviews

A **review** judges one revision of a live change: a **verdict** —
`approve`, `request_changes` or `comment` — a summary of at most 4 KiB, and
the **findings** it raises, at most 64, each saying something about lines of
the change. The review and every finding it raises are ONE record, so a
review is never half there; its id and each finding's are derived from its
operation id, so a review asked again is the review that landed.

- **Who reviews:** a principal that reviews the repository (`review`, `write`
  or `maintain`) and a person — and never an author of the change: the seat or
  person who opened it, and everyone whose push moved its head. An author's
  review of their own change, its approval above all, would count for
  nothing, so it is refused `not_permitted`, as is a seat below `review`, the
  sentence naming what it holds and what would admit it.
- **It judges a revision the change has reached** — one past the change's
  head as the writer reads it is refused `unknown_revision` — and a change
  that is closed or merged is reviewed no further (`change_closed`,
  `change_merged`).
- **A reviewer's standing verdict** is the latest of their reviews that
  approves or asks for changes. A review that only comments judges nothing
  and leaves it as it was, so a comment after a request for changes withdraws
  nothing. The change counts the reviewers whose standing verdict asks for
  changes; a pushed revision answers none of them — a request for changes
  stands until its reviewer reviews again.
- **A review past what one record carries is refused**, before anything is
  read: its findings with their suggestions encode to more than a record keeps
  beside the room it saves for whom the change concerns. Split it into two
  reviews of the same revision, which supersede nothing of each other.

**What a seat's day counts of them.** The node whose committer opened a change
announces a `change_opened` event once, and the node whose change writer landed
a review a `review_submitted` event — after the write is answered, never for a
retry its operation ledger collapsed. Each names the seats it counts on by their
agent ids, and a seat's day of usage counts the changes it opened, the reviews
it gave, and the findings reviews raised on the changes it owns — see
[Budgets and spend § A seat's changes and reviews](../guides/budgets-and-spend.md#a-seats-changes-and-reviews).

### Findings

A **finding** points at lines of one revision — a file, a side of the
revision's diff, its first and last line, the commit and the blob they were
read in — and says how much it weighs (`blocker`, `major`, `minor`, `nit`),
what it is about (`correctness`, `security`, `performance`, `tests`,
`design`, `style`, `docs`), a title of one line, a body, and, where it
suggests one, the lines it would put in their place.

A finding **stands** while it is open, or an author answered it. Who may
answer, and what each answer does:

| Answer | Given by | The finding |
|---|---|---|
| `resolved`, `wont_fix`, `disputed` | an author of the change | Still stands: an author's answer is theirs to give and the raiser's to judge |
| `dismissed` | its raiser, a principal that maintains the repository, or a person | No longer stands, and nothing answers it again |
| `superseded` | the raiser's review of a later revision that does not carry it | No longer stands, and nothing answers it again |

- **Only a dismissal or the raiser's later review clears a blocker.** A
  standing blocker holds its change back whatever its authors answered: a
  blocker resolved, marked won't fix or disputed still counts among the
  change's standing blockers, for its raiser to judge.
- **A review carries what still stands.** A review names the findings of its
  reviewer's own that still stand as it judges its revision — its
  **carries**; a review of a LATER revision supersedes every other standing
  finding its reviewer raised on an earlier one, open or answered alike. A
  review of the same revision supersedes nothing: one reviewer's second pass
  over a revision adds to the first. A review carries its reviewer's own
  findings alone (`not_raiser`), and only ones still standing
  (`finding_gone`).
- **A reviewer keeps at most 1 024 findings standing on a change**, which a
  review that would leave more is refused `too_many_findings`: what one
  review supersedes is part of its one record, and the bound keeps it one
  record however long a reviewer has followed a change.
- **An answer names findings standing on the change** (`finding_gone`
  otherwise), at most 64 at once, each with a note of at most 1 KiB and, where
  an author says a commit answers it, that commit. Resolving, marking won't fix
  or disputing is refused `not_author` to anybody else, and dismissing
  `not_raiser`. An answer that asks for what every finding already says
  changes nothing and appends nothing.

A write of a conversation that is refused appends nothing, and says why in one
sentence:

| Refusal | Means |
|---|---|
| `repo_deleted`, `repo_archived` | The repository is deleted, being purged or gone, or archived and read-only |
| `change_not_found` | The repository holds no such change |
| `not_permitted` | The asker may not make the write, a reviewer asked may not review the change, or an author asked to review their own — the sentence says which, and what would admit it |
| `no_reviewer` | A reviewer asked is no seat of the chart |
| `too_many_reviewers` | The change would ask more than 32 reviewers at once |
| `unknown_revision` | A comment points at, or a review judges, a revision the change has not reached |
| `change_closed`, `change_merged` | An ask for review, or a review, of a change that is not live |
| `thread_gone` | A reply or a resolution names no thread of the change, or a reply no finding of it |
| `not_author` | A finding resolved, marked won't fix or disputed by one who did not write the change |
| `not_raiser` | A finding dismissed by one who neither raised it nor maintains the repository, nor is a person — or carried by a review of one who did not raise it |
| `finding_gone` | A finding named that does not stand on the change: none by that id, or one dismissed or superseded |
| `too_many_findings` | The review would leave its reviewer more than 1 024 findings standing on the change |
| `busy`, `not_applied`, `unavailable` | The writer did not reach the write in time, or the code log refused it or dropped it on every node: nothing was decided, and the write is sent again |

- **A conversation goes with its repository.** A repository's
  [purge](#deletion-and-purge) removes each change's conversation before the
  change's own row, within the purge's bounded steps: a conversation is
  weighed by what its comments, its reviews' summaries and its findings' texts
  hold, read off its counters row, and one heavier than a step is cut by them
  — its comments, oldest thread first, then its findings' texts and its
  reviews. A record about a purged repository's change applies nowhere, as one
  about the repository does.

`crewlet.code.conversation.writes` counts every write of a conversation a
node's change writer was asked to decide, by its write and what became of it
(the [metrics reference](../reference/metrics.md)).

---

## Checks

A repository defines its **checks** — the builds, tests and linters a change
is measured by — in one file at the root of its tree, `.crewlet/checks.yaml`.
A check is a measurement of a commit, so it is defined beside the code it
measures and moves with it: a new test target lands in the change that adds
the tests. But a change is never judged by a definition it brought. The file
that governs a change's checks is the one its **target** branch holds, so a
change that edits `checks.yaml` is checked by the file it edits, never by its
own edit.

Which checks a protected branch **requires** is not the file's to say either:
that is its protection rule's, so a file can never mark its own checks
optional. And where a check may run, how long a company lets one run and
which secrets exist to be handed to one are the company's configuration's:
the file names, the company decides.

### The checks file

```yaml
checks:
  - name: unit                 # required
    run: make test             # required
    timeout_seconds: 600       # required
    paths: ["**/*.go", go.mod] # run only for a change touching one of these
    report:
      junit: out/junit.xml
      sarif: out/lint.sarif
    run_in: container          # or e2b
    checkout: [cmd, internal]  # a cone, or none
    env: [NPM_TOKEN]
  - name: api-shape
    run: ./scripts/api-shape.sh
    timeout_seconds: 120
    checkout: none
```

The file is a mapping holding `checks:` and nothing else — `checks: []`
defines none — and each check is a mapping of these keys and no other:

| Key | Required | What it says |
|---|---|---|
| `name` | yes | The check's name: a lower-case letter or digit, then up to 63 lower-case letters, digits, dots, underscores and hyphens — once in the file. It is the check's key in every row and on the token its box fetches with, so `unit` and `Unit` are never two checks |
| `run` | yes | The command, handed to `sh -c` whole: a line, or a script a block scalar (`run: \|`) spells over several. At most 4 KiB — a longer script belongs in the repository, where it is reviewed as code |
| `timeout_seconds` | yes | How long it may run: a whole number, 1 to 21 600 — six hours, GitHub Actions' per-job ceiling, so no job moved from a pipeline has to be cut. A company holds its checks to its own ceiling at or under that (`code.checks.max_timeout_seconds`) |
| `paths` | no | At most 32 path globs, in the grammar [path owners](#path-owners) use — `*` within a component, `**` for whole components, relative to the repository's root — of which a change must touch one for the check to run on it. None runs it on every change |
| `report` | no | `junit` and `sarif`: the files the check writes its results to, each a path inside its checkout, read once the command ends. With neither, a check is judged by its exit code alone |
| `run_in` | no | `container` — a container on a git node — or `e2b`, a remote box, among the cells the company allows (`code.checks.allow_run_in`); unset is the company's choice (`code.checks.run_in`, see [Check boxes](code-sandbox.md#check-boxes)). **`direct` and `self` are refused by name**: a check runs a change's own code, which `direct` would run as the engine's own user on the engine host, reading what the engine reads, and `self` is a seat's own executor box, which no check has |
| `checkout` | no | What of the tree the check's box checks out: all of it (unset), a **cone** — at most 64 directories, git's sparse checkout, each a clean directory inside the repository — or `none`, for a check that reads what it needs by object id through the API instead of a clone |
| `env` | no | At most 32 variable names, each the key of an entry in the company's check secrets (`code.checks.env`): the secrets this check is given. A check is given none it does not name |

**It is read strictly**, as a config document is, because a check that quietly
does something other than what its file says is worse than one that is
refused: a typo'd `timout_seconds:` ignored would run the check under no
timeout its author believes it has, and a typo'd `paths:` ignored would run it
on every change instead of the few it meant. So the file is refused for:

- a key it does not know, or one written twice — YAML keeps the last of two
  and a person reading the file the first;
- a value of the wrong shape: a number or a boolean where text belongs, which
  YAML reads without a word (`name: 1.10` is the number 1.1; quote it), a
  fraction where a whole number of seconds belongs, a word where a list
  belongs;
- an alias or a merge key (`<<`), which spell a check somewhere other than
  where it stands;
- more than one YAML document, more than 64 checks, or more than 64 KiB;
- every value outside its bound above — a list longer than its bound by its
  length alone, none of its items read, so a file costs whoever reads it no
  more than its bound, whatever it packs into one list.

Every fault is reported, not the first, each at its field and its line, in the
shape a config document's are — the first 128 of them, with one more line
counting the rest, since a 64 KiB file can hold thousands of faulty keys.
`crewlet validate .crewlet/checks.yaml` reads the file as the code host does.
It tells the file apart from either config
tier by a `checks:` beside no key of either tier, whatever else the file
carries — so a typo'd key beside `checks:` is refused at its own line by the
file's strict reading, rather than reported as a document no tier could be
told from — or reads it as one under `-tier checks`
([`crewlet validate`](../reference/cli.md#crewlet-validate)):

```
.crewlet/checks.yaml: 2 checks (unit, api-shape)
```

### What the log records of a check

Each **attempt** of each check of each commit, for each branch it is checked
for, is a row on the code log — `code_checks`, keyed by the repository, the
commit, the **target** branch, the check's name and the attempt — written by
records on a subject of the repository's **checks**
(`crewlet.code.log.checks.<repo>`), never on its repository's own: a check's
records share nothing with the repository's pushes, and decided in its
[committer](#the-committer)'s queue they would wait behind their batches.

The target is part of what an attempt is: a commit is checked by its target's
checks file and required by its target's protection, so one commit checked
for two branches — a head two changes into two branches share, a tip of one
branch a change into another names as its head — is two sets of attempts,
each run under its own branch's definitions. Two changes into one branch with
one head share theirs: the same commit, judged by the same file.

| Record | What it says |
|---|---|
| `check_queued` | An attempt is queued on a commit: what it is for (a change's head, a merge queue's candidate, a protected branch's tip, a bisection), the branch it is checked for, the checks file it was read from and its definition whole — so it runs under the definition it was queued with, whatever the file says by the time a box starts — whether that branch requires it, and why it was queued: dispatched, run again by a seat or a person, queued again after its runner was lost, or reported by a person's own CI. Or the attempt is recorded `skipped`: its change touched none of its `paths` |
| `check_started` | A runner claims the attempt — and, once it has made one, names its box |
| `check_finished` | The attempt ended: its state and why, its exit status, how long it ran, its summary (at most 4 KiB), the last 16 KiB of its output, the failures its reports named (at most 200, weighing at most 64 KiB, the rest counted), the link a person's CI reported, and the log a failed run kept |
| `checks_sweep` | The rows that ended before an instant are removed — the `code-retention` duty's, for a repository whose checks stopped ending, swept back to the 30 days once a row of it outlived them by a day ([How long a check is kept](#how-long-a-check-is-kept)) |
| `check_dispatched` | The home read a target's checks file at its tip — or found none there — and **dispatched** the heads it names under it: their checks queued for that target, the record their dispatch's last; with no heads, a moved target's reading refreshed ([How a head's checks are dispatched](#how-a-heads-checks-are-dispatched)) |

An attempt's **state** is one of seven: `queued`; `running`; and the five
endings — `passed` and `failed`, which measured the commit (a run past its
timeout fails, since the timeout is the check's own), and `errored`
(its runner was lost, its box or its setup failed, or its definition could not
be read — an attempt its home records errored as it queues it, `defs_invalid`,
saying why), `cancelled` and `skipped`, which did not.

**A check's records commute.** They carry no expectation: many runners claim
and end one repository's checks at once, and arbitrating each would put every
one of them behind every other for rows two of them seldom share. So every
data node must hold one row whatever order two records about one attempt reach
it in, and every record's apply is a **join**:

- **the state only rises** — once ended, an attempt is never running again,
  and once running never queued, whatever arrives after;
- **two claims of one attempt keep the earlier** — by its runner's instant,
  then the runner's name — wherever each landed, and the box is the kept
  claim's, named once by its holder;
- **two endings keep a measurement over infrastructure's** — a run that
  failed says more of the commit than one whose runner was lost — then the
  earlier of them, and the log is the kept ending's;
- **a field arrives with the record that carries it**, so a record about an
  attempt no other record has reached yet makes its row with what it carries.

A failed run's **log** is one object in the [object store](object-store.md),
named by a `code_check_logs` row: a live reference, audited and carried by
every backup, since nothing builds it again.

### Who records a check

A check's records are written by the repository's **committing data node** —
the node its pushes are [decided by](#the-committer), the first of the
repository's rendezvous order over the live data nodes — whichever node asks:
the repository's home queuing a change head's checks, or the git node that
runs one claiming it and ending it. **No git node publishes a check's record
itself**: it asks the committing data node, through the estate's
`code.checks`, as a push's receiver asks for a push to be decided. That node's
**checks writer** — beside its committer, sharing none of its queue — takes
one repository's check writes **one at a time**, each decided against the
rows every earlier one left and published once the one before it applied
there, so:

- **a second claim of an attempt is refused before it appends**: a runner
  asking for an attempt another runner holds, or one that has ended, is told
  `claimed` with the holder's name, appends nothing, and starts no box. The
  holder's own claim asked again changes nothing, and its record naming the
  box it made sets the box once;
- **an attempt follows the one before it, ended**: the home's dispatch queues
  a commit's first attempt, and one already queued — a second dispatch of the
  same head — is answered from its row, queuing nothing; the next attempt is
  queued only once the one before it ended (`not_finished` before), and none
  passes the tenth (`attempts_exhausted`). The home's dispatch queues a next
  attempt only after one a dispatch errored for a checks file that did not
  run — nothing of the commit was measured — and is refused `rerun_genuine`
  after any other ending;
- **the home queues again only what its infrastructure ended** — an attempt
  whose runner was lost or whose box or setup failed — and only while that
  attempt is below the third: a dead runner's check runs at most three
  attempts (`attempts_exhausted` past it), and an attempt that passed, failed,
  or ended any other way is never queued again by the home (`rerun_genuine`);
- **an ending the row would not keep appends nothing**: an infrastructure
  ending after a measurement — a lost runner reported once its run had already
  failed — is answered from the row as it stands.

Where two nodes both believe they decide for a repository — the committing
node gone quiet, its runner-up asked — each may append, and the
[join](#what-the-log-records-of-a-check) converges what they wrote: two claims
keep one runner's, and the runner whose claim the row does not keep, reading
the row back, makes no box. A request carries the floor its asker read at, so
a node deciding it has applied what the asker saw first or says it is behind,
and the asker asks the next node.

Each write is asked under an **operation id** its asker mints once, so a write
asked again — of the runner-up after the committing node went silent, or after
an answer was lost — is the same write, collapsed onto the record that landed.

An ending that applied and that its attempt's row keeps is announced **once**,
by the node that wrote it, as a `code_check_finished` event in that node's
[event log](event-system.md) — never by the applier every data node runs,
which would record it once a node — naming the attempt, what it was for, how
it ended and where it ran. It is kept out of the activity feed: every push of
every change ends each check its file defines, faster than a person reads a
feed, and an attempt's ending is read where it is asked. An ending that
**replaces** one the row already kept — a runner that was only slow, ending its
attempt after the home declared it lost — is announced naming the ending it
replaced (`replaces`), and counted under it: a correction of the attempt's
ending, never a second one, so each attempt is counted once.

A queued attempt that waits for a runner longer than its **own**
`timeout_seconds` raises `code_checks_backlog` ([alarms](../reference/alarms.md))
— each attempt judged at the time its author gave its check to run, never at
one threshold for every check, so a long check's ordinary queue raises
nothing and a one-minute lint stuck for an hour does. Every data node measures
it from the rows, so it stands even where no git node runs checks at all,
reading the queued attempts in the order they fell **overdue** — each
attempt's queued instant plus its own timeout, which its row carries — so a
lint stuck behind a thousand attempts of a long check is found as soon as it
is overdue, and every attempt the reading counts is one past its time. It
counts up to 1 024 and says there are more past that. A **deleted**
repository's queued attempts are not counted: every write of its checks is
refused — a runner's claim and the home's cancel alike — so they wait for its
purge to remove them, and no runner was ever going to claim them.

The writer counts what it decided on three instruments: the attempts it
queued (`crewlet.code.checks.queued`, by purpose), the endings the rows kept
(`crewlet.code.checks.finished`, by state, placement and the ending each
replaced — `none` for an attempt's first) and every claim's
outcome (`crewlet.code.checks.claims`) — `claimed` is git nodes racing for one
attempt, each losing a box it never made, and `lost` above zero is two nodes
writing one repository's checks at once ([metrics](../reference/metrics.md)).

### How a head's checks are dispatched

A change's head is checked by its **target's** checks file, read at the
target's tip — never the head's own, so no change narrows the checks that
judge it. Its checks are queued by its repository's
[home git node](#the-home-git-node) — the same loop that reads its facts and
merges it ([The home's change work](#the-homes-change-work)), working a second
worker, `checks`, from the rows. The home reads `.crewlet/checks.yaml` at the
tip through the git reads (`checks_file`, the engine's own read of the file
at one commit, whole), queues every check it defines at the head for that
target, and then records that it did: a `check_dispatched` record naming the
target, the tip it read, the file it found there — or none — and the heads it
dispatched under it. Two tables keep what those records say: each target's
checks file as the home last read it, and the file each head was dispatched
under.

What a dispatch queues:

- **every check the file defines that the head holds no attempt of** for its
  target. A check with `paths` is queued where the change touches a path one
  of its globs matches, judged against what the head touches against that
  tip, and recorded **skipped** where it touches none; one with no `paths`,
  or a change touching more paths than a list holds, runs. A check the
  target's protection rule requires is queued marked required;
- where the file is **none that runs** — unparsable, or something at its
  path that is no file — every name the rule requires is recorded
  **errored**, `defs_invalid`, the file's refusal its summary, and nothing
  else is queued, since nothing else of the file is known. Once the target's
  file runs again, the next dispatch of the head queues each of those checks
  as its next attempt, under the mended file;
- and before it queues a head's checks, the home ends every attempt of the
  change still queued or running at a head it moved on from, or for a target
  it was retargeted from — `cancelled`, `superseded` — unless a live change
  into that attempt's target heads its commit and shares it.

The dispatch record is the **last**: a dispatch cut short leaves the head
listed, and the next turn queues what is missing — every queue that landed
already answered from its row — before recording it. The heads one turn
dispatches under one target's tip share a record, up to 500 of them.

The home's work list is read from those rows alone. A live change's head is
due a dispatch while no dispatch of it for its target names the file the
target's reading holds — a head never dispatched, a change retargeted, a
target whose checks file changed since — and a head whose target defines no
checks at all leaves the list after one record, as one whose target defines
many does: the list never asks whether an attempt exists, which a repository
that has not adopted checks never has. A target is due a reading again once
its branch has moved past the tip it was read at and that reading is older
than the home's refresh — ten minutes — so a target pushed to on every
landing is read at the home's pace, and only a file that changed makes its
waiting heads due — which is how a check added to a target's file reaches the
changes waiting on it. A new change needs no refresh: it is dispatched at its
target's tip as it is listed, and its record moves the target's reading
there. A name the target's rule requires that its file does not define leaves
the merge gate waiting on it, until the file defines it. Two homes recording
one target's reading at once, across a failover, leave one row: each row
keeps the reading of the later tip.

A head is due a **requeue** too, where the latest attempt of a check at it
for its target ended `errored` for its infrastructure — its runner lost, its
box or its setup failed — below the third attempt
(`code.AutoRequeueAttempts`): the same check queued again as the next
attempt, under the definition it ran under. Past the third, only a rerun runs
it again. A record on a repository's **checks** subject nudges its home as a
record on the repository's own does, so a requeue follows an ending at once
rather than at the next pass.

**A finished check refreshes the change's readiness.** The checks of a
change's head are an input of its [merge readiness](#merge-readiness), and a
record on the checks subject moves neither version a stored readiness is
judged at. So the stored readiness also remembers the position of the latest
record that queued or ended an attempt at the head for its target as its
writer read it, and an attempt queued, requeued or ended past it makes the
change due a **refresh** from its home: recorded above the stored row, never
refused `superseded` as one judged at its inputs already is. The
`mergeability` record that stores it names the repository's checks in its
scope, so a node holding a check record it could not decode decides none.

### Reading a repository's checks

A repository's checks are read through `code.read`, held to the reader's
grant on the repository as every other read of it is:

- **`checks`**: one repository's attempts, newest ended first and every
  attempt not yet ended after them; one change's — the attempts that judge
  it: its head's, or a commit it names, checked for its own target, by name
  and attempt; one commit's, for every branch it was checked for; or — naming
  no repository — every repository's the reader may read whose attempts ended
  since a moment, newest ended first. Each is a page of at most 200 attempts
  (50 unless asked), resuming exactly where the page before ended;
- **`check`**: one attempt whole, named by its commit, its target branch, its
  check and its number — its row, the definition it ran under, the last of
  its output, the failures its ending named and its kept log;
- **`checks_due`**: the engine's own, which no person or seat may make — the
  work the repository's home polls for: the live changes whose head has no
  **dispatch** for its target naming the checks file the target's reading
  holds, or owes a **requeue** of an attempt its infrastructure ended — each
  with what it owes — and the targets whose branch moved past the tip their
  checks file was read at, once that reading is older than the home's refresh
  ([How a head's checks are dispatched](#how-a-heads-checks-are-dispatched));
- **`check_box`**: the engine's own too — the one attempt a check's box's
  token names, by its key, as its row stands, which the box's admission reads
  on every request ([Check boxes](#check-boxes)). A person's or a seat's read
  naming an attempt's key is refused, so no read that forgot whom it asks for
  is answered held to no grant;
- **`checks_queued`**: the engine's own — a git node's [claim
  walk](#who-runs-a-check): every repository's queued attempts, oldest queued
  first, each with the definition it was queued under and its repository's
  name, a page at a time;
- **`checks_running`**: the engine's own — a repository's home's sweep of a
  runner that is gone: the repositories holding a running attempt, or one
  repository's running attempts.

### Check boxes

A check runs in a **box** of its own — a container on a git node, or a remote
box, with no coding agent in it ([Code Sandbox → Check
boxes](code-sandbox.md#check-boxes)) — which reaches the code host as a
[coding box](#coding-boxes) does, at
`/box/git/<name>.git` on a node whose roles name `git` (on the public listener
where one is set), with a credential of its own and nobody's else: git's Basic
user name `check` and, as the password, a token naming **one attempt** of one
check of one commit and the name of the repository its box was given, signed
under the fleet's keyring in the coding boxes' key domain (`crewlet.git.v1`). A
run's token and a check's are told apart by what each names, and each is
valid under its own user name alone: a check's token presented as `run`, or a
run's as `check`, is refused as no token.

**The attempt's row is the authority, the token its backstop.** A token is
valid for its check's timeout and ten minutes more, the box's setup
(`code.CheckBoxGrace`, a box's setup step's own budget); what admits the box
is its attempt's row, read on every request, so an attempt that has ended
admits its box to nothing more from the moment its row says so, whatever its
token's expiry.

**A check's box fetches one commit, and does nothing else.** It fetches over
git's protocol v2, which every git a box runs speaks by default, and is shown
**no ref**: its advertisement is v2's, which names none, and an `ls-refs` — a
`git ls-remote` — is answered empty. It fetches its commit by id, which
protocol v2 serves of any object the repository holds:

```sh
git init --quiet check
git -C check remote add origin https://hooks.example.com/box/git/web.git
git -C check fetch --quiet --depth=1 --filter=blob:none origin <commit>
git -C check checkout --quiet --detach <commit>
```

— the code host its remote and so its promisor, which the checkout fetches
the files it needs from, every request admitted again by the attempt's row.

A push is refused, and so is a request in protocol v0, which fetches only what
a ref names.

**The admission**, in this order, each refusal in the one line git prints:

| Step | Refused | Answer |
|---|---|---|
| The credential | none presented (git's first request never carries one), a user name other than `run` and `check`, a token this fleet did not sign for the user name presenting it | `401`, a challenge whose sentence names the box's token, never an API token |
| A fetch | a push | `403 read_only` |
| | a request in protocol v0 | `403 protocol_v0` |
| Its requests in flight | sixteen of the attempt's git requests already in flight on this node | `503 run_busy` with `Retry-After` |
| The name given | a repository other than the one the token names | `403 not_granted` — before anything of the code log is read, so a box guessing names costs the log nothing |
| The attempt | its row cannot be read | `503` with `Retry-After` |
| | the rows hold no such attempt — read again through the code log's barrier first, since a copy behind the log may not hold its queue yet, while the check's re-syncs last | `403 check_unknown` |
| | the attempt has ended | `403 check_ended`, decided on whatever copy shows it: a check's state only rises, so an ending is for good, and a box still asking past its attempt's end costs the log no barrier |
| The repository | not found, deleted or purging — decided through the barrier, as a coding box's is; a name a rename left redirecting is served for the repository it redirects to | as a person's |
| | a name now held by another repository than the attempt's | `403 not_granted` |

**Every read is at the check's session**: the position its attempt was queued
at, which its commit's push precedes, so the copy serving the box holds its
commit and no read of it pays a barrier. **Its transfers are a run's**, in the
class that holds at most half the [transfer lane](#fair-share), keyed by its
**repository** (`check:<id>`), so the checks one push fans out — a check for
every name its file defines — take one turn of the class between them where a
coding run takes one; its spool is every box's one credential, `runs`. And
**it acts as nobody**: a check's box runs its repository's own checks file, so
its fetch of a `visibility: unit` repository is no principal's read — its
attempt's row, naming its commit and who queued it, is its record ([Who read
it](#who-read-it)). A refusal is logged as a coding box's is
(`code_box_challenged`, `code_box_refused`, `code_box_unavailable`), naming the
attempt and the repository, never the token.

**Its re-syncs and requests in flight are bounded as a coding run's**
([a box's re-syncs](#a-boxs-re-syncs-and-requests-in-flight)): a check's file
runs code nobody vouches for. Every read of its request through the code
log's barrier — a want its check's session lacks, a request past its bound,
the repository resolved again, and the second read of an attempt the stale
copy does not hold — is paid from its **attempt's** bucket on its
**repository's checks**, 16 at once and one every 11.5 minutes after, and past
it is answered at its session saying so: `fatal: remote error: <oid> is not in
<repo> at the position this check was queued at; a check's box fetches the one
commit it checks (not re-judged at linearizable: this check's re-syncs are
spent until <instant>)` — `its repository's checks'` where that bucket refused
it — or `403 check_unknown` with the same clause for an attempt the stale copy
does not hold. Keyed by the repository, the checks one push fans out share one
term, as they share one turn of the transfer lane. At most **16** of an
attempt's git requests are in flight on a node (`503 run_busy`), and its reads
at `/box/code/` pay the same attempt's bucket at the git node that answers
them: a node keeps one set of buckets for its git and its reads alike, so an
attempt has one burst on a node, not one for each, and its reads past it are
counted in `crewlet.code.box.resyncs` with its git's.

**Reading a commit without a clone.** A check whose file says `checkout: none`
reads what it needs of its commit by object id at `/box/code/<name>/…`, with
the credential its git presents, admitted as its git is:

| Route | Reads |
|---|---|
| `GET /box/code/<name>/tree` | A directory's listing: `path` (or `path_b64`, a path's bytes in base64 where they are no UTF-8), `recursive`, `cursor` |
| `GET /box/code/<name>/blob` | A file's lines: `path` (or `path_b64`), `start_line`, `end_line`, `cursor` |
| `GET /box/code/<name>/diff` | The diff from the base its attempt was queued with to its commit: its stat, narrowed by `paths` (repeated), or one file's patch, `file` (or `file_b64`); `renames`; `cursor` |

```sh
curl -fsS -u "check:<its token>" "https://hooks.example.com/box/code/web/blob?path=go.mod"
```

**Only that commit.** No read takes a revision — a tree and a file are read at
the attempt's commit, a diff from the attempt's base to it — and a parameter
no read takes is refused `400 bad_params`, never ignored: a box that asked for
`rev=` and was answered its own commit would read the wrong code with nothing
saying so. The read is asked of the repository's home git node as a
[server-side read](#server-side-reads) the engine makes for the check, held to
no grant, recorded as nobody's read, and held by the git node to that commit
again. Its answer is the read's page — a tree's listing, a file's lines, a
diff's stat or one file's patch — with the repository's name, the commit, the
base a diff was read from and the next page's cursor, and none of the push
provenance a seat's read carries; a refusal of the read itself is the API's
envelope (`404 not_found` for a path the commit does not hold, `400
bad_params` naming the parameter, `503 unavailable`), while the admission's are
the one line its git is answered in. Where a node serves none of the code host
now, a check's box is told why only once its token verifies, on both prefixes,
as a [coding box](#coding-boxes) is.

### Who runs a check

**Every git node runs checks.** Each walks the queued attempts, oldest queued
first, and ranks each by its repository's **git order** — every live git node
in the repository's rendezvous order, its [home](#the-home-git-node) first:
the node at rank *r* claims an attempt only once it has waited *r* × 2 seconds
since it was queued. So the home claims first, where the repository's warm
copy is, and the runner-up two seconds later only if the home did not — its
container cells full, or none — and so on down the order. Every node computes
the same order from the same set of git nodes; there is no lease and no
coordinator. A node walks the queue when a check record lands on the code log,
at most once a second, and every fifteen seconds whatever happens.

**The claim comes before the box.** A runner claims an attempt — a
`check_started` record naming itself, decided by the repository's checks
writer ([Who records a check](#who-records-a-check)) — and makes a box only
where its claim holds: a second claim of an attempt another runner holds is
refused before it appends, and a runner whose claim the row does not keep
reads that back and makes nothing. A lost race costs a record, never a box.
With its box made, the holder names the box on the attempt.

**Container cells are bounded by the node's cores.** A `container` check is a
container on the git node's own machine, so a node runs at most
`git.max_checks` of them at once — unset, a quarter of its cores and at least
one; `0`, none ([Deployment](../guides/deployment.md#running-checks-on-git-nodes)).
A node with no free container cell claims no container check, and the next node
of the attempt's order claims it two seconds later. An `e2b` check runs on
another machine and takes none of them.

**The run.** The box is a [check box](code-sandbox.md#check-boxes): no coding
agent, no model's sign-in, none of `providers.sandbox.setup`; exactly the
company's check secrets its file names, what the check is, and the code host's
base and its check's token, with no commit identity. It is made — its git's
floor checked and its one commit checked out — within ten minutes of the claim
(`code.CheckBoxGrace`); then its command runs in its checkout, detached, and
every fifteen seconds the runner asks whether it still runs, refreshes the
box's lifetime, and reads the attempt's row:

| What happens | How the attempt ends |
|---|---|
| The command exits 0 | `passed`, its summary alone |
| The command exits non-zero | `failed`, with the failures its reports name, the last 16 KiB of its output and its kept log |
| The command runs past its timeout — the file's `timeout_seconds`, held to `code.checks.max_timeout_seconds`, from the command's start | `failed` `timed_out`, with its tail and its log, its box closed: the timeout is the check's own, so running past it measured the commit |
| The box could not be made, or stopped answering for four polls in a row | `errored` `box_failed` |
| A setup step failed — the git floor, the checkout — or a named secret resolved to nothing | `errored` `setup_failed` |
| The file names a cell the company does not allow, or a secret it does not declare | `errored` `defs_invalid`, with no box |
| The row ended meanwhile — its head moved on, a person stopped it, the home swept it | nothing more: its box is closed |

The box is closed on every path. **A check spends no tokens**: no model runs
in its box, and what it costs is its box's time, which its row records as its
duration. The runner counts what it holds on `crewlet.code.checks.running` and
how long each took on `crewlet.code.checks.duration`, by placement
([metrics](../reference/metrics.md)).

**A runner that is gone.** A runner that dies holding attempts leaves their
rows running. The repository's **home** sweeps them, at every pass of its
loop: a running attempt whose runner has **left the repository's git order** —
a node that left the fleet — or whose claim is older than its check's timeout,
the ten minutes of setup and a minute more — a node that restarted mid-run and
so is in the order again holding nothing — is ended `errored` `runner_lost`,
and its box killed where it is an `e2b` one; a container box on a node that
left is that node's own container runtime's to reclaim. The home queues such
an attempt again, so **a dead runner's check runs at most three attempts**. A
runner that was only slow and ends its attempt after the sweep replaces
`runner_lost` with its measurement. A runner that **stops** — its node
draining — ends each attempt it holds `runner_lost` itself, so it is queued
again at once.

**A node without `CREWLET_BOX_GIT_URL` runs no check**: a check's box clones
its commit from the code host at that address, and reads it without a clone at
its `/box/code` sibling — so the address is the public listener with
`/box/git` on the end. Such a node says so once, as `code_checks_not_run`, and
leaves the fleet's checks to its other git nodes; a fleet where none runs them
raises `code_checks_backlog`.

### How long a check is kept

A check's rows are kept **30 days** after it ended: past that, a check is one
nobody reads — a change merged weeks ago, a tip its branch has long moved
past.

- **Every ending removes its own repository's history**: the rows that ended
  before its instant less the 30 days, oldest first, at most 500 of them and
  about a mebibyte — so a repository whose checks keep ending keeps a month of
  them by itself, and no other repository's rows are touched.
- **A repository whose checks stopped ending is swept** by the
  `code-retention` duty: each pass lists the repositories holding a row that
  ended more than **31 days** ago — the 30 days and a day's grace — and
  appends a `checks_sweep` on each one's checks subject removing every row of
  it past the 30 days, which every data node applies as an ending's removal
  ([Deletion and purge](#deletion-and-purge)). A repository whose checks end
  more often than once a day removes such rows itself first, each ending
  taking up to 500 of them, so the duty does not repeat what its endings do;
  an idle one keeps its checks 30 to 31 days, and is swept at most once a
  day.
- **A check queued and never ended is kept** until it ends, or its repository
  is purged.
- **A repository's purge takes its checks and their logs** with the rest of
  it, and a check record about a purged repository — a runner ending a check
  its repository's purge outran — writes nothing on any node.

---

## Retention and history

Six horizons, each kept by records that apply it inside their own
repository, never by a node's own sweep:

- **Push history** — a push row is kept **181 days**, the same horizon as the
  company's [usage history](../guides/retention.md), so "what happened over
  the last half year" has one answer across the engine. A row names who
  pushed — a person's credential and the seat they hold, a seat by its
  handle, or the engine on its own account (`system`, for [the ref
  lifecycle](#the-ref-lifecycle)'s deletions) — how the push's pack was built
  (`git`, sent by a client's git, `engine`, built by the engine itself,
  `merge`, landed by a merge, or `expiry`, the ref lifecycle's deletion of
  idle seat branches), and for a seat's push the turn that
  made it, the work item that turn was charged to and the model it ran on —
  and for a push the seat's coding box made, the coding run that pushed and
  the coding agent it ran, which is how the engine reads back what a run
  delivered when it collects the run: each stamped by the engine from the
  call or the run that made the push, never read from a commit, whose author,
  committer and trailers whoever made the commit writes. A row names every
  ref its push named, refused ones too. A ref the
  engine refused for being longer than 255 bytes, or for not being UTF-8, is
  kept as much of it as fits in 255 bytes beside its length and a digest of
  the whole
  (`refs/heads/x… [60013 bytes, sha256 <32 hex>]`), so a ref nothing can ever
  name costs every node's history no more than one that could. The history
  is kept, listed and forgotten in the order the pushes LANDED on the code
  log, newest first in its listing and oldest first as it ages out — never
  by the instant on each record, which is the stream leader's clock and can
  step back when the leader changes — so a push stamped earlier than the one
  before it waits for that one, at most the brokers' clock skew past its own
  181 days, and every node forgets the same pushes.
- **Ref tombstones** — a deleted ref is kept as a tombstone for **48 hours**,
  so a node bringing a repository up to date learns of the deletion — or
  sooner, oldest first, once the repository holds **100 000** of them. Each
  push's record always has room to forget as many old ones as it deletes,
  so a repository at that bound keeps every new deletion and forgets the
  oldest. A node last brought up to date before a forgotten deletion reads
  the repository whole: it re-reads the refs and keeps every pack it holds,
  and downloads nothing again.
- **Ref drops** — a push that force-moves a branch, or deletes a branch or a
  tag, records the object the ref held, since no ref names it any more: one
  row an object, naming the ref, the object and when the push landed. It is
  kept **14 days**, git's own `gc.pruneExpire` default, because dropping a
  repository's unreachable objects keeps what such a push left for that
  long: history force-pushed away or deleted stays fetchable by its id for
  two weeks after the push. The repository's later pushes forget the rows
  past that, oldest first, with whatever room each push's record leaves once
  its tombstones are forgotten. That is room for at least as many as the
  push records, except behind the busiest records: two pushes of forced
  updates sharing one record (about 1 900 short), or a thousand deletions or
  more while older deletions' tombstones are being forgotten too (from four
  short, for one push of a thousand, to about 1 250). What a push could not
  forget waits for pushes with room, up to about 4 000 rows a push, so the
  rows past two weeks pile up while a repository's pushes stay that busy and
  go once they do not. A fast-forward or a create leaves nothing behind and
  records nothing. The committer counts these rows in the record it forms,
  so a record holds one push of a thousand forced updates or deletions where
  it holds three of a thousand fast-forwards.
- **Check history** — a check's rows are kept 30 days after it ended, removed
  by each later ending of its repository and, where its checks stopped
  ending, by the `code-retention` duty's sweep record once a row has outlived
  the 30 days by a day ([How long a check is
  kept](#how-long-a-check-is-kept)).
- **Queue history** — a merge queue's ended entries and a protected branch's
  landings below its green row are kept 30 days, removed by each later train,
  ejection and landing of the repository's queues and, where they stopped, by
  the `code-retention` duty's `queue_sweep` record once a row has outlived the
  30 days by a day ([The merge queue](#the-merge-queue)).
- **A purge** — below.

A pack a compaction retires is kept a day as a retired row, which the
`code-retention` duty then removes ([below](#deletion-and-purge)). Until then
the row is how a node bringing the repository up to date learns of the
retirement; a node last brought up to date before its removal reads the
repository whole at its next sync, as after a forgotten deletion, and
downloads nothing again.

### Deletion and purge

A repository removed from `code.repos` is **deleted**, softly: its rows and
packs stay, and declaring it again under its name restores it. Once it has been
deleted longer than **`code.deleted_retention_days`** (1 to 365 days; unset is
7, the trim's default `min_age`, so a deletion is undone from the same window
the log can replay), the fleet-singleton `code-retention` duty purges it (see
[Singleton duties](seat-ownership.md#singleton-duties)):

- every 15 minutes, it first removes the rows of the packs a compaction
  retired more than a day ago, so the collector can take their objects (see
  [Live, retired and derived references](object-store.md#live-retired-and-derived-references)),
  then the check rows that ended more than 30 days ago in a repository whose
  checks stopped ending — one holding a row that ended more than 31 days ago
  ([How long a check is kept](#how-long-a-check-is-kept)) —
  then finishes every purge already begun — whatever the retention, since a
  purge has no undo — then purges the repositories deleted longest ago first,
  then releases every renamed repository's old name whose redirect has ended,
  and last deletes every scratch repository idle past
  `code.scratch.idle_days` that no live turn holds
  ([A scratch repository's idle expiry](#a-scratch-repositorys-idle-expiry));
- each purge is a sequence of bounded **steps**, each one record removing a
  bounded part of the repository's rows — its push history, its refs, its
  packs and the ones a compaction retired, its
  [ref drops](#retention-and-history), then its [changes](#changes), each
  whole: a change's revisions, authors, [conversation](#reviews-and-findings)
  and description, then its row — then its [checks](#checks), each with its
  log — and the last leaves only a **deletion marker**, which drops every
  later record about the repository on every node, for ever — on the
  repository, on its changes and on its checks alike
  ([the deletion gate](../guides/replication.md#applied-is-not-permanent)).
  A step is bounded by how many rows it removes and by what its widest rows
  weigh: the push records, the change descriptions, the comments and the
  checks' definitions and output it removes together stop at about 32 MiB, so
  a repository with long descriptions, long discussions or many failed checks
  takes more steps rather than longer ones;
- every data node applies every step on its one write lane, so the duty paces
  itself: after each step it waits twice the step's duration, a third of the
  lane, and a pass stops after a minute of steps and the next resumes from
  the rows;
- once the purge has applied on the duty holder's own rows, the name is free,
  and a name the current configuration still declares — re-added while it
  purged, which the reconcile logs as `code_repo_awaits_purge` — is created
  again at once;
- a purge **begins only on a node that serves the fleet's current
  configuration** — the revision the [activation pointer](control-plane.md)
  names, applied on that node. The duty purges by the retention its own node
  serves, and a purge has no undo, so a revision that lengthens
  `code.deleted_retention_days` to save a deleted repository is never defeated
  by a holder that failed to apply it: a node behind the fleet's revision, or
  one whose fleet has activated nothing yet, declines the duty and gives it
  back, logging `code_retention_duty_declined` with the reason and both
  epochs, and a node that has applied the revision takes it at its next tick.
  A pass that outlasts a renewal of the duty's lease is asked the same before
  the renewal, and stops there if its node has fallen behind. Being behind is
  a fault only once the lag is
  [confirmed](control-plane.md#where-the-gate-sits) — an apply of the
  revision failed on the node, or it has been behind longer than propagation
  explains: a node merely applying an activation, or starting up before its
  reconciler has ticked, declines quietly and asks again a reconcile interval
  (15 seconds) later, so a restart or a configuration write costs a purge
  seconds. Declining costs a purge at most one tick (15 minutes) on a fleet
  with a current node; on a fleet whose only worker data node is stuck, every
  purge waits for its apply to succeed.

A push to a repository that is not there to take it is refused
`repo_deleted`, and what follows the code says which of four states the
committer found, because each has its own remedy:

| State | What the push is told |
|---|---|
| deleted | declaring it again in `code.repos` restores it as it was — or, for a repository a [tool made](#repositories-made-by-tools), a restore through the code tools — until its purge, which may begin from the instant named: the deletion plus `code.deleted_retention_days` on the committer's current revision — the earliest the duty begins it under that revision |
| being purged | the purge cannot be undone; once it completes, declaring the name — or creating it again, for one a tool made — creates a new, empty repository |
| purged | it holds nothing; declaring its name creates a new, empty repository |
| an id no repository had | it does not exist |

The packs a purged repository named, the ones it had retired with them, are no
longer named by any row, so the
[object collector](object-store.md#collection-and-audit) deletes them a day
later. The code log itself is trimmed like every other log (see
[Retention](../guides/retention.md)), and a node whose copy of it is out of
service declines the duty rather than purge from rows the fleet may not agree
with.

### The ref lifecycle

A seat that runs coding work leaves a branch in its own namespace,
`refs/heads/seats/<handle>/`, for every piece of work it does, and nobody else
deletes them. So a branch **a seat set in its own namespace and nobody has set
for 14 days** is **deleted** by the `code-retention` duty — its last step, after
the purges, the released names and the scratch expiry. Fourteen days is past a
working fortnight and a person's holiday, and short enough that a seat running
dozens of coding runs a week does not fill its repositories with hundreds of
dead branches against the [limit](#limits) on refs.

Only such a branch is ever deleted: a branch a person pushed last, even in a
seat's namespace and even to the same commit, a tag, `main`, a release branch
and a seat's push outside its own namespace are never touched. Neither is a
branch of an archived repository, which is read-only; a deleted repository's
are its purge's.

A branch is **kept** while anything still depends on it:

- **a live turn holds it** — one that read or wrote it through a code tool,
  one a coding run's launch resolved, or one the run's box pushed — held as
  a [scratch repository](#a-scratch-repositorys-idle-expiry) is, in the
  running turn's memory and on a parked run's row, by the same reader and in
  the same order: the pass fixes its cutoff and reads its page first, and only
  then the holds. A turn that holds a branch idle 334 hours or more (14 days
  less two hours, two nodes' clocks disagreeing by an hour each) also records
  the hold on the repository, so a deletion listed before the turn's read is
  ordered against it: recorded first, the deletion keeps the branch; deleted
  first, the turn is told the branch was deleted, by a push or by this
  lifecycle, and at which commit to restore it. **A hold that cannot be read
  expires nothing**: the pass says so (`expiry_unread`) and logs
  `code_ref_expiry_skipped` at warn;
- it is its repository's **default branch**: deleted, the repository's HEAD
  would name nothing and every clone would check nothing out;
- it is the **source or the target of a live change**: deleting a source
  closes its change, and a review may take longer than a fortnight;
- a [protection rule](#protection-rules) matches it — every rule takes a
  person's direct push alone (`persons`, unset reads it) or nobody's (`none`),
  and the lifecycle is no person.

The deletion is a **push**, on the engine's own account — no credential, no
seat, no run — decided by the repository's [committer](#the-committer) like any
other, so a push or a hold that lands first wins and the branch is kept
(`stale`, with the reason). It moves no activity clock: a scratch repository's
idle time and a repository's last push stay where its last real push left
them. Its row in the push history — `via` `expiry`, `actor_kind` `system` —
keeps every branch it deleted beside the commit it was at, so a branch is
**restored** with `code_create_branch` at that commit for as long as the push
history keeps the row (181 days) and the repository still holds the commit,
which a deletion keeps for its [prune grace](#retention-and-history) at least.

Each push deletes up to 1 000 branches, fewer where their names are long
enough to fill a push's record, and a repository with more idle branches is
several pushes. A committer that answers `busy` — persons' and seats' pushes
queued — ends the step: the lifecycle yields, and the next pass resumes.

A pass's `code_retention_pass` line counts `refs_expired`, `refs_kept` (proposed
and found no longer idle by the committer), `refs_held` (kept for a live
turn's hold) and names `expiry_failed` and `expiry_unread`; each push that
deleted something logs `code_refs_expired`.

**Before the bound refuses a create, an alarm says so.** The lifecycle makes
room only from branches nobody set for 14 days, so a repository setting refs
faster than they go idle — or holding persons' branches and tags, which never
expire — can still reach the [limit](#limits) of 100 000 live refs, after which
every push creating a ref there is refused `repo_refs`. A repository whose live
refs and the refs it set in the last 14 days together reach 100 000 reaches
the bound within 14 days at that rate — or holds it already, refusing every
create — and none of those recent refs is the lifecycle's to delete, so every
data node raises [`code_refs_near_limit`](../reference/alarms.md) while one is:
it names how many repositories have reached that point and the one with the
most, with its live refs
and how many it set in the window. An archived repository, which takes no push,
and a deleted ref are never counted. It is measured from the data node's own
rows on the trim's quarter-hour tick — a walk of the active repositories in
name order, and a count of the refs set in the window for each one holding
at least 50 000 live refs, the only ones that can reach the point — and a node
that cannot read them logs `code_refs_near_limit_unread` and raises nothing
until the next tick reads them. Delete the branches and tags the repository no
longer needs; one that needs more than 100 000 live refs is past what the code
host serves, and is split.

---

## The cache on a git node

A repository lives as rows on the code log and as pack bundles in the object
store. A node that serves git keeps a **cache** of each repository it is asked
for: an ordinary bare repository in a directory of the node's own, from which
git's own `upload-pack` answers clones and fetches. The cache is disposable:
nothing in it is the only copy of anything, and deleting it costs nothing but
the time to download it again. The engine's [backup](../guides/backup.md)
never copies it, and its directory carries a
[`CACHEDIR.TAG`](https://bford.info/cachedir/), so a backup of your own run
with `--exclude-caches` — tar, borg and restic all take it — leaves it out
too; without that option they copy it like any other directory. The
directory is the cache's alone: a node refuses one that already holds
anything without that tag — a volume's own `lost+found` aside — because it
empties its own scratch space there at every boot, and refuses one another
process is using.

A cache is a **function of the rows**: its refs are exactly the repository's
refs at the log position it records, its packs exactly the repository's packs
there. Nothing but the cache itself writes it — its sync, the adoption of a
push's pack once the push's row has applied, and its background index — and a
push never does: a push is received in a throwaway scratch repository that
reads the cache through an alternate, so an object nobody committed can never
appear in a cache, where any reader could fetch it by id.

```mermaid
flowchart LR
  rows["code log rows<br/>(refs, packs)"] -->|"what moved since the cache's position"| sync[cache sync]
  store[("object store<br/>S3 or the broker's bucket")] -->|"one object a push, 16 at once"| sync
  sync -->|"split into pack, index, reverse index"| packs["objects/pack"]
  sync -->|"after every pack they need"| refs["packed-refs, HEAD"]
  refs --> serve["git upload-pack<br/>clones and fetches"]
  packs --> serve
```

**Warm and cold.** After a push, every other node brings its cache forward by
a **delta** the next time somebody fetches from it. A delta downloads only
what changed — usually the push's one pack — and rewrites the repository's ref
file, and that rewrite grows with the refs the repository holds: a delta takes
about 7 ms in all at a thousand refs, and about 50 ms at the 100 000 a
repository may hold. The node that received the push does not download it
back: it links the pack its own git wrote into its cache once the push's row
has applied. A **cold** cache — on a node that has never served the
repository, or whose budget evicted it — downloads every pack: sixteen at once
for one repository and at most thirty-two at once on the node, largest first,
so a second repository's cold sync never waits behind the first one's longest
downloads. Each is verified against its SHA-256 and installed as the receiving
node's git indexed it — a push stores its pack together with its index, so no
other node spends a core re-indexing it, and a cold sync is bound by the
object store rather than the CPU. S3 answers each request in about 100 to
200 ms, and from a fake answering that late a cold sync read about 75 to 150
small packs a second: the fifty or so a [compacted](#compaction) repository
holds come up in under a second, the 2 048 a repository may hold at the most
in about 14 to 27 seconds before the first request is answered (about
5 seconds from the same fake answering in 30 ms), and 10 000 — as many
pushes, one pack each, were nothing to merge them — in about 70 to 130
seconds, which is why a repository holds at most 2 048 live packs
([Limits](#limits)).

The first fetch of a repository after a node restarts reads the whole of its
rows once, reusing every pack the cache already holds, which is what makes a
cache exact again after a crash, a full disk or a stray file. A cache never
goes back because one data node's copy of the rows is behind; it goes back only
when the code log did — recreated, or restored from an older backup — which the
node notices, reading every repository whole at its next sync.

**Many repositories at once.** A full read — a repository's first fetch on a
node that just started or joined, a rebuild — reads the repository's rows from
a data node a page at a time, and every page is one read on that data node:
one of the 64 answers its lane gives the whole fleet's reads at once — a seat's
tracker read, a page write, the check every git request is admitted by — and
one of the connections its database scans every one of those reads on, of
which the smallest data node has eight (or, on a git node that holds the data
itself, its own database's, which serves those reads too). So a node reads
**at most eight repositories' rows whole at once**, the rest waiting their
turn, first come, first served: a node coming up with hundreds of repositories
asked of it asks no more of a data node than the smallest scans at once,
rather than every page while the fleet's reads wait behind them. A sync after
a push reads only what moved and never waits for one of the eight. The wait is
part of the request's own: a clone waits for its repository's turn as it waits
for the sync, and a seat's read that has no answer within two seconds is sent
to another git node while the sync goes on.

Against a model of a data node of eight cores, every page as long as a full
page of refs takes to scan — about 60 ms, the dearest a page is — a node
**restarted** over the cache it kept, which downloads nothing, brought 500
repositories up in about four seconds, the data node's own rate, while the
fleet's other reads waited about a tenth of a second for a connection (p99).
Allowed more at once, it came up no sooner and those reads waited longer — a
third of a second at sixteen, a second at sixty-four, 3.6 seconds with no
limit — and allowed four, it took 7.6 seconds. Four nodes **joining** together
read their 2 000 repositories' rows in about 16 seconds whatever the limit,
while the other reads waited about 0.8 seconds at eight, a second at sixteen
and 15 seconds with no limit; in those 16 seconds each node downloads five to
nine packs for each of its repositories from S3, so for repositories of more
history the downloads, not the reads, set how long a join takes. A repository
of a few hundred refs is one page of a few milliseconds, which holds its turn
a twentieth as long. `crewlet.code.cache.full_rounds` shows a node's full reads
running and waiting, and `crewlet.code.cache.read.page.duration` how long the
data nodes take to answer each page.

**The budget.** A node holds its caches to a byte budget —
`git.cache_max_bytes`, or a share of the cache's volume derived at every boot
([`git`](configuration.md#git)) — counting everything they hold on disk: every repository's packs, indexes and refs, the
answers its [memo](#the-memo) keeps, and the downloads and pushes in flight.
Past it, the memo is trimmed to its sixteenth of the budget first, and then
the repositories used least recently are evicted, and nothing else evicts one: a repository nobody fetched over a
weekend is still there on Monday, brought forward by the rows that changed
rather than downloaded again. Each repository's last use survives a restart,
so one read all day and rarely pushed to is not the first evicted after one.
A repository a request is reading is never evicted — one larger than the whole
budget is still served, logged as `code_cache_over_budget`, and evicted as soon
as no request holds it. Every git node that serves a repository caches it
**whole**, so size the volume for the repositories it serves, not the company's
total. A dedicated disk is worth it, because a cold sync writes everything it
downloads at the speed the object store delivers it. Size the node's memory
for its largest repository too: the background index below needs about 600
bytes of memory for every object of a repository to write its bitmap, and a
repository past what the node can give it is indexed without one.

**Background indexing.** With one pack a push, git would search pack after
pack for every object. So once a repository's packs have stood still for ten
seconds — or the oldest has waited ten minutes, for one pushed to without pause
— a git node writes it a multi-pack index with a bitmap, and a commit-graph, so
a clone counts its objects from the bitmap rather than walking the history. The
bitmap carries git's lookup table, so a request that opens it reads only the
bitmaps it needs rather than every commit's: over a gigabyte, the pack-objects
run that answers a blobless clone's lazy fetch of one blob takes 5.0 ms with
the table and 6.2 without, and the one that answers a fetch of one new commit
8.2 ms rather than 9.0 — the git process alone, which is part of what such a
request costs beside its exchange, its sync and its checks. The node writes
them again once 64 more packs have gathered beside them, at most every ten
minutes and at most a tenth of a core's time for any one repository. A
run, on one core, takes about 12 seconds a gigabyte of packs at 100 MB and 14
at a gigabyte on git 2.56.0, more a gigabyte the larger the repository. Runs
take at most half the node's cores, one each,
in a slot that is free or not at all, so they never make a push or a fetch
wait; and only as many run at once as a quarter of a gibibyte a core holds
beside the largest, so a node that has just joined does not index all its
largest repositories at once.

**The commit-graph first.** A copy the node has just read whole — after a cold
sync or a rebuild, or on a node that has just joined — gets its commit-graph at
once: ahead of every other repository's index, without waiting for its packs
to stand still, and in a slot of its own while the background work holds the
others — the one kept for `crewlet code verify` included, for the graph alone
and never for the index and bitmap the copy may be due for after it. Without
one, git walks a repository's commits by date, so a push of a
commit dated before its parent — a clock that is behind — walks every commit
of the history in each of its checks; with one, the engine's checks go by
generation and only git's own connectivity check still walks by date (see
[A push, end to end](#a-push-end-to-end)). The graph is one walk of the
commits, about 3.4 seconds over a gigabyte of source history, so that window
lasts seconds rather than as long as the node's queue of other repositories'
bitmaps. Reading a copy whole does not write the graph itself: the request
it was read for would wait those seconds to spare the rare skewed push the
same. A copy that serves from a published chain gets its node's **tip**
over the chain the same way ([Published layers](#published-layers)).

**Changed-path filters.** Every commit-graph a git node writes carries git's
changed-path filters, one a commit: a `git log -- <path>` or a blame skips
each commit whose filter says the path did not change rather than diffing it
against its parent — 0.044 seconds with them against 0.399 with no
commit-graph, for one path over a synthetic history of 40 000 commits on git
2.56.0. Every write computes every filter its commits lack: a copy's own
graph computes them all at its first write and reads them back from the
graph it replaces at every write after, and a node's tip over a published
chain ([Published layers](#published-layers)) computes its tail's afresh at
each write. No write bounds how many it computes, because a bound saves the
write little and costs every read: what a write holds for its filters is
the trees their diffs read, which git keeps up to its delta base cache's
96 MiB however few filters it computes, and a commit without a filter is
diffed by every read that passes it. Over a gigabyte's 455 680 commits whose
every commit rewrites a directory of a thousand entries four levels down, a
file's blame answered in 0.073 seconds and a page of a path's log in 0.116
once a copy's own graph held every filter, against 0.98 and 2.51 seconds
after a first write of 100 — a bound of 100 a write would have needed over
four thousand writes to reach every commit — and over a tip on a tail of
90 909 objects, 0.065 and 0.103 seconds against 0.66 and 0.71 with 100 new
filters a tip. Computing every filter took that copy's first write 33.5
seconds where 100 took 3.0, once, and the tip 0.70 seconds where 100 took
0.07. They are git's version 2:
version 1 hashes a path by whether the platform's `char` is signed, so its
filters disagree between an `amd64` and an `arm64` build for every path
outside ASCII, and every git a node runs reads and writes version 2.

**When packs leave a copy.** A [compaction](#compaction) merges a repository's
packs and retires the ones it merged, and a whole read can find a pack no row
names any more, after the code log went back under the node. git opens a pack only when a request first needs
it, part way through, so a pack never leaves a copy a request may be
reading: the node builds the copy's **next directory** beside it — every pack
the rows still name, the ref file and the files built over them that the
change leaves valid, each hard-linked from the old directory, so it
downloads nothing, runs no git and costs three file links a pack it keeps and
four to six disk syncs (six to nine for a copy that serves from a published
chain, whose two chain files it writes from the rows), whatever the packs'
bytes (a sync that downloaded a merge's output and moved a copy of 50 packs
took 7 ms, and one of 2 048 took 119 ms, measured over the in-memory store,
whose download waits on no first byte) — moves it into place
whole, moves the memo of answers
into it in one rename, and serves every later request from it, while the
requests already reading the old directory keep
every pack they may still open until the last of them ends. A compaction
keeps the commit-graph, since a merge drops no commit; a pack that left with
no compaction retiring it takes the graph with it, and the background
indexing writes it again at once. The multi-pack index and its bitmap name
the packs that left, so the next directory starts without them, and the
background indexing writes them again at its usual pace. A background index
run the move interrupts counts as one of the repository's runs, so a
repository compacted more often than its index takes to write holds a
background slot no more than its usual share of the node's time. The two
directories share every file the new one kept, so whatever a request still
reading the old directory learns about one counts for the new one: damage its
git reports there makes the new directory's scrub due, and a request that
fails on damage there runs once more over the new directory before any repair
is spent, which ends it if the damaged file was one that left and repairs it
in the new directory if the two share it. A move that
fails — the disk refusing a link, a full disk — leaves the copy as it was,
still holding every pack the rows name, and the repository's next sync tries
again; one a crash interrupts leaves the old directory in place, and the
node's first sync of the repository after its restart finishes the job,
keeping the graph. `crewlet.code.cache.swaps` counts every move by whether
the commit-graph came along, was dropped or was never there to keep —
`chain` for a copy that serves from an installed published chain, whose graph
layers move with it, a placement of one included
([Published layers](#published-layers)) — and every one that failed.

**Packs an expunge is dropping.** An expunge of a repository's unreachable
objects — which nothing in this build begins yet ([what it does not do
yet](#what-it-does-not-do-yet)) — first rewrites the packs it takes into
packs of what the refs reach, kept, and packs of what nothing reached when it
read the refs, **dropping**, and only a second record takes the dropping ones
out. Between the two, a git node keeps the repository's dropping packs in a
directory of their own beside its packs, `objects-dropping/pack`, which only a
git process serving the repository reads: a clone, a fetch and a seat's read
still find an object there by its id, so a client that already holds one
loses nothing before the expunge ends. A push never sees them. It is received
in a scratch repository that reads the copy's own packs and nothing beside
them, so a push that relies on an object being dropped is refused by git's
own connectivity check rather than landing a ref on an object the expunge
then takes out of every node; and a push the node checked while the packs the
expunge rewrote were still in the copy is refused `expunging` and runs again
([What git prints](#what-git-prints-and-what-it-means)). No index or bitmap
covers the dropping packs. They count against the cache's budget like any
other pack, and `crewlet.code.cache.dropping.bytes` shows how much of the
node's cache they hold: nothing but while an expunge is open on a repository
the node holds, and held up for long by one that began and does not end.
`crewlet code verify` names a dropping pack found among the live ones, or a
live one among the dropping, as a stray, and its repair puts each back. When
the expunge ends, the dropping packs leave the copy as a compaction's packs
do, by its next directory, and they are the one thing whose leaving takes the
commit-graph with them: the commits in them are gone from the repository, and
git walks a graph that still names one as though it were there. The answers
the copy keeps for seats' reads go too, since one may quote what was purged,
and so does a node's tip over a published chain; the multi-pack index and the
published layers stay, never having covered the dropping packs.

**What the cache writes beside the packs.** Six kinds of file in a
repository's cache are derived on the git nodes rather than named by the
rows: the multi-pack index — a full read's, written before it answers once
the repository holds 256 packs, and the background indexing's, with its
bitmap or without one — the commit-graph, the index of which push introduced
each commit ([Which push landed a commit](#which-push-landed-a-commit)), the
answers of seats' reads by object id ([The memo](#the-memo)), and an
installed published chain's multi-pack-index layers with their bitmaps and
its commit-graph layers, each half with the chain file the node writes over
it ([Published layers](#published-layers)). Each lives
inside the repository's own directory, so it counts against the budget and
leaves with the repository when it is evicted. The multi-pack index and the
commit-graph are written again over a rebuild's packs, and each is dropped
before a pack it may name leaves the directory — the copy's next directory
never holds it ([When packs leave a copy](#the-cache-on-a-git-node)) — so no
git process reads an index that names a pack that is gone; the one exception
is the commit-graph when a compaction retires the pack, since a merge drops no
commit and the graph names only commits the repository still holds. An
expunge is the other way round: the packs it drops lie beside the others,
where no index or layer looks, so those stay, while the commits it takes out
leave the repository, so the commit-graph goes — a graph still naming one has
git walk through it as though it were there — and the answers of seats' reads
go with it, since one may quote what was purged. An
installed chain is cut rather than dropped, below the first layer naming a
pack that leaves — and a pack an installed layer names stays while the layer
does, so it is the chain moving that takes one out. The installed layers are
linked into a rebuild's copy where their files and packs are whole, and
fetched again otherwise. The index
of pushes is built from the push rows rather than the packs, and the memo's
answers are about objects rather than packs, so a pack leaving changes nothing
of either: a rebuild or a move to the next directory links the index of pushes
into it and moves the memo there whole, in one rename however many answers it
holds, and a log restored or recreated under the node drops both. One the node
has not written since it started is held to its
checksum before it serves — git's for the first two and a chain's layers,
the layers installed for its chain files, the run's own for the
index of pushes — and every file of a copy is checked again behind the
serving; a memo's answer is checked by every read of it instead. Anything else in the directory that is neither one of them nor the
repository's own files is moved aside the next time the node reads the
repository whole — and so is a symbolic link, or anything but a regular file,
at one of their names or at the repository's own `HEAD`, `config` or
`packed-refs`, whatever it points at. git reads the file a link at `config`,
`packed-refs` or one of those files leads to, outside the directory. A link at
`HEAD` it refuses outright, the copy then being no repository to it, so every
request on the copy fails until the link is gone — unless the link's own text
begins `refs/`, which git reads as the ref named there, or, where the rest of
that text climbs back out of `refs/`, as the file it leads to.
`crewlet code verify` names any of them. Each is declared once in the engine,
and this table is that list; the steps are what
`code_cache_maintenance_refused` and its counter name when the node's memory
refuses one — never `provenance`, `memo` or `layer`, which the engine's own
code writes, holding no git slot and charged against no memory budget. The
`layers` and `graph-layers` files are git's own, written once by the
repository's maintainer and installed byte for byte from the object store
([Published layers](#published-layers)) — all but the node's **tip**, the
one `graph-layers` file its own background indexing writes (`tip-graph`), over
the commits the installed layers lack. A copy holds them or `midx` and
`commit-graph`, never both, and placing a chain drops the other two first.
The tip stays when a compaction retires a pack and the chain beneath it stays
whole, and goes with every other change to the chain or the packs below,
written again at once.

| Declared file | Its files, in the repository's directory | Written by | When a compaction retires a pack | When an expunge drops a pack | When any other pack leaves |
|---|---|---|---|---|---|
| `midx`, the multi-pack index | `objects/pack/multi-pack-index`, `objects/pack/multi-pack-index-<checksum>.bitmap`, `objects/pack/multi-pack-index-<checksum>.rev` | a sync, before it answers: `index`; the background indexing: `bitmap`, `index` | dropped first | kept | dropped first |
| `commit-graph`, the commit-graph | `objects/info/commit-graph` | the background indexing: `graph` | kept | dropped first | dropped first |
| `provenance`, the index of which push introduced each commit | `crewlet/provenance.<n>` | a sync, before it answers: `provenance`; the backfill: `provenance` | kept | kept | kept |
| `memo`, the answers of seats' reads by object id — a directory, one file an answer | `crewlet/memo` | a seat's read by object id: `memo` | kept | dropped first | kept |
| `layers`, the installed published multi-pack-index chain — each layer's index and bitmap, and the chain file the node writes from the rows | `objects/pack/multi-pack-index.d/multi-pack-index-chain`, `objects/pack/multi-pack-index.d/multi-pack-index-<checksum>.midx`, `objects/pack/multi-pack-index.d/multi-pack-index-<checksum>.bitmap` | the install of the maintainer's published layers: `layer` | cut first, below the layer that names it | kept, no layer naming a pack an expunge drops | cut first, below the layer that names it |
| `graph-layers`, the installed published commit-graph layers, one a layer, the node's tip over them, and their chain file | `objects/info/commit-graphs/commit-graph-chain`, `objects/info/commit-graphs/graph-<checksum>.graph` | the install of the maintainer's published layers: `layer`; the background indexing: `tip-graph` | cut first, below the layer that names it | kept, but for the node's tip, dropped first | cut first, below the layer that names it |

**Memory.** Every git process a node runs over a whole repository holds memory
in proportion to the repository's objects, measured over a source repository's
history — the background index's writes on the node pin, git 2.56.0, and the
check and a clone on git 2.43: about 600 bytes an object for the bitmap (388
over 32 300 objects, 560 over 296 000 and 287 over three million: a share
that rises to 100 MB and falls past it, with no fixed part at the small end),
80 for the index without one and 1 MiB beside it whatever the repository's
size, 67 for the commit-graph and 100 MiB beside it whatever the
repository's size, 192 for `crewlet code verify`'s
connectivity check, and a clone about 240 more than the 224 MiB its git
transfer slot keeps for it — the clone of a repository with no bitmap walks
its whole history. The commit-graph's 100 MiB are its changed-path filters':
each filter is a diff of a commit against its parent, whose trees git keeps
in its delta base cache up to that cache's 96 MiB cap — 112.8 MiB in all
over a 100 MB history whose every commit rewrites a directory of a thousand
entries, and 250.1 MiB over a gigabyte of it, a line meeting no objects at
95 MiB. The node's tip over a published chain is the one step
that walks less than the whole history, the commits the chain lacks, so it is
charged by the tail rather than the repository: 50 bytes an object of the
tail and 100 MiB whatever its length, for its changed-path filters — measured on
git 2.56.0 over tails of 23 000 to 364 000 objects outside a gigabyte's
published layer, 2.4 to 16.7 MiB with 100 new filters a write, and 102.3 MiB
computing every filter of a tail of 90 909 objects of that deeper history. A
[compaction](#compaction)'s build walks no history, only the packs it merges,
and is charged by them: 550 bytes an object merged, 240 more an object merged
for a transfer's room beside them, and six times
`code.limits.max_blob_bytes` for its delta search. A
[publication](#when-a-repository-is-published)'s steps run one after another,
and it is charged the largest of theirs: 600 bytes an object of the whole
repository for its bitmap — whose writer walks every commit however few
objects the layer adds — when the layer is the chain's root, and 350 when it
is written over a base, every publication after the first (a later layer's
bitmap held 176, 312 and 90 bytes an object at 10 MB, 100 MB and a gigabyte,
a root's 384, 560 and 287); 350 an object of the layer for its commit-graph
layer, and 100 MiB beside it for its changed-path filters' diffs; and a
compaction's for its repack. A repository is placed on the node
that maintains it by its root's charge, which its maintainer must be able to
hold. A git node reads its
memory — the least of the machine's and any cgroup limit set on its
container — and keeps 224 MiB of it for every
git transfer it admits at once and 896 MiB for each core its git runs on, for
the store, the broker, the engine and its reads. Both count only the cores the
memory backs at 2 GiB a core: a container limited in memory and not in CPU sees
its host's every core, so a node of 64 cores and 16 GiB admits 32 transfers
rather than 256, and runs as the eight-core node its memory makes it. What is
left is the budget of its background work, and every step of it — the
bitmap, the index without one (a cold cache's included), the commit-graph, the
tip, a connectivity check, a compaction's build, a publication's — is held to the budget
together with everything in flight, each charged its own figure an object of
what it walks and room beside it for one clone of the whole repository — but
a compaction's build, whose room is a transfer of the packs it merges: any
other step refused costs speed, a publication's repack among them, while a
build refused leaves the repository's packs to gather until its pushes are
refused, so a room that grew with the repository would refuse every merge, and
in time every push, of any repository whose clone the node's budget cannot
hold beside the build — on either node worked through below, repositories
under the 20 GiB one may hold. A
step whose charge alone is past the budget never runs on the node, and the
repository is served without it: a bitmap refused leaves the repository
indexed without one — its pushes keep joining an index, so lookups stay fast,
while a clone walks its history — keeping the bitmap the node wrote while it
could until 256 packs have gathered outside it. Each step refused is logged
once as `code_cache_maintenance_refused` — a compaction's build as
`code_cache_compaction_refused` — with its charge and the budget and counted
by `crewlet.code.cache.maintenance.refusals`, and a refused bitmap raises the
[`code_maintenance_refused`](../reference/alarms.md) alarm for as long as it
stays refused. A refused tip raises nothing: the copy is still served through
its chain's bitmap and graph layers, and only the commits pushed since the
last publication go by date — `crewlet.code.cache.maintenance.refused{step=tip-graph}`
counts the copies. A publication that shortens the tail lifts a tip refused for
the tail's length, never one refused for the repository's size: the tip keeps
room beside the tail for a walking clone of the whole repository, so a node
whose budget cannot hold that clone writes no tip of the repository until it
is given more memory. A connectivity check the budget cannot
hold is refused to whoever asked for it, and one that the work in flight
leaves no room for is busy, as a full lane is. A step that fits the budget
alone but not beside what is running waits for it, and the first such
repository in the queue is given the room it needs: nothing smaller starts
in its place.

So **a git node's ceiling on a repository it keeps a bitmap of is bounded by
memory — about 840 bytes an object, with the clone beside it — before it is
bounded by disk**: at about three million objects a gigabyte of a source
repository's packs, about 2.5 GB of memory for each gigabyte of packs, and some
50 GB for the 20 GiB a repository may hold by default, which few nodes have.
A node of 2 GiB a core leaves a quarter of a gibibyte a core (1 GiB on four
cores: bitmaps of repositories of up to about 1.2 million objects, indexes of up to
about 3.3 million); 16 GiB on four cores leaves 9 GiB: bitmaps of up to about
11 million objects, indexes of up to about 30 million. Give the node the
memory: it reads it again every ten seconds, so a container resized in place
is held to its new limit at once (`code_cache_memory_changed`), while the git
lanes keep the sizes they took at boot until a restart. An operator's
`git.max_transfers` is kept as written rather than sized to the memory, so a
figure set by hand can leave the background work nothing: once its
transfers' 224 MiB each reach the memory less the 896 MiB of each core it
backs — as little as 56% of the transfers the memory holds at 224 MiB each,
where the node has a core for every 2 GiB or more. Lowering or unsetting
it, or memory past what the node keeps, gives the budget back; a restart
alone does not. A node whose memory cannot be read at all refuses to serve
git rather than guess it.

The budget bounds the background work and keeps room for one clone beside
each of its steps; it does not bound the clones themselves, which take the
node's git transfer slots — four a core and no more than one for each
512 MiB of its memory, each kept 224 MiB — whatever the repository. A node
that serves repositories of millions of objects to many cloning clients at
once needs memory for each clone in flight: about 240 bytes an object of the
repository, past the 224 MiB.

A cold cache of a repository
holding 256 packs or more gets an index — without the bitmap — before its
first request is answered, since at that count it saves the request more than
it costs, when the budget holds it beside the work in flight (otherwise it is
logged as `code_cache_round_index_held` and left to the background); the
bitmap follows when the background indexing reaches the repository, which on
a node that has just joined comes after every repository with more packs
waiting. A cache without either is slower, never wrong.

**Damage and repair.** A damaged cache is repaired under the request that
found it — a clone, a fetch, a push, or the node's own sync of the repository
— and the request then served from the repaired copy; any number of requests
failing on one damaged copy cost one repair. How depends on the damage:

- **Its indexes, before they serve.** Every file git reads to find an object or
  a parent — the commit-graph, the multi-pack-index with its bitmap and reverse
  index, and each pack's index and reverse index — is held to its own checksum
  (the one git writes at its end) before a copy the node's current process did
  not write answers its first request, and again whenever such a file is
  replaced. git checks none of them when it reads, and a damaged one answers
  wrong without a word: a commit-graph naming the wrong parent lets a push
  force a protected branch as though it were a fast-forward (git 2.55 and
  later answer from the graph), a pack index naming the wrong offset makes a
  clone ship the wrong object, a bitmap missing objects makes every clone
  incomplete. A derived file that fails is dropped and written again at once
  over the same packs; a pack whose index fails is fetched again — one
  download, renamed over its files in the same copy, so a request already
  reading the pack is not cut off. What the node's current process wrote, or
  has checked since, is not read again until it changes. What a check finds
  that its sync cannot put right there and then — the object store refusing
  the download, or the sync failing on a newly pushed pack the store will
  not hand over — waits for the next sync rather than the next check: an
  index damaged where it lies after the node checked it looks to the node
  just as it did, so only that finding says it needs repairing.
- **Every byte, behind the serving.** Each copy a node uses is read whole
  again — every pack against its own checksum, and every index file — once
  after its first request when a previous run of the node left it (its packs,
  and nothing that request's own check had just read), then once a day while
  it is used, and at once when a git process reports a pack or an object of it
  unreadable or dies of a crash — unless that process was a request's whose
  failure the request repairs itself (its packs, below), whose repair is then
  the answer. That is the one way two kinds of damage are
  ever found: a damaged pack is sent to clients byte for byte (git copies a
  stored object as it is stored — damaged bytes from any pack, and a damaged
  entry header from the bitmap's preferred pack, which it sends without
  reading — so no git on the node notices while every clone of it fails at
  the client), and a file damaged in place after it was checked — a bitmap
  missing an object fails every clone of its branch — is reported by
  nothing. What fails its checksum, or what the disk cannot read, is fetched
  again or dropped as above, one download a pack; a pack fetched again for
  its body alone keeps the multi-pack-index, bitmap and commit-graph built
  over it, since it is the same pack byte for byte. A download that fails
  leaves the copy serving as it is and is tried again ten minutes later, then
  twice as long each time up to a day (`code_cache_pack_reinstall_failed`).
  It runs one copy at a time on a node,
  a core and a read slot a 64 MiB piece, and reads nothing of a copy nobody
  uses; scrubs go in the order of their evidence — one a failing request
  asked, then one a report asked, then a copy's first, then the daily ones —
  and a request's or a report's stops a first or daily scrub at its next
  piece rather than waiting behind it. A request on a node whose every other
  read slot is busy waits for one piece at most: 51 to 59 ms from the page
  cache, and the piece read at the volume's own throughput when the copy is
  cold — about half a second at a gp3 volume's 125 MB/s baseline. A report
  within an hour of a scrub that found nothing asks for no other, whatever was
  written to the copy meanwhile; one within an hour of a scrub whose findings
  still wait for their repair — a download the object store refused, say —
  asks for none either, since that repair is its answer; past that hour such
  a report scrubs the copy again, at most once an hour, so damage that landed
  elsewhere in the copy meanwhile is not left to the daily scrub.
- **Its shape.** The node will not run git in a copy that is not the shape the
  engine writes — its config edited, a `commondir`, an objects directory that
  is not its own, or an alternates file planted in it (by a disk, or by
  anything outside the engine; a copy's objects come only from its own packs,
  so an alternates file there is refused whichever request meets it). Nothing
  of what it holds is lost, so the copy is put right **in place**: read whole
  again from the rows in the same directory, which rewrites its config, moves
  aside whatever the engine never writes there, and reuses every pack it holds
  — no download, and never held back, since it costs a read of the rows. Only
  a copy that read leaves still refused — something that keeps writing into
  it, or a filesystem that does not keep what it is given — is rebuilt as
  below.
- **Its packs.** When a request's git finds a pack or an index it cannot read,
  or an object a row names that no pack holds — or the node's own sync's git
  does, peeling the tags it sets — the copy is put right **in place first**,
  under that request or that sync. The node reads it whole again in the same
  directory, holding every index file to its checksum whatever it checked
  before — at most once an hour while nothing has been repaired in the copy
  since, whether or not that read found anything and however its sync ended
  (what it found waits for the next sync, as above), or while the only packs
  it could fetch again are ones the object store has just refused, so a busy
  repository's requests failing on one damaged pack cost one such read, not
  one each — and fetches again, one download each, every pack whose index
  files fail, that the copy has lost or holds at another size, or that git
  named —
  git 2.55 and later name the pack of a corrupt object, which the node hashes
  against what the push stored before it believes it. A damaged
  pack body no index shows is found by scrubbing the copy at once for that
  request — unless a scrub of the copy has already said: one whose findings
  still wait for their repair is the answer, however often a request's own
  read finds the same files failing again, and so is one that found nothing,
  if it began after the request's git or ended within the hour with nothing
  repaired in the copy since. Only when neither finds anything to fetch again
  is the repository read whole from the rows and the object store into a new
  directory beside the damaged one, which replaces it once it is complete —
  linking every pack the scrub found whole rather than downloading it, so a
  copy whose rows are the damage costs no download at all. Requests already
  reading the old copy finish on it.
  A request whose git began before the node last wrote a repair
  into the copy — a pack fetched again or put back, a derived file dropped,
  before the request's own repair or while it runs — is not repaired any
  further: it may have failed on what the repair replaced, so it is run once
  more over the copy as it stands, once any repair still running on it has
  ended, and only damage it meets again is repaired; a sync whose own git met
  it so syncs again. Each request or sync passes each step once, so a repair
  ends whatever the damage. A pack the object store will not hand over is
  asked for once: the download that failed holds the next one off — ten
  minutes, then twice as long each time up to a day
  (`code_cache_pack_reinstall_failed`) — and until it has passed, a request
  or sync that meets the pack's damage takes no rebuild, since that would ask
  the store for that very pack again, and no forced read of the copy but the
  hourly one above, which leaves the pack as it stands. It is answered 503 —
  with the store's own refusal of the pack wherever a rebuild would have come
  next, and that the copy is still damaged otherwise
  — and the first to meet the damage once the hold has passed asks the store
  once more. A pushed pack is no
  repair: it replaces nothing a failing request read, so a repository pushed
  to all day has its damage repaired like any other. What the failing git
  reported belongs to that request, or that sync: it is held for its own repair
  rather than asking the copy's scrub as well, so a repair of one pack never
  pays for a read of the whole copy beside it — 1.13 times its pack bytes,
  some 24 GB for the largest repository the defaults admit — nor races one to
  fetch the same pack again (`crewlet.code.cache.reports{outcome=request}`).
  It stays the request's through a repair of the copy's shape or its derived
  files, which reads no pack and answers none of it, so when the request's
  next try meets a damaged pack beneath a dropped index, that pack's repair
  answers it too. A request that repairs no pack — its git failed after its
  answer began, or in a way the node does not repair, or never failed, or
  was put right by a repair of the shape or the derived files alone — hands
  what it held on as it ends, and the copy's scrub is due for it as for any
  other report.
- **Its derived files.** The multi-pack-index git builds over the packs (with
  its reverse index and its bitmap) and the commit-graph are DERIVED files,
  written on the node from the packs it already holds, so damage git names in
  them alone is put right **in place**: the damaged files are dropped — the
  index, the graph, or both where git's message names a chunk-format file
  without saying which — and the node writes sound ones over the same packs
  at once: no download, no new directory. That happens whenever **any** git
  process on the node names them damaged, whether or not a request failed:
  git reads around most damage to a derived file — it warns, answers from the
  packs and walks by date, slower — and the request that got its answer never
  looks at the warning, so every git run is reported to the cache, the
  node's own background indexing included (writing an index over a damaged
  one fails every time, and its own warning is what has it dropped). A run
  that merely reported damage takes nothing out of service: the copy keeps
  answering while a sync of its own, with no request waiting, drops the files.
  Two things hold it back: a file the node wrote after the reporting process
  began is kept — that process may have read the file it replaced — and a
  check of the copy defers the drop until it ends, so a check reports what it
  found rather than a repair made under it. A derived file named damaged again
  within a day of its last drop for damage — whatever the node wrote of it
  between, its own ordinary rewrites included — is dropped and left to the
  ordinary schedule rather than written again at once, so a disk that keeps
  damaging what is written to it costs one rewrite at once a day and the
  ordinary schedule's after it rather than a loop
  (`code_cache_derived_damaged_again`). A clone over an index whose positions
  are damaged makes git die `invalid MIDX object position, MIDX is likely
  corrupt` (every git at the node floor; one older than 2.47 crashed there
  without a word) as it starts building the pack, after the answer's first
  line — so a fetch's
  answer starts only once its pack does, and a fetch whose git dies before
  then has shown the client nothing and is run again, over the repaired copy.
  Only damage that survives the drop — a pack the index named is itself
  damaged — falls through to a pack rebuild above. Before, such a clone failed
  with `aborting due to possible repository corruption on the remote side` on
  every clone of the copy until an operator repaired it.

Damage is read from git's own report of each failure, by the FORMAT of the
message — the sentence git's source fixes — and never by its words. Git
repeats the text of a request line it refuses in the message it dies with, so a
request quoting `is corrupt` would, if the node read the words, have it read
the repository whole again from the object store; the format carries none of
the request's text, so no request can name damage. Every git process the node
runs reports to it this way, the ones git starts itself included, so a fetch
whose own git meets damage before its answer begins — an object the fetch
names that git cannot read — is repaired and served like any other request.
One damage git 2.56 names only in words — a packed object whose header is
wrong, reported as a bare sentence with no format of its own — is not taken at
its word either: the node hashes the pack the sentence names against what the
push stored, and fetches that pack again only when they differ. Git 2.55
reports the same sentence with a format of its own, and the pack it names is
held to what the push stored the same way, so the repair fetches that one
pack.

A rebuild that does not end the failure is not repeated at once: the damage is
then in the object store or in the rows rather than in the node's copy, and
another rebuild would read the same bytes. So the next attempt waits ten
minutes, each one after it twice as long, up to a day; a cache holding exactly
what such a rebuild read is not rebuilt again for a day unless something writes
into it — and a copy still refused for its shape always has, so only the
doubling wait holds it back; and every refusal is counted, and logged as
`code_cache_rebuild_refused` at most once every ten minutes a repository. A
pack the object store does not have is called lost only once a read at the
log's head, made after the store said so of that pack, still names it: the
sync's own read, even one at the head, came before that answer, and a row can
leave the log in between — a deleted repository's purge removes its pack rows
— in which case the sync carries on to what the log holds, and asks about
every pack it then cannot have the same way. A lost pack, or one the store
holds damaged, fails the sync at once naming the pack, with nothing fetched
again, and the collector's daily audit raises the
[`objects_missing`](object-store.md#alarms) alarm for the same object.

**Checking a copy.** An operator asks a git node to check its copy of a
repository against the rows — every pack hashed as the bundle its push stored,
every multi-pack-index, bitmap and commit-graph held to its own checksum (the
one check that names a damaged bitmap: no git command can), an installed
chain's chain files to the layers installed and each layer to the row it was
installed from ([Published layers](#published-layers)), the refs against
what was written, git's own connectivity check — and, asked
to, to repair what it finds ([`POST
/code/repos/{name}/verify`](../reference/api-endpoints.md#post-codereposnameverify)).
Where everything it found is the copy's own directory out of place — a file
the engine does not write there, a fixed file edited or replaced by a link, a
pack no row names, a shape git is refused in — it reads the copy whole IN
PLACE, as a shape a request meets is: every pack, the multi-pack-index, its
bitmap and the graph kept, nothing downloaded — but a pack no row names,
which leaves as the copy moves into its next directory, taking the
multi-pack-index and the graph built over it along, still with nothing
downloaded (When packs leave a copy, above). Damage — a pack missing, at
another size or not its row's bytes, an index failing its checksum — is
rebuilt into the next copy, and so is whatever the read in place leaves —
something writing into the directory as the node puts it right, a shape among
it — linking in every pack the check found whole so the rebuild downloads only
the rest: a rebuilt copy has no index, bitmap or graph until the node writes
them again, minutes to hours of slower clones of a large repository, which a
directory put right in place never costs.
The sync that first brings the copy up to date repairs nothing for a check: damage
its own git meets there is the check's first finding, repaired only when the
check was asked to repair, so a check never hides what it was asked to name —
and an index file that sync's own checksum check found damaged and put right
(the copy's first sync since the node started) is named first too. The read in
place is the check's in the same way: what its git meets is the second check's
first finding and is rebuilt under the operator's repair, never left to the
repair a request's failure asks for, which would download every pack again and
can be held back by how recently the copy was last rebuilt.
A check reads every byte of the copy, so it runs on the node's own time — the
request that asked for it is answered in place when the check ends within a
quarter of a second, a small copy's, and otherwise the check is read until it
ends — and it is **admitted
before it reads anything**: it holds a slot of the maintenance lane — the one
the background indexing leaves free — and its connectivity check's memory from
its first read to its last, and a node with neither free refuses it at once
rather than after reading the repository. Each check pass is **bounded by what
the copy holds** — about 1.1 seconds a mebibyte, against a measured 9 ms,
nearly all of it git's connectivity check — and ended there, so a check wedged
on a disk holds neither its slot nor the operator who asked; the node says
when it ends the pass running, and a client waits by that. It checks only a
copy the node holds: one it never served has nothing to check, and building one
would be a download of the whole repository that evicts a warm one — so a
check goes to the git nodes that serve the repository.

**What to watch.** `crewlet.code.cache.bytes` against the budget,
`crewlet.code.cache.repos`, `crewlet.code.cache.sync.duration` by `kind` (full
syncs dominating is a cache too small to stay warm; a full sync's time counts
its wait for one of the eight full reads),
`crewlet.code.cache.full_rounds` by `state` — `waiting` climbing is a node
bringing up more repositories at once than it reads whole —
`crewlet.code.cache.read.page.duration` by `kind` — a `full` page past about
60 ms is the data node's connections queueing, which its
`crewlet.store.pool.wait` confirms — `crewlet.code.cache.rebuilds` by `reason`,
`crewlet.code.cache.rebuild.refusals` by `rule` — rising at all, damage no
rebuild repairs — `crewlet.code.cache.swaps` by `graph` — `failed` at all, a
cache volume refusing links or out of room, and `dropped` beside a code log
nobody restored, whole reads that could not tell a compaction's packs from
others, each a commit-graph written again —
`crewlet.code.cache.checks` by `outcome`, `file` and
`trigger` — any `damaged` is a disk losing data, or something outside the
engine writing into the cache, and `damaged` under `file=pack` or
`trigger=scrub` damage only the scrub could have found —
`crewlet.code.cache.reports` by `part` and `outcome` — `queued` rising is a
disk losing data under the cache, `again` one that keeps losing it, `crash` a
git process dying on something in a copy, `request` requests failing on
damage they then had repaired under them — `crewlet.code.cache.maintenance.refused`, above zero a
repository whose bitmap the node's memory cannot hold, or under `step=tip-graph` a chained copy whose
tip it cannot —
`crewlet.code.cache.chained` against `crewlet.code.cache.repos`, the
repositories served from an installed published chain rather than in their
own mode ([Published layers](#published-layers)) —
`crewlet.code.cache.layers` by `outcome` — `missing`, `corrupt` or
`newer_git` at all a layer this node refused and serves without, each logged
as `code_layer_refused` ([When a layer is missing](#when-a-layer-is-missing)),
and `failed` climbing an object store failing the installer — beside
`crewlet.code.cache.layer.install`, how long each layer took to fetch, check
and place — `crewlet.code.publish.refusals` by `reason` on a maintainer —
`memory` at all a repository no git node holds the publication of, which
`code_maintenance_refused` raises and `code_cache_maintenance_refused` (step
`publish`) names, any other reason logged as `code_publication_refused` — and
`crewlet.code.cache.memo.lookups` by `outcome` beside
`crewlet.code.cache.memo.bytes`: misses climbing while the memo sits at its
sixteenth of the budget is a memo too small for the pages seats ask for again,
and `corrupt` at all a disk damaging what the cache holds
([metrics reference](../reference/metrics.md)).

### Published layers

A repository's multi-pack index, its bitmap and its commit-graph are the
dearest files a git node derives — writing the bitmap walks the whole history
and holds about 600 bytes of memory an object of it ([Memory](#the-cache-on-a-git-node))
— and every git node that serves the repository wants them. So they can be
written **once**, by one git node, the repository's
[maintainer](#who-maintains-a-repository), and installed by every other
([ADR-0061](https://github.com/crewlet/crewlet/blob/main/adr/0061-a-derived-object-is-published-kept-and-never-audited.md)).
The maintainer writes git's own incremental
multi-pack-index **layer** over the packs the layers below it do not cover,
with that layer's bitmap and a commit-graph layer over the same packs; stores
the three files in the object store, in parts of at most 1 GiB; and names the
layer on the code log on top of the repository's **chain**: at most 32
layers, root first, each valid only over the exact layers beneath it
([Live, retired and derived references](object-store.md#live-retired-and-derived-references)).
Every git node installs the chain the rows name. Each layer is fetched, held
to its row and placed byte for byte as the maintainer's git wrote it, which
any node holding the layer's packs byte for byte can read — every copy does.
The two chain files that list the layers are the node's own, written from the
rows' order, since a layer's files never name the layer beneath them. Who
publishes and when is [When a repository is
published](#when-a-repository-is-published); this section is how every git
node installs what the rows name.

**Chain mode or own mode, never both.** A copy holding an installed chain
serves in **chain mode**: each layer's `.midx` and `.bitmap` in
`objects/pack/multi-pack-index.d/`, each commit-graph layer in
`objects/info/commit-graphs/`, and the chain file of each. A copy holding
none serves in **own mode**, from the whole multi-pack index, bitmap and
commit-graph its background indexing writes (above). git refuses the two side
by side: it reads a whole index before a chain, and writing either whole
deletes every layer and its chain file. So placing a chain drops the copy's
own index and graph first; the background indexing writes neither while a
chain is installed — only the node's tip over it (below) — and a sync writes
no index of its own beside one; and a chain cut to nothing leaves the copy in
own mode, its index and graph the background indexing's to write again.
`crewlet.code.cache.chained` counts the repositories a node serves in chain
mode.

**No node writes a layer of its own.** On git 2.56 a layer with a bitmap
costs the walk of the whole history however few objects it adds. Over a
synthetic history of 220 000 objects in 64 packs, a layer of an eighth of the
objects took 1.52 seconds and 132 MiB and a layer of a sixty-fourth 1.89
seconds and 128 MiB, against 1.43 seconds and 168 MiB for the whole index;
over a source repository's history, a layer of 64 small pushes on a published
root took 0.65 seconds and 88 MiB at 100 MB and 15.1 seconds and 254 MiB at a
gigabyte, against 0.94 seconds and 158 MiB and 11.5 seconds and 812 MiB for
the whole index — its time and memory following the history, never the few
hundred objects it adds. And a layer without a bitmap switches off the bitmap
of every layer beneath it. So the packs pushed since the last publication, the
**tail**, stay outside the chain, and git reads them beside it through each
pack's own index. A bitmapped walk of every object took 0.004 seconds with no pack
outside the chain, 0.03 with one, 0.09 with four and 0.35 with sixteen, and a
clone's pack ran in the same 2.7 to 3.0 seconds with four packs outside it as
with none (4.6 with no index at all).

**Each node writes its own tip commit-graph.** The tail's commits are outside
the published commit-graph layers, and a walk into them would go by date. A
commit-graph layer, unlike a multi-pack-index layer, costs only the commits it
adds and switches off nothing beneath it, so each node's background indexing
writes one over the chain, the copy's **tip**: git's own split write of just
the commits the installed graph layers lack, over exactly those layers, with
changed-path filters — on git 2.56.0, 0.01 to 0.03 seconds over a tail of 64
pushes, and 0.70 computing every filter of one of 90 909 objects outside a
gigabyte's published layer, whose every commit rewrites a directory of a
thousand entries. A copy holding no tip gets one at once, as a copy
with no graph does ([The commit-graph first](#the-cache-on-a-git-node)); once
the tail's packs change, the tip is written again by the background indexing's
usual pace, **whole** — over the installed layers alone and every commit of the
tail — rather than a new tip stacked on the old, which would grow without bound
while the tail waits for its publication. While the tail holds the packs the
tip was written over, nothing writes it again: the node records which packs
those were beside the tip, so neither a restart nor a full sync of a quiet
repository costs it a write. The old tip's file goes with git's
own write; a request that read the old chain file and opens the old tip after
that is told `unable to find all commit-graph files` and goes on through the
layers beneath it, as git's own split writes leave their readers, and the node
counts that as no damage. The tip is valid only over its exact base and the
commits it names: a placement, a cut, or a pack leaving that no compaction
retired takes it, and the background indexing writes it again at once over
what the tail then holds; a compaction's retirement keeps it, since a merge
drops no commit.

**A full sync installs the chain before it answers.** A full read of a
repository — a cold cache, a node's first sync of it after a restart, a
rebuild — keeps the longest part of the chain, from its root, that the copy
already holds whole: every file there at its row's size and held to its
checksum, as every file a previous process left is before it serves
([Damage and repair](#the-cache-on-a-git-node)). It fetches the rest, every
part of every layer queued in the sync's one download queue behind its packs,
so a cold copy's first answer is served through the bitmap for the layers'
bytes downloaded beside the packs' rather than after them. A layer's index and
bitmap are about 37 bytes an object it covers, about a tenth of a source
repository's packs. A layer is installed only over every pack it names, and
only in the rows' order: one whose packs the copy does not hold, or a git
newer than the node's wrote, is not installed, and nothing above it is either.
Measured on git 2.56.0 from an S3 fake answering each request 100 or 200 ms
late, sixteen downloads at once, one run each: the fifty packs of a compacted
repository came up under one published layer in 0.47 and 0.87 seconds,
against 0.43 and 0.83 with none, and under the twenty-one layers the default
`repo_max_bytes` reaches in 0.59 and 1.09 — about one first byte more, never
twenty-one; the 2 048 a repository may hold came up under one layer in 14.0
and 28.1 seconds, against 20.0 and 26.2 with none — the first pair a single
run each on a loaded machine, the second the layer's part adding seven per
cent; and each copy's first count of every object went through the bitmap,
in 5 to 66 milliseconds.

**A delta never downloads a layer.** A sync after a push that brings a new
publication hands the new layers to the node's **installer** and answers at
once. A root layer of the largest repository the defaults admit is some
2.6 GB, which inside a delta would hold every fetch waiting on that sync. The
installer runs behind the serving, on at most half the node's downloads —
sixteen parts at once, of the thirty-two the node runs at once — and one core
of the node's read lane for checking what it fetched, as the scrub takes, and
one for reading each placement through its bitmap, so the requests the lane
serves keep the rest of it through a mass install — a node handed one small
layer for each of 1 000 repositories, as after a restore, placed them all in
11.2 seconds at a 100 ms first byte and 17.5 at 200 ms: each repository's
first byte and about 80 ms of its own, sixteen at once. It fetches each layer in
chain order, and places them once all are held. Meanwhile the copy serves from
its chain as it was, the new packs in its tail. A copy that moves while its
layers download — a compaction's swap, a sync that reads the chain anew —
costs no second download: what was fetched waits in the node's temporary
space for the next placement, which takes it, or drops it once the rows have
moved past it.

**The chain holds its packs.** A publication retires the packs its own repack
merged, which the chain installed before it still names. Every pack an
installed layer names stays in the copy while the layer does, whatever its row
says — no sync, scrub or check takes one out — and leaves with the layer once
its replacement is placed. Dropped at the sync that retired them, they would
cut the chain, and its bitmap with it, until the replacement was fetched. A
layer the rows no longer name whose replacement cannot be placed for half an
hour is cut, and the packs it held leave with it
(`crewlet.code.cache.layers{outcome=cut}`, which counts every layer that
leaves a copy, a replaced one included).

**A placement is a swap, read before it is seen.** A copy a request may be
reading has its chain placed the way packs leave one ([When packs leave a
copy](#the-cache-on-a-git-node)): in its next directory, the layers it keeps
linked from the old one and the fetched ones renamed in, the chain files
written from the rows, and the copy moved into place whole. A copy nobody
holds — a node's first sync of it after a restart, a rebuild's — has its chain
placed in place. Either way the chain is read once through its bitmap before
any request sees it. A layer's files do not name the layer beneath them, so
one placed over the wrong base passes git's own `multi-pack-index verify` and
fails the first read that uses its bitmap (`invalid MIDX object position, MIDX
is likely corrupt`). That read is one bitmapped walk of every object from the
copy's refs, in the node's read lane: 0.004 seconds with no pack outside the
chain, 0.35 with sixteen. A placement whose read fails refuses the layers it
added as `corrupt`, and the copy serves from the layers beneath them. A read
that could not run — every slot of the read lane busy for ten seconds, or the
read past its deadline — says nothing of the layers: they are not placed and
not refused, and the installer tries them again a minute later.

**The maintainer's own layers.** The node that published a layer already
holds its files, so it links them into its copy rather than fetching them back
(`crewlet.code.cache.layers{outcome=adopted}`), as the node that received a
push links the push's pack. Files no install claims within the twelve hours
a write may take are removed: their publication never applied.

**Damage.** Every file of an installed chain is held to its checksum as the
other derived files are — before a copy a previous process left serves, by
every scrub, and by `crewlet code verify`. A layer's `.midx` and `.graph` are
held to the checksum their names carry, its bitmap to its own, and the two
chain files line for line to the layers the copy installed. `crewlet code
verify` also holds each layer to the row it was installed from: its packs,
its counts and the graph layers beneath it, which no checksum says. A layer
file that fails is fetched again by the copy's next full sync, which a scrub's
finding asks at once: before it answers, the sync fetches that layer and every
layer above it, since a layer is valid only over its exact base, and places
them as any layer is placed. A layer the sync cannot fetch is cut, with every
one above it, and the copy serves from the layers below. A chain file that
fails is written again from the rows, in place, without the node's tip. The
tip is held to the checksum its name carries as a layer's `.graph` is, and one
that fails is dropped — the chain file written again without it — and written
again at once by the background indexing, never fetched: no row names it. A
tip a previous process left is kept only over the very layers it was written
over, and is written again only once the tail it covered has changed. A git
process that names a
chain's files damaged drops nothing: it makes the copy's scrub due instead,
which holds every file of the chain to its checksum. A rebuild links into its
new copy every layer whose files pass and whose packs it linked, and fetches
the rest.

### When a repository is published

A repository's [maintainer](#who-maintains-a-repository) publishes in a loop
of its own beside its merges, so a publication of a large repository — minutes
of its build — never holds back a merge that keeps a busy repository under the
pack bound, the repository it is publishing included: its merges take the
packs pushed since its plan. Once a minute it lists the repositories holding a
pack no layer covers, least recently published first, and publishes each it
maintains whose tail is due:

- **Worth a layer**: the repository was never published, or its tail holds 64
  packs, or a sixty-fourth of its objects, or 90 909 objects — a second of walk
  for every clone at the slowest a walk is held to — whichever comes first.
  A tail of a sixty-fourth of a repository's objects added nothing measurable
  to a clone of it, where a sixteenth added a quarter, since a clone walks the
  tail rather than reading it off the bitmap. The bound in
  objects keeps the largest repository's tail small: a sixty-fourth of it
  would be nearly a million objects, some ten seconds of walk every clone
  pays.
- **Settled**: the tail has stood still 10 seconds, so a burst of pushes is
  published once; or it has waited 10 minutes since the maintainer first
  listed it, or since its last publication where that is later, so a
  repository pushed to without a pause is published as often as one that
  pauses.
- **Paced**: 10 minutes since the last publication, or ten times that
  publication's build where that is longer, so a repository's publications
  take at most a tenth of the time of the node that runs them. A build that
  did not land — it failed, it was refused, or its record is not known to
  have applied — paces its repository the same way from when it began, so a
  repository whose publication keeps failing costs its maintainer at most a
  tenth of its time too, and the loop goes on to the next repository rather
  than build that one again at the head of every pass.

**The plan is git's own.** A new layer covers every live pack the layers
beneath it do not: git writes a bitmap only over packs closed over what their
commits reach, and every pack live at one position is. It replaces the chain's
top layers geometrically — the next layer down is absorbed while the layers
above it hold at least half its objects, and regardless while the chain would
pass 32 layers — so the chain stays logarithmic in the history and each object
is written into a new layer a logarithmic number of times over its life. Where
more than 8 of the new layer's packs are not full, they are repacked first, as
a merge is, under its caps — over exactly those packs, in a scratch of their
own, so the repack's cost follows what it merges and never the history beside
it ([Compaction](#compaction)) — and held to a merge's rules, its count and
every output's bundle, before the layer is written over the outputs, so a
repack that fails them costs its own build and never a bitmap already
written. A layer whose files would pass 16 parts of 1 GiB is refused before it
is built (`layer_too_large`), and a plan that meets a layer the maintainer's
own installer has not placed yet waits for it.

**The build** runs in a scratch of hard links of the maintainer's copy whose
chain files name exactly the layers the new one is written over — on git
2.56.0 the same write with layers above its base still named took 196–200
seconds, against 1.03 with its base alone — never in the copy, and never over
a base the object store no longer holds: once the build is admitted, and
never before, so an attempt the node turns away asks the store nothing, the
maintainer asks the store for every part of the base, sixteen at a time —
about one first byte of S3's for every sixteen parts, where one after another
a large repository's chain of layers cost a second or two at every attempt —
and replaces a base the store lost, with every layer above it. It writes the
multi-pack-index layer with its bitmap, then the commit-graph layer with every
changed-path filter, in one slot of the node's maintenance lane and against
its background budget at the largest of its
steps' charges ([Memory](#the-cache-on-a-git-node)), and gives both back once
its last git process has ended, before the uploads. A tail holding no commit
gives git nothing to bitmap, and a layer without a bitmap would switch off
every bitmap beneath it, so such a layer is planned once more over one more
layer. The three files are cut into parts of at most 1 GiB and uploaded at
once with any repack's outputs — at most 32 uploads, 256 MiB of an `s3`
store's buffers — and proposed to the repository's committer as ONE compaction
carrying the layer, which it decides alone, holding it to the chain it was
built over and to every pack live at its plan that its base does not cover. A
store with no room refuses the publication `store_full` and holds the
repository back until it has the room, as it holds a merge back, never retried
as a blip. The maintainer then links what it built into its own copy, so it
downloads none of it, and every other git node's installer fetches the layer.
`code_publication_applied` logs each one, and `crewlet.code.publish.runs`,
`.duration`, `.refusals` and `.bytes` count them
([metrics reference](../reference/metrics.md)).

### When a layer is missing

A layer is never needed for a right answer: without the chain, git reads each
pack's own index — slower, never wrong. So a layer a node cannot install is
**refused**, never an error. The copy serves from the layers below it — the
longest part of the chain from its root that it holds sound — or in own mode
where none is left, its background indexing writing the copy's own index and
graph. The layer and every one above it wait for the rows to name another
chain. Each refusal is logged once a copy as `code_layer_refused`, with its
`outcome`, and counted by `crewlet.code.cache.layers`:

- **`missing`**: a part of the layer the object store holds nothing for. The
  rows may have moved to a newer publication since the node read them, and
  the store collected the old one's parts, so the node asks the log's head
  first: the layer is refused only while a read made after the store's answer
  still names it. While the head cannot be read, nothing is decided, and the
  layer is tried again after the wait a store that fails to answer gets.
- **`corrupt`**: a part whose bytes are not its row's, files the row does not
  describe — sections, packs or counts not the row's, a graph layer over
  another base — or a placement whose read through the bitmap failed.
- **`newer_git`**: written by a git newer than the node's own, which the node
  does not hand its git to read.

A refusal holds while the rows name that very layer record, and lives in the
node's memory, so a restart tries the layer once more. A failure that may
pass is no refusal. A store that fails to answer, a disk that refuses a
write, a missing part while the log's head cannot be read, or a placement whose
read through the bitmap could not run is tried again a minute later, then
twice as long each time up to half an hour — at most one try of a repository a
node each half hour once it keeps failing — while the copy serves as it is.
Each is logged as `code_layer_install_failed`, or as `code_cache_swap_failed`
where it was the placement's swap that failed.

**The maintainer repairs.** A part the store lost or damaged is refused on
every git node, the maintainer's own among them, so the maintainer's next
publication pass finds the refusal in its own copy whether or not anything was
pushed since, and publishes again over the layers it holds sound: the refused
layer and every one above it are replaced, at most once every 10 minutes, and
a repair that does not land paces its repository as any publication's build
does. An idle repository whose layer the store lost is repaired by the first
git node to be its maintainer, or at its next push.

**After a restore.** A backup carries no layer: a layer is derived, and the
backup neither copies nor asks after it ([Live, retired and derived
references](object-store.md#live-retired-and-derived-references)). So a
restore brings back rows naming layers whose parts the restored object store
may not hold. On `s3` it holds none; on the broker's bucket it holds what the
bucket's snapshot did. A part the store holds is held to its row and placed
like any other. A layer with a part it lacks is refused `missing` on every git
node, and each copy serves from the layers below it, or in own mode, until
the repository's maintainer publishes again — over every live pack, once, for
a chain the store lost whole: one walk of the repository's history
([Restoring](../guides/backup.md#restoring)). Nothing
alarms: a layer is not data, and the collector's audit never asks after one.

---

## Persons and their tokens

A person reaches a repository with git's own smart HTTP at
`https://<the API's address>/git/<name>.git`, on the node's `api.port`, and
with a token the engine already knows — their own machine token, minted with
[`crewlet iam token -login`](../reference/cli.md#crewlet-iam) or from the
dashboard's Account page and acting as them — given to git as the
**password** (git asks for
it, or a credential helper answers —
[`crewlet code credential`](../reference/cli.md#crewlet-code-credential) is the
engine's own, reading the token from the environment; the user name is
ignored). There is no second credential for git — no SSH key and
no deploy token — because the token is already how the engine knows who a
person is, and a second identity system would be one more thing to revoke and
audit ([ADR-0055](https://github.com/crewlet/crewlet/blob/main/adr/0055-a-person-reaches-git-with-their-own-bound-token.md)).

- **A credential acting as a human seat pushes as that person** — every
  person holds one ([humans in the org](humans-in-the-org.md)), so a person's
  own machine token pushes as them. That is the same principal the
  dashboard's writes and an assistant's operator tools act as, and git is the
  third way a person acts beside them. A pipeline that pushes does it with a
  Tier A token or a service account the identity directory binds to a human
  seat **nobody holds** — a person's seat is theirs alone, so a bind to it is
  refused — and its pushes are that seat's.
- **A credential bound to no seat fetches only** — a Tier A token or a service
  account the directory binds to none: a push is refused at its first request,
  before anything is sent, saying the token is not bound to a person.
- **Every push is recorded** as one `operator_acted` event with transport
  `git`, naming the caller exactly as every write does — the seat, its kind
  and the credential the push came through — with the repository and what
  became of the push, refused and failed ones included. A fetch is a read and
  is not.
- **`/git/` and `/code/` are guarded like every route**, reads included: a
  repository is the company's source. The token may arrive as git's Basic
  password on `/git/` and on no other route. Neither prefix is ever on the
  [public listener](../reference/api-endpoints.md#which-listener-serves-what) —
  it serves outside parties, who hold no token of the company's.

git is served by every node whose `node.roles` name `git` (an undeclared node
has every role), once the company sets `code.native`: the node builds the code
host at boot, or at the activation that brings its native runtime up, and
refuses to start one on a git older than 2.55 (the node floor), naming the git it
found; a person's git, CI's or a sandbox box's needs 2.40 or newer. A node
without the role answers every `/git/` request `404`. Every node answers the
code host's REST reads
(`GET /code/repos…`, [API Endpoints](../reference/api-endpoints.md#the-code-host-git-and-code)).
The [Code guide](../guides/code.md) walks through turning it on, declaring a
repository, binding a token and the first push.

---

## Coding boxes

A seat's [coding run](code-sandbox.md) reaches the code host from its box at
`/box/git/<name>.git`, served by every node whose roles name `git` — on the
[public listener](../reference/api-endpoints.md#which-listener-serves-what)
where one is set, beside the box's other two edges, the tool bridge
(`/mcp/{token}`) and the telemetry receiver (`/otlp/{token}`), and on
`api.port` otherwise. It holds no person's credential and must never hold one:
it presents its **run's own**, git's Basic user name `run` and, as the
password, a per-run token signed under the fleet's keyring in a key domain of
its own (`crewlet.git.v1`), naming the run's turn and its current launch. Any
node can verify one another node minted, and a keyring rotation in flight
keeps them working ([Secret store](secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost)).

**A run names its repositories, and the engine gives the box the rest.** A
seat calling `run_sandbox` names the repositories in its `repos`; the launching
node resolves them into the run's grants at the seat's level — and its
requester's, in a turn somebody woke — writes them on the run's row before any
box exists, and hands that launch, in its environment alone, the token and the
code host's address: the node's `CREWLET_BOX_GIT_URL`, a git node's public
`/box/git` or a balancer over the git nodes, since any of them serves any box.
The box's git is configured to present the token for that address only, and
the engine's own setup steps clone each repository before the coding agent
starts ([Code Sandbox → Repositories on this company's code
host](code-sandbox.md#repositories-on-this-companys-code-host)). A node without
`CREWLET_BOX_GIT_URL` refuses a launch naming repositories rather than give a
box one it cannot reach.

**The token is a backstop, the run's row the authority.** A token is valid
seven days (`host.RunTokenTTL`), longer than any job a person would leave
running, since a coding job has no wall-clock limit and an expiry would fail
its only durable output, its last push. What admits a box is its run's row on
the fleet's coordination store, read on every request: a run that ended, was
reaped or launched again — a second `run_sandbox` call of the same turn — is
refused the moment its row says so, whatever its token's expiry.

**What a box may do is what its run was given, held to what its seat may do.**
A run is given each repository by name, to `read` or to `write`; the seat's
own grant comes from the chart, judged by this node's current one ([Who may
read and push](#who-may-read-and-push)); a box does no more than both allow. A
run given a repository to read pushes nothing, not even under its seat's own
namespace. A run given it to write creates and fast-forwards branches where its
seat may, and never forces or deletes outside its seat's namespace — which
needs maintain — whatever its seat holds. The seat's grant is judged again by
the push's decide (rule 2b); the run's only here, since the decide reads no
run row.

**A box working for a colleague does only what that colleague could.** A run
launched in a turn a colleague seat woke — its ask, its message — names that
seat on its row, and the box is held to the colleague's own grant from the
same chart as well as its seat's, as the seat's own code tools are in that
turn ([Reads made for somebody else](#reads-made-for-somebody-else)): it
fetches no repository the colleague could not read, and pushes nothing outside
its seat's own work that the colleague could not push — a colleague who
only reads a repository can ask a seat to change it on the seat's own branch,
`seats/<handle>/…`, never on `main` or a `release/*` branch the seat itself
could move. The seat's own work — the branches under its own namespace, and
every branch of a repository it owns, such as its scratch repositories — stays
its own, which anybody who can message it steers. A run a person asked for, or nobody, is bounded by its seat
alone: a person maintains every repository.

**The admission**, in this order, each refusal in the one line git prints:

| Step | Refused | Answer |
|---|---|---|
| The credential | none presented (git's first request never carries one), a user name other than `run` — or `check`, which a [check's box](#check-boxes) presents and is admitted by its own steps — a token this fleet did not sign for the user name presenting it — forged, expired, under a key this node's `secrets.keys` does not hold, or a check's token presented as `run` | `401`, a challenge whose sentence names the box's token, never an API token |
| Its requests in flight | sixteen of the run's requests already in flight on this node — twice the eight repositories a run is given | `503 run_busy` with `Retry-After`, before the run's row is read: a box's own burst waits on itself (below) |
| The run | its row cannot be read | `503` with `Retry-After`: the credential was good, and nothing about it is known to be wrong |
| | no such run, a launch the row has replaced, a run whose job has left its box | `403` |
| The run's grant, by the name it was given | a repository the run was not given | `403 not_granted` — before anything of the code log is read, so a box guessing names costs the log nothing |
| | a push of a repository it was given to read | `403 read_only` |
| The stop, for a push | a seat a person stopped (below) | `403`, naming who stopped it; `503` on a node that has not yet read the pauses |
| The repository | not found, deleted or purging, or archived for a push, as a person's request is — decided through the code log's barrier, never on the box's own read, while the run's [re-syncs](#a-boxs-re-syncs-and-requests-in-flight) last; past them, on its run's floor, a not-found saying so. A name a rename left redirecting is not refused: the box is served the repository under its new name, a push included, since the box's remote is the name its launch gave it | as a person's |
| | a name renamed away from the repository the run was given and since taken by another | `403 not_granted` — call `run_sandbox` again |
| The seat's grant | this node holds no chart yet, or is confirmed behind the company's configuration | `503` (`no_chart`, `behind_config`): the configuration it failed to apply may be the one that revoked the seat; the box retries, and `/ready` already drains this node |
| | a fetch of a repository the seat reads nothing of — a `visibility: unit` one outside its unit — or, for a run working for a colleague, one that colleague reads nothing of | `403 not_permitted`, naming whose grant reads nothing |
| The run's hold, for a push (below) | the run store could not be written | `503 holds_unwritten` with `Retry-After` — the push is never received unheld |
| | the run already holds the most repositories and branches one run may (1 024) | `403 holds_full` |
| A push's commands, each | a command the seat's grant refuses, or — for a run working for a colleague — one outside the seat's own work (its own namespace and the repositories it owns) the colleague's grant refuses | `not_permitted` on that ref, saying what the seat or the colleague holds and what the command needs |

**A box's push holds the seat branches it pushes.** A seat's branch — a ref
under `refs/heads/seats/<handle>/` — that a coding run's box pushes is what the
parked run depends on, so before anything of the push is received, stored or
proposed, every such branch the push names that the run's row does not hold
yet is added to the row's holds, in one write: the same holds the run's turn
keeps for the branches it read or wrote through a code tool and every ref its
launch resolved, kept until the run ends and its row is removed. A branch the
row holds already costs a later push nothing. A box's fetch holds nothing: its
checkout is its launch's grant, held at the launch.

A refusal before a token this fleet signed is logged at debug
(`code_box_challenged`): `/box/git/` is a public route, and a line anybody who
can reach the listener prints at will is a log flood. Every refusal after one
is logged `code_box_refused`, and a `503` `code_box_unavailable`, each with its
reason, the run's turn and launch, the seat and the repository — never the
token.

**A node serving none of the code host tells a box why only once its token
verifies.** Where a node serves none of it now — the company hosts no code;
the node is applying the configuration the fleet activated, could not apply
it, or could not read it; its runtime is owed a restart — a person's `/git/`
request is told which, in the `404` or `503` [git prints](#what-git-prints-and-what-it-means).
`/box/git/` is public, and that reason is the deployment's state, so a box's
request is told it only once it presents a run token this fleet signed: any
other — no credential, a person's, a token that does not verify — is
challenged `401` in exactly the words the admission's first step uses, and
logged as it logs one (`code_box_challenged`, under the component `api`),
whatever the reason. Every node verifies the token, its code host built or
not, since every node holds the keyring it is signed under. A box's git
loses nothing: it asks its credential helper at the challenge, as on every
first request, and is told the reason on the next.

**A box reads at its run's session.** A person's reads of refs go through the
code log's barrier; a box's are read at its run's **floor** — a position on the
code log at or past everything its box has been shown and has pushed — with no
barrier, so a fleet of boxes adds nothing to the log's barrier budget. Every
answer that names refs — a protocol v0 advertisement, an `ls-refs`, a push's
advertisement — raises the run's floor to the copy it is served from before
its first byte, and an applied push raises it to the push's row before `ok`
is written. That is what keeps a box consistent across nodes: git builds a
push's thin pack against the refs that push's own advertisement named, and a
balancer may send the advertisement to one node and the push to another; raised
at the advertisement, the floor makes the second node's copy reach every base
the pack deltas against, where it would otherwise fail with git's `missing
necessary objects`. The raises at an `ls-refs` and a v0 advertisement mean a
box never sees a branch go back between two nodes, and a box reads its own push
through any node at once. A v2 clone takes at most one raise, a push at most
two; a raise the run store refuses answers `503`, and the box retries. A
fetch whose wants the node's copy lacks, and one past its repository's
request bound, each read through the barrier once — rare, as a person's do,
and paid from the run's re-syncs (below).

**What a box's request costs its run's record.** Every request a box makes —
each lazy fetch of a blob into a blobless clone among them — reads its run's
record once, from the stream leader of the bucket the fleet keeps runs in,
and each raise of its floor is one compare-and-set of that record. So the
record holds the run's facts alone: the job's suspended conversation and its
tool log are kept beside it, never on it
([Code Sandbox](code-sandbox.md#the-runs-record-and-what-is-kept-beside-it)).
Measured on three members with every bucket at three replicas, from a member
not leading the runs' bucket, for a job keeping half a mebibyte of
conversation and a full bridge log of 200 calls: the record is 2 983 bytes,
its 2 KiB brief among them, and a read takes 0.15 ms and a raise 0.42 ms at
the median (0.21 and 0.88 at the 99th percentile). With the conversation and
the log on the record, as they were, it was 1 046 412 bytes, a read took
8.4 ms and a raise 16.4 (11.4 and 22.1) — and every listing of runs and every
flip of one moved the same mebibyte (`BenchmarkABoxRequestOnItsRun`,
`internal/coord/kv`, thirty operations each on two E-cores of an Intel Core
Ultra 7 265U).

**Its transfers are its run's.** Every box's git is charged to the `run` class
of the [fair share](#fair-share), keyed by its launch, and boxes together hold
at most **half** the transfer lane, so a burst of coding runs cloning — or a
box reading a gigabyte slowly — never leaves a person's push waiting for a
slot. Every box is one **spool credential**, `runs`: boxes together hold at
most one credential's share of the node's room for large fetch requests, and a
person's spooled fetch never waits behind them.

**A box commits nothing as the engine.** The engine's committer,
`Crewlet <engine@<code.identity_domain>>`
([who a commit names](#who-a-commit-names)), marks the commits the host built,
and a box's git can write it into any commit it makes. So a box's push whose
pack holds a commit committed as the engine — the address compared in any
case, `Engine@Seats.Invalid` too — is refused `forged_committer` on every ref
it sends, naming the commit, and nothing of it is uploaded: the receiving node
reads the committer of every commit in a box's pack before the pack's upload
begins. Every commit in the pack, not only those the updates that land
reach: a pack is stored whole, so a commit sent with an update the host
refuses — a push to a branch that takes a person's push alone — or one no
update reaches would be in the repository all the same. A commit the
repository already holds is no forgery — a box's old checkout pushing back a
branch deleted since, the engine's own tool commits on it — so each commit
committed as the engine is looked up in the node's copy of the repository,
and only one the copy lacks is refused. One push may bring back up to 1 000
of them; a longer history is refused with the remedy of pushing it in parts,
since a part that landed is reached by a ref and the box's git does not send
it again. A refused push stores and proposes nothing, and its `code_push` line
names the refusal. A person's push and an import are never read for their
committers: an export imported again carries the engine's commits as git
carries any.

**A stopped seat pushes nothing.** A person's pause with `stop_running` is the
emergency stop, and a detached coding run is not fenced by it — it outlives its
turn by design — so its box's push is refused, at the advertisement and at the
push itself, naming who stopped the seat, until the seat is resumed
([Pausing a seat](agent-runtime.md#pausing-a-seat)). Its fetches are served: a
read changes nothing, and a box winding down may need one. A pause without
`stop_running` stops new work only, so a running job's pushes land. A push
admitted before the pause landed and decided after it lands, as a push of a run
that ended between its admission and its decide does. The same stop, read the
same way, refuses the run's calls of the seat's tools that write through the
[tool bridge](code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box),
so a stopped seat's run changes nothing by either edge until it is resumed.

**Neither transport takes the other's credential.** On `/box/git/` the guard
reads no credential at all — a person's token presented as a bearer, a
signed-in browser's cookie and an API token as the Basic password each admit
nothing, and the challenge names the run's token — and on `/git/` a run's
token is the guard's `401`, as any credential it does not know is.

**What is recorded.** A box's push is no `operator_acted` event — no person
acted — and its push row is its record: the seat as an agent, the run that
made it (its turn, its launch, its work item, its model and its coding agent),
received over git. A refused one is its `code_box_refused` or `code_push` line.
Those rows are also how the seat learns what its run delivered here: when the
run is collected, the engine reads the pushes made under its launch to the
repositories it was given — one linearizable read a run, its later pages at
`session` at the first one's position, scanning those repositories' pushes
since the launch rather than an index every data node would keep for every
box push — and hands the seat each ref as it ended
([Code Sandbox → What a run pushed is reported for it](code-sandbox.md#what-a-run-pushed-is-reported-for-it)).
A box's fetch of a `visibility: unit` repository is recorded under its seat
([Who read it](#who-read-it)). A push the decide refuses after its bundle was
stored — `push_quota` among the reasons — leaves that bundle named by no row,
and the object collector removes it after its grace: the relay checks no quota
first.

**A box's commits carry trailers by convention, and the host reads none.** The
coding clone's commit hook adds the seat, the job and the work item to every
commit a box makes, as trailers. The box can drop them, change them or write
any trailer it likes, so nothing on the host reads them: a push is recorded by
the run its box was admitted as, whatever its commits say, and that record is
the provenance. A trailer is a note in git for where the record does not reach
— a mirror of the repository elsewhere, and a commit older than the push
history ([Code Sandbox → Commit trailers](code-sandbox.md#commit-trailers)).

### A box's re-syncs and requests in flight

A box runs code nobody vouches for, so what it can take of a node and of the
code log is bounded by its run, never left to the box to keep small.

**Its re-syncs at the log's head.** Every read of a box's request that goes
through the code log's barrier — a fetch whose want the node's copy lacks, a
request past the bound its admitted row set, a repository its run was given
that its floor would refuse, and every read of a run whose row names no floor
— appends a barrier to the code log, which the log's census sizes at 125 a
day for each agent seat. Each is paid from the run's **bucket** on the node:
**16** at once, one more every **11.5 minutes** (the census's 125 a seat-day),
and paid as well from its **seat's** bucket on that node, of the same size —
the one its seat's code tools on that node pay too (below) — so
a seat with several coding runs live on one node shares its census term
there. A barrier a fetch takes raises the run's floor to it, so the box's next
request, on any node, pays none for the same objects. Past either bucket, the request is answered at the run's floor — what it has
been shown, which holds everything it pushed — with no barrier: a want the
floor lacks is answered as upload-pack answers one it cannot serve, which git
prints as

```text
fatal: remote error: <oid> is not in <repo> at the position this run has been shown; it has not been pushed yet, or ask code_resolve (not re-judged at linearizable: this run's re-syncs are spent until <instant>)
```

— `this seat's` where the seat's bucket refused it — a request past its bound
is held to the bound its admitted row set, and a repository the floor does
not hold is refused saying it was not judged again. Persons are never
charged: their reads of refs go through the barrier by right, and the code
log's census counts them as its clients. The buckets are each node's own, in
memory, so a box whose requests spread over several git nodes has a burst on
each, and the log's barriers from boxes are bounded by the fleet's git nodes.
`crewlet.code.box.resyncs{outcome}` counts them, `taken` or `spent`, and every
`spent` answer is logged `code_box_resyncs_spent` with the bucket's scope (the
seat), its principal (`run <launch id>`) and the object.

A seat's code tools are bounded the same way on the git node that answers
them: a [server-side read](#server-side-reads)'s one second look at the log's
head, and an [engine write](#repositories-made-by-tools)'s one re-judgement of
a refusal there, are paid from its **turn's** bucket and its seat's — the git
node's ONE set of buckets, which its boxes pay too, 16 and 11.5 minutes as a
box's. The seat's bucket on a node is one, whether its boxes or its turns
reach the log's head through it: the census gives a seat one term, and a box
looping on what nobody pushed spends what its turns' tools have left on that
node, and the other way round. Past them the
read is answered from the copy at the seat's floor, and the write's refusal
from its session, each ending `(not re-judged at linearizable: this turn's
re-syncs are spent until <instant>)`.

**Its requests in flight.** At most **16** of one run's requests — twice the
eight repositories a run is given — are in flight on one node at once; past
them a request is answered `503 run_busy` with `Retry-After` before anything
is read for it, and logged `code_box_unavailable`. A place is taken when the
run's token verifies and given back when the request ends, its client hangs
up or the node stops. The transfer lane's run class orders which box is
granted a slot next, and the spool's one box credential bounds what boxes
hold of it together; this is what stops one run holding a node's every
admission at once. `crewlet.code.box.inflight_refused` counts the refusals.

---

## Who may read and push

What a principal may do to a repository is **derived, never stored**: there is
no list of who may push to which repository, because such a list is a second
org chart, and it drifts from the first the day a seat moves units. It is
computed every time it is asked, from the repository's row and the chart the
deciding node runs, by one rule
([ADR-0058](https://github.com/crewlet/crewlet/blob/main/adr/0058-a-repository-grant-is-derived-from-the-chart.md)).

| Who | What they may do |
|---|---|
| A person — a credential acting as their human seat | **maintain** every repository: create, update, force and delete any branch, create and delete tags |
| A credential bound to no seat — a Tier A token or a service account | **read** every repository: fetch and clone, never push |
| A seat | the **lesser** of its tier and its relation to the repository, below |

A seat's **tier** is its `role.code.access` and caps what it may do anywhere:
`read_only` reads, `review` reads and reviews, `full_access` maintains; a seat
that sets none reads. Its **relation** is what the chart relates it to the
repository by, the first row that holds:

| The repository's owner | The seat | Relation |
|---|---|---|
| a unit | the unit's effective lead (its own, or the nearest above it) | maintain |
| a unit | sits in the unit or in a unit below it | write |
| a unit | leads a unit above it | read |
| a seat | is that seat | maintain |
| a seat | leads that seat's unit or a unit above it | read |
| a `visibility: company` repository | any other seat | read |
| a `visibility: unit` repository | any other seat | **none** |

So a `full_access` seat in the owning unit writes — creates and fast-forwards
branches, creates tags — and only the unit's lead, at `full_access`, also
forces and deletes. A `review` seat reads whatever its relation. Below
`full_access` a seat changes no repository at all, so it is not offered the
[code write tools](../guides/tools-and-mcp.md#the-companys-code) either: a
`read_only` or `review` seat reads with the code tools, and is told nothing
about writing with them.

**A seat's own branches.** A `full_access` seat owns
`refs/heads/seats/<its handle>/` on every repository it may read, and may do
anything there — create, update, force, delete — whatever its relation: that is
where a seat that only reads a repository pushes its work, in place of a fork.
A branch there that nobody has set for 14 days is deleted by [the ref
lifecycle](#the-ref-lifecycle) unless something still depends on it.
The namespace sits under `seats/` and never at the handle alone, because a
handle is a role name's slug (`docs`, `release`, `qa`) and people name their own
branches under exactly those prefixes. `refs/heads/seats` itself and
`refs/heads/seats/<name>` as a branch are nobody's: no ref is ever created
beside a directory of its own name ([A ref and a directory of
refs](#a-ref-and-a-directory-of-refs)),
so a branch named `seats` would block every seat's namespace on its repository,
and one named `seats/<name>` the namespace of the seat with that handle — one
the chart has now or may add later. Any push of either is refused
`reserved_ref`, a person's included, and neither is taken as a repository's
`default_branch`. Another seat's namespace takes maintain.

The [protection rules](#protection-rules) are judged after the grant and are
never skipped for one: a grant decides who may move a ref, a protection what
any push may do to it.

### Visibility

A repository's `visibility` decides one row of that table — the last, for a
seat the chart relates to the repository in no other way — and nothing else:

- **`company`** (unset): every seat reads it. A seat outside the owning unit
  reads its code and pushes to its own branches there.
- **`unit`**: only the seats the rows above relate to it read it — the owning
  unit's members and the leads above them, or the owning seat and the leads
  above its unit. Every other seat reads **nothing**: the repository is
  absent from its listing and its changes from every listing of changes, and
  its detail, its push history, a listing of its changes and any one of them
  are refused `not_permitted`, naming the visibility and who does read it
  ([Reading changes](#reading-changes)). That a
  `visibility: unit` repository exists is no secret — its name is in the
  company document, so asking for one that does not exist is still answered
  "not found" — and what it holds is.

**Visibility narrows seats only.** A person reads every repository whatever it
says, and so does a credential bound to no seat: the people who run the company
are never shut out of its code by a setting that exists to keep one team's
seats out of another's. A seat's own branches are only on the repositories it
may read, so a `full_access` seat outside the unit has none on a `unit`
repository. What does read a `unit` repository's contents — a person, a
credential or a seat — is recorded, for an admin to ask
([Who read it](#who-read-it)).

A `unit` repository owned by a unit with **no lead** — neither its own nor one
above it — is read only by persons and the unit's members, and `crewlet
validate` warns about it at its `visibility`. Changing a repository's
visibility is a settings change like any other: one record at its activation,
and every listing and read after it judged by the new one.

A person's standing reads no chart at all. A seat's is judged from the chart of
the node that answers or decides — the data node serving a listing or a read
holds the asker to its own epoch's chart — so two nodes on different
activations may decide one command differently while one of them has not yet
applied the other's. A node that holds no chart yet, from its start until it
installs its first company configuration, judges no seat: it refuses a seat's
read as a node that cannot answer it, and the read is asked of another data
node — never answered that the seat may read nothing. Nor does a node
[confirmed behind the company's configuration](control-plane.md#where-the-gate-sits)
— shed or stuck, about a minute after an activation it failed to apply, at
worst — judge a seat: it answers a seat's listing, read or ref `behind_config`
even when it is the last data node left to ask, because the revision it failed
to apply may be the one that revoked the seat's grant, so the read is answered
by a current node or by nobody; a person's read it answers as before, asked
last. A seat reads a repository through its code tools
([Server-side reads](#server-side-reads)) and pushes through them, each
change one push the code host builds for it ([Engine-built
commits](#engine-built-commits)), or from its coding box
([Coding boxes](#coding-boxes)) — never from a git client of its own
([What it does not do yet](#what-it-does-not-do-yet)).

### A seat's push

A seat's push is decided by its repository's [committer](#the-committer), as
a person's is, and held to four rules a person's is not — each judged from
the chart and the calendar of the data node that decides it. A seat pushes
through its code tools, each change a push the code host builds for it
([Engine-built commits](#engine-built-commits)), and from its coding box
([Coding boxes](#coding-boxes)); these are the rules the committer holds
every such push to, whichever way it came.

- **Its grant, at the decide.** Every command is judged against the seat's
  grant (the table above) before anything about its ref is read, and one the
  grant does not cover is refused **`not_permitted`**, with the grant's own
  sentence. It is a verdict on the command, recorded with the push's other
  commands where any of them lands, as every such verdict is: who tried to
  move what past their grant is a fact the push history keeps.
- **A protection takes a person's push alone.** `direct_push: persons` — what
  every protection rule says unless it says `none` — lets a person push to the
  ref and **nobody else**: a seat's push to it is refused **`protected_direct`**
  whatever the seat's grant, its unit's lead included, and `direct_push: none`
  refuses everybody. A seat's work reaches a protected branch when a person
  pushes it.
- **A seat outside the owning unit fills at most half.** A seat whose
  relation to the repository is below write — one that reads it and pushes
  only into its own branches there — creates a ref only while the repository
  holds fewer than 50 000 live refs, half of the 100 000 one may hold, and
  lands a pack only while the repository's packs stay within half of
  `code.limits.repo_max_bytes`. Its own branches are on every repository it
  reads, so without the bound one seat of another unit could fill a
  repository its owners work in and refuse them every push. The other half is
  the owners': a person, and a seat of the owning unit, are held to the whole
  of each. The seats outside share their half, with no bound on any one of
  them: once a repository's packs are compacted their bytes belong to no one
  seat, so a bound per seat could hold its refs but never its bytes. The
  refusals are `repo_refs` and `repo_size`, each saying which half it met.
- **A daily quota.** A seat lands at most `code.limits.pushes_per_seat_day`
  pushes in one day on the company's calendar
  ([`timezone`](../getting-started/configuration.md#the-companys-clock)),
  every repository together: 1 000 unless set, some thirty-three times the
  reference company's thirty pushes a seat a day — a bound on a seat pushing
  in a loop, not a budget. Past it a push is refused **`push_quota`**, whole and
  **unrecorded**: nothing of it is decided and no record carries it, because a
  refusal recorded for every push of a runaway seat would cost the log the
  record a push the quota exists to bound. Its sentence names the instant the
  count starts again, the company's next midnight. A person's pushes are never
  counted. A push the quota refuses has already stored its pack; nothing names
  it, and the [object collector](object-store.md#collection-and-audit) deletes
  it a day later — the quota bounds what a seat costs the log, not its git
  work.

The quota counts a seat across repositories, and each repository's committer
counts what the code log has applied on its own node, so a seat pushing to
several repositories in one instant may land past the bound by its pushes in
flight at that instant, less one — never more, and never again that day: once
the count is past the bound, every later push counted on that day is refused.
Each committer counts on the day its own clock reads, so while two data nodes'
clocks straddle the company's midnight a seat's pushes are counted on two days
at once. The earlier day's count is kept beside the newer one's, so a
committer whose clock has not reached midnight yet still holds the seat to its
day — as long as no two data nodes' clocks stand a day apart, which
[`clock_skew`](../reference/alarms.md) reports long before: it is raised by a
clock an hour from the fleet's. An exact count would put every push of a seat
working in many repositories behind one another, on one arbitration, to save a
few pushes on a bound of a thousand.

A data node that cannot judge a seat decides none of its pushes, and says so
about itself, so the repository's next committer decides the push under the
same push id: one that holds no chart yet, from its start until it installs
its first company configuration, and one [confirmed behind the company's
configuration](control-plane.md#where-the-gate-sits), which decides no seat's
push even as the last committer left to ask — the revision it failed to apply
may be the one that revoked the seat's grant, so the push is decided by a
current node or by nobody. A person's push it decides as before.

### Reads made for somebody else

A turn somebody woke — a person's message, a colleague seat's message or ask,
or a change either of them made on a work item, a page or a repository's
[change](#who-a-change-wakes) that wakes the seat —
is that somebody's errand, and a seat that read out for them whatever it may
read itself would hand a `visibility: unit` repository to anybody who can
message one of the unit's seats. So inside such a turn every code read the
seat makes is decided again for whoever woke it: the **requester ceiling**
the [artifacts tools](artifacts.md) apply, here on a repository's grant.

| The turn was woken by | What the seat reads |
|---|---|
| nobody — a schedule, a watch, the seat's own work, the outcome of its own act (a workflow or a pipeline its own push failed), or the engine's own notice on a work item, such as one it reports unblocked, a change's home's own record — a conflict, a change ready to merge, a revision's facts — or an [alarm's wake](event-system.md#alarm-wakes) | what its own grant allows |
| a person of the company | what its own grant allows: a person maintains every repository, so they bound nothing |
| a colleague seat | what **both** grants allow: a listing leaves out every repository the colleague may not read, and a read of one is refused, naming the colleague and the visibility that shuts it out |
| a person who may not act in the company now — invited and not yet enrolled, suspended, or removed | nothing |
| somebody the company does not know — a chat sender no seat or person maps to — or a credential acting as no seat, such as an automation's change on a work item or a page | nothing |

A change on a work item or a page names who made it by the kind its record
holds, never by how the name is spelled: a seat's change, or a person's made
as the seat they hold, is that seat's, and the engine's own is nobody's. A
record about a repository's change names who made it in the routing its
writer stamped, a person by the human seat they hold, and its home's own
records name nobody. A
credential acting as no seat holds no seat a ceiling could be decided for, so
the turn its change wakes reads nothing, as a stranger's does. The seat's own
voice is never its requester: when the outcome of its own act opens a thread,
whoever speaks in it next is — a colleague or a person as above, and somebody
the company does not know as a stranger.

The same holds for the repositories a seat with the
[health tools](../guides/fleet.md#the-health-tools) reads through
`code_status`: its hot list and the repository it names are read as the code
tools read them, so in a colleague's turn the hot list leaves out what the
colleague may not read as well, and a stranger's turn is answered nothing. Who
woke the turn is asked only what a code read asks of them — the deployment's
view beside the repositories is the seat's own grant.

And for a [coding run](#coding-boxes) the seat launches in such a turn: its
repositories are resolved for whoever woke it, so a stranger's turn is given
none and a colleague's is given none that colleague could not read; the run's
row names the colleague, and its box is held to the colleague's grant on every
request it makes — every fetch, and every push outside the seat's own work —
its own namespace and the repositories it owns. A launch asking to `write` a
branch the colleague could not push is refused before any box exists.

The colleague's grant is judged where the seat's own is: by the node that
answers each read, from its own chart and the repository's row, and never
from a chart the turn read once, so an activation that takes the colleague out
of the owning unit stops the very next read made for it. A seat that may not
read the repository itself is told so about itself first, whoever it reads
for — that is the refusal whose remedy it can act on. A refusal made for the
colleague is not one a retry clears, and nothing the seat could be given
changes it: it is about what the colleague may read.

A turn whose requester the node cannot establish — the identity directory
out of reach — reads nothing and is told to try again, never read as a turn
nobody woke. A person's own assistant, which reads through the same tools,
calls them in no turn and reads for nobody but its caller. The ceiling bounds
and never grants: what the seat reads is still its own read, judged by its
own grant.

**The ceiling holds the turn somebody woke, and no turn after it.** Each turn
is decided for whoever woke that turn. A colleague the seat asks with
`a2a_ask` answers in a turn the asking seat woke, so it reads for the asking
seat and never for whoever woke the asker; the answer wakes the asking seat in
a turn of its own work, which nothing beyond its own grant bounds; and a work
item the seat files for itself is worked in a turn of its own too. Nor does
the ceiling reach what a seat already holds: its memory, or what it read in a
turn nobody woke. What a seat was refused for somebody can therefore still
reach them through a colleague's answer or the seat's own memory: the ceiling
decides what a turn reads for somebody, not what a seat may go on to tell
them.

### Writes made for somebody else

The same ceiling holds what a seat **changes** in such a turn, through the
[code write tools](../guides/tools-and-mcp.md#the-companys-code): a seat a
colleague outside a unit messages, or a stranger on a chat surface, changes
nothing of a repository they could not change themselves. Each write is
decided again for whoever woke the turn, at the relation the write needs —
moving a branch, creating one, deleting one, changing a repository's settings
or its protections — exactly as they would be decided asking it themselves.

| The turn was woken by | What the seat changes |
|---|---|
| nobody, or the outcome of its own act | what its own grant allows |
| a person of the company | what its own grant allows: a person maintains every repository |
| a colleague seat | what **both** grants allow: a branch the colleague could push, a repository the colleague maintains — and its own work |
| a person who may not act in the company now, or somebody the company does not know | its own work, and nothing else |

**Its own work is steered by whoever woke it**, as an agent's own artifact is:
anybody who can message a seat may ask it to fix its own work. That is every
branch under its own namespace (`seats/<handle>/`), every branch of a
repository the seat owns — its scratch repositories, and a project a person
handed it — creating its own scratch repository, and changing the settings or
the protections of a repository it owns. **What discloses, hands on or retires
a repository is never steered**, as sharing, transferring and trashing an
artifact are not: changing its visibility or its owner, renaming, archiving,
deleting or restoring it are decided for whoever woke the turn like any other
write. A colleague seat bounds every read the write makes too, so a write tool
shows nothing of a repository the colleague could not read.

The write's ceiling is judged where the write is decided — the git node that
builds an [engine-built commit](#engine-built-commits) and the data node that
decides a repository's create, settings or protection — from the same chart and
row as the seat's own grant, so an activation that takes a colleague out of a
unit stops the next write made for it. A refusal names whom the work is for and,
for a colleague, what that colleague may not do; a turn whose requester the
node cannot establish writes nothing and is told to try again. A person's own
assistant writes for nobody but its caller.

**A [change](#changes)'s writes are held the same way**, by the data node that
commits the repository, once the seat itself may make the write: opening,
editing, closing or reopening a change, asking for a review, reviewing,
commenting, and answering findings. For a colleague seat the question is the
**access** the write needs on the repository, never the relation beyond it:
`review` for a review and `read` for every other write. Who owns the change, who
may push its source branch and who wrote it are the seat's own to meet, and
judged for the seat alone. That is what lets a change's author ask a seat to
review the change, which its author may never review itself. For a stranger,
or a person who may not act in the company now, a seat writes nothing of a
change except its own work: a change it owns, an open of a branch under its own
namespace, and any change of a repository it owns. Every read a change tool
makes is held to a colleague as every code read is. **A change's
[rebase](#a-change-rebased) is a branch write**, so the git node that builds it
holds it as it holds every one: for a colleague seat, a source branch the
colleague could push, or the seat's own work.

---

## Server-side reads

A git node also answers questions about a repository's content without a
clone: a tree's listing, a file's lines, a diff — its per-file stat, or one
file's patch — how two commits compare (their merge base, which contains
which, the commits each has that the other lacks, and what one changed since
the merge base), a path's history ([A path's history](#a-paths-history)),
which commit last changed each line of a file
([Who changed a line](#who-changed-a-line)), whether a commit landed in a
branch and which push landed it (`landed_in`,
[Which push landed a commit](#which-push-landed-a-commit)), and what a change's
revisions are: a revision's facts against its target, where lines anchored in
one revision are in another, and what the change changed between two
([A revision's facts](#a-revisions-facts)).
It answers each with real git over its own
[cache](#the-cache-on-a-git-node), in the cache's read lane, from the copy it
holds, and names in every answer the push that introduced each commit the
answer names, with what its index of pushes holds.

**Who asks them** is a seat, through its code tools, and a person through
their own assistant, which the operator catalogue serves the same tools to:
`code_read`, `code_diff`, `code_compare`, `code_landed_in`, `code_log` and
`code_blame` read by commit id, beside `code_list_repos`, `code_get_repo` and
`code_resolve`, which read the rows — what a repository is, its branches, what
a ref points at now — at linearizable; and `code_get_change` reads a change's
rows at linearizable and its files, its patches and where its findings' lines
are by the commit ids its revisions recorded
([The company's code](../guides/tools-and-mcp.md#the-companys-code)).
A turn reads each repository it names once, and resolves the refs a call
names in one read — a comparison's two branches cost one barrier — so a seat
that resolves a branch once and reads by the commit it answers pays one
barrier for the repository and one for the branch, however many files it
reads. A file's lines — a blame's among them — and a patch reach the model
[fenced](agent-runtime.md#text-somebody-else-wrote), at trust `external`, and
so does every commit's subject a history or a blame names: a read by commit id
knows the bytes and not whose they are.

**By object id, never by ref.** Every revision a read names is an object id,
whole or abbreviated to 7 or more hex digits. What a branch or a tag points at
is a row, read at whatever level its reader asks; an object id names the same
object on every node at every position, so a read by one needs no level. An
abbreviation is expanded from the repository's objects alone, never as a ref
name — git's own reading of a short name tries it as a ref first, so a branch
or a tag named like an abbreviation would answer for another commit — and one
that two objects begin with is refused, asking for more of its digits.

**A path is data.** A path a read names reaches git on a batch command's
standard input, where it resolves to the id of the tree or file it names, and
every git process the read runs is handed ids, fixed options and numbers the
node formats alone — or, for a history and a blame, whose git reads no path on
its standard input, the path as the ONE argument after `--`, which git reads as
a path and never as an option, a history's as a literal pathspec: so no path
can be read as an option, and a path named `--output=x` is the path
`--output=x`. A diff's stat is filtered by the paths a read names after git has
answered, never by a pathspec, and so is an interdiff by the paths its
revisions touch.

**One page at a time, never cut.** Every answer is at most 56 KiB of JSON and
says where the next page starts: a listing's entries, a stat's files, a file's
lines, a patch's lines, a history's commits and a blame's lines are each whole
on the page that holds them, and a listing's total, a file's line count and a
diff's totals are the whole answer's on every page. A line of a file longer
than a page is served in pieces at character boundaries that concatenate to the
line; a line of a patch longer than a page is refused, naming the file's lines
to read instead. A binary file is answered by its size, as git calls one
binary: a NUL in its first 8 000 bytes.

**Who may read** is judged by the git node that answers, from its own chart,
by the rules of [Who may read and push](#who-may-read-and-push): a person reads
every repository, and a seat what its tier and its relation allow, so a
`visibility: unit` repository is refused to a seat outside the unit,
`not_permitted`, naming the visibility. A read a seat makes for the colleague
whose ask woke its turn is held to the colleague's grant as well, from the same
chart and row, and refused where the colleague could not read it
([Reads made for somebody else](#reads-made-for-somebody-else)). A deleted or
purging repository is answered not found, with what became of it.

**A git node that cannot answer says so about itself**, and another git node
may answer the same read — nothing about the repository's content is concluded
from such an answer:

| Unready | What it means |
|---|---|
| `syncing` | the node is bringing its copy up: it could not be brought to what the read needs within two seconds, or before the read's answer fell due — a copy downloading the repository, whose download goes on after the read stops waiting — or a git process over the copy reported damage, which the cache is repairing, or a sync failed on what every git node's copy is filled from (the code log, the object store, the repository's rows, or a repair the cache will not repeat because reading them whole did not end the damage), which another node's download would only meet again |
| `share` | the node holds its copy and is busy: its seats' git work holds its whole share of the read lane (below) and none came free within two seconds, its read lane stayed busy for ten, or the read's waits for either, or its own git, ran past the instant its answer fell due |
| `no_chart` | a seat's read on a node holding no chart yet — at boot, before its first apply — which cannot judge a seat, as opposed to judging it has no access |
| `behind_config` | a seat's read on a node confirmed behind the company's configuration (shed or stuck), which judges no seat's grant from the chart the fleet replaced |
| `node_failed` | the node failed the read for a reason of its own — its sync of the repository failed with nothing the code log, the object store or the rows said (its disk refusing a write, its cache stopping), or one of the short checks of what the read names ran its whole bound, which no healthy node takes over a few object ids |
| `not_held` | the node holds no copy of the repository and the read did not name it the one node of its walk that may fill one ([The home git node](#the-home-git-node)), so it started nothing |

A person's read reads no chart, so neither `no_chart` nor `behind_config`
applies to one. Which of them a node gives decides more than the sentence: it
tells the read's walk whether a copy is coming on that node, and so whether
the next node may fill one ([The home git node](#the-home-git-node)).

**Fresh enough by construction.** A read is answered from the copy as it
stands, or brought to the reader's floor where it holds one; an object the copy
lacks is looked for once more after a sync at the log's head — the reader may
know of a push this node has not synced yet — and only then answered as no
such object.

**Ten seconds of git a read, and too large only past them.** A read's own git
— a listing's one process, a patch's lookup and its diff, a comparison's two
walks and its stat — shares one budget of ten seconds of git run time, each
process given what the ones before it left. A read whose git runs all of it is
too large to answer, refused naming what to narrow — a directory below, fewer
lines, a narrower range, commits closer together — since any git node would
take as long. A comparison's walks go by generation number on a copy the node
has maintained, which holds a commit-graph — 13 ms for the middle of a
14 080-commit history against its tip — while a copy just filled, before its
maintenance, walks by date and may run the budget out over a long history; the
same comparison answers once the copy is maintained.

**Answered inside its asker's attempt, never late.** Every read is answered
within the router's git read attempt, seventeen seconds — the share's two
seconds, two two-second syncs, the read's ten seconds of git and a second for
the answer's trip back — from the node that asked it in process as from every
other. The node serves the read until a second before the attempt ends, and
every wait it makes ends there: the copy's sync, a unit of the share, a slot of
the read lane for each process, each process. Those slots and the short checks
of what a read names are not in the sum, so under load they come out of the
attempt, and a read they hold up past it is answered `share` while its asker
still listens: the read goes on to the next git node, and the busy one is not
taken for a node that is gone. Never too large: a read cut short had less than
its ten seconds, and a node with the time would answer it.

**Seats' reads hold at most half the read lane.** The read lane — one core a
slot, `git.max_processes` of them — is also where a person's push scans its
objects, where a sync reads the refs it installed, and where a fetch checks
the objects it wants, each waiting in turn for at most ten seconds before it is
refused busy. So the git a seat's read runs takes a unit of the node's **seat
git-work share** — half the lane, at least one — and gives it back the moment
that git ends: a read waiting on its sync holds none, and the other half of the
lane stays the persons' and the node's own. The build of an [engine-built
commit](#engine-built-commits) takes its unit of the same share — one share a
node, so seats' reads and writes between them never hold more than half —
and gives it back before its push lands. What the share holds is the read's
or the build's own git; the short checks of what either names, before it,
wait in the lane's queue like any short read. `crewlet.code.seatwork.held` is
the share held now and `crewlet.code.seatwork.waits` every unit asked for, by
the `work` it was for and whether it came free in time
([Metrics](../reference/metrics.md)).

A read's git process that reports damage to the copy is never answered from,
even where git answered anyway — it reads around a damaged index from the
packs and exits as if it had not — and the cache repairs what it reported
while other nodes answer.

### A path's history

`code_log` asks for the commits that changed a file or a directory — or every
commit — newest first from a commit: one `git log --topo-order` over the copy,
walking by generation where the copy holds a commit-graph, and reading the
graph's changed-path filters, so a commit that touched nothing under the path
is passed over without a diff. Each commit is named with its parents, its
author and address as the commit states them — never a `.mailmap`'s, so the
answer is a function of the commits alone — its two dates, its subject (the
message's first paragraph) and the push that introduced it. The path is the
history as git keeps it: a file renamed is listed under each name for the
commits that touched that name, the rename among the old name's, and a file
deleted since keeps the history it had.

**A literal, matched on the node.** `grep` keeps the commits whose whole
message holds its words, in any case, matched by the node over each message as
it streams by — never handed to git as a pattern — so a message of any length
is searched in memory the size of the words, and a dot is a dot. `since` keeps
the commits committed from a time on, as git's `--since` does: the walk stops
at a commit committed before it on each line of history it reaches.

**A page ends where the history says.** A page holds as many commits as
`limit` asks and its 56 KiB hold — every subject counted fenced, as the tools
send it — and ends before the next; its cursor is how many commits of the walk
the pages before it covered, so the next page's walk skips them. A `grep` page
ends at its `limit`-th match rather than walking on to find one more, since
past it every later message is read only to learn whether another page
exists, and where the words are rare that is the rest of the history. `scanned`
is how many commits a page covered, the ones `grep` passed over included, and
`complete` that the walk reached the history's end — which a `grep` page that
filled its `limit` never says, so the page after it may list nothing and be the
complete one. A walk that runs the
read's ten seconds of git — a `grep` that matches little in a long history —
answers what it found with its cursor past it, never an error and never a
claim that nothing more matches; one that reached no commit at all in that
time is too large, naming the narrowing (a path, a later `since`).

**A record no message can forge.** Every field of a commit ends in a NUL
byte, which no message holds — the code host's own check of every push and
every engine-built commit refuses a NUL in a commit — so a message holding
whatever other bytes it likes, a forged commit's worth of them included, is
read as the one commit it is. A record that does not read as git writes one
fails the page, logged `code_git_unparsed`, and is never answered.

### Who changed a line

`code_blame` asks which commit last changed each line of a file at a commit:
one `git blame --porcelain` of a range of at most 500 lines, its path the one
argument after `--`. Each line comes with its commit, the line it was in that
commit, and the path it had there where the file was renamed since; each commit
once a page, with its author as the commit states it — a blame in a bare
repository would read the `.mailmap` at the default branch, which the node
turns off — its date, its subject (the message's first line) and the push that
introduced it. A blame's cost is the walk of the file's history for the lines
it names, so a range is read in pages by one blame: the run that cuts the first
page cuts every page of the range, and the rest are answered from the
[memo](#the-memo). A range that begins past the file's end is refused naming
how many lines the file has; a path that names a directory or nothing is
refused; an empty file has no lines.

**Described, never cut.** A subject past 4 KiB, an author's name or address
past 1 KiB and a line past 8 KiB — each measured as the tools send it, a
control byte six bytes and a fence's marker its escape — are answered absent,
beside their size (`subject_bytes`, `author_bytes`, `author_email_bytes`,
`text_bytes`), in a history and a blame alike, and so are a merge's parents
past 64 (`parent_count`): so the widest commit, and the widest line with its
commit, always fit a page, and no page carries half a field read as the whole.

### The memo

Every page a git node cuts for a server-side read is kept beside its copy —
one file an answer, in the copy's `crewlet/memo` — and the next read that asks
for the same page is answered from that file with no git run, its answer
saying so (`memo: true`). A page is a function of the object ids it names, the
read's arguments and its cursor alone, so it is never stale, only absent; what
an answer carries of the moment — the copy's position, the push that
introduced each commit it names and what the index of pushes holds — is added
to a page from the memo as to one just cut. A read naming whole ids asks the
memo as soon as its grant is judged, before any git runs, so a seat outside a
`visibility: unit` repository is refused before the memo is asked; one naming
an abbreviation asks it once the abbreviation is expanded. And the memo is
the copy's: an answer in it was cut from objects this copy holds, so none
crosses repositories.

**One run cuts every page.** git cannot seek inside a file or a listing, so
the fiftieth page of a file is a read of the forty-nine before it, and the
recursive listing of a 100 000-file tree is some 250 pages, each a whole
`ls-tree`. So the git that misses cuts every page from the one asked to the
answer's end — a listing's, a file's or its line range's, a stat's, a
patch's, a blame's range — and the memo keeps each: reading a file page by
page streams it once, listing that tree page by page runs `ls-tree` once, and
blaming a range page by page blames it once. A comparison is kept whole; a
history's page is its walk's own, kept where the walk ended it and never where
the read's ten seconds of git did; whether a commit landed in a branch never
is, since its answer moves with the branch. A read that misses a page another read on the node is
cutting waits for it and is answered from what that read kept, so the seats a
push's review reaches, all asking for the same diff at once, cost the node one
git process rather than one each.

**Measured.** Over a history of 14 080 commits under a tree of 10 000 files,
on two cores of a laptop, a page from the memo is answered in 21 to 90 µs,
where cutting it with no memo took 4.6 to 262 ms — 133 to 3 460 times faster,
the stat of an edit across all 10 000 files the most. A read the memo misses
costs what one with no memo does, but for a long file's first page: it cuts
all 55 pages of a 3 MB file in 32 ms, against 9.5 ms for that one page, and
every page after it is a hit.

**At most a sixteenth of the budget.** The memo holds at most a sixteenth of
the cache's [budget](#the-cache-on-a-git-node), each answer counted at the
whole disk blocks it takes and the pages a read is still cutting counted with
those it keeps, and is trimmed to that before any repository is evicted — an
answer costs one git process to compute again, a repository a download of
every pack it holds. No repository is ever evicted for what the memo holds past
its sixteenth, and a page that would take the pages being cut past it is not
kept at all.

**Trimmed a sixteenth at a time, the least recently used first.** A copy's
memo is kept in **cohorts**, a directory each under `crewlet/memo`: every
answer is in the cohort that was the node's newest when it was last kept or
read, the node opens a new one each time a sixteenth of the memo's share has
been kept in the newest, and a read that finds an answer in an older cohort
moves it into the newest. So the oldest cohort is the sixteenth of the memo
used least recently, across every copy, and that is what a trim takes, whole —
one rename of its directory in each copy that holds it, however many answers
it holds — until the memo is back within its share: a trim gives back a
sixteenth, so the next is due once as much has been kept again rather than
after every read the memo missed. The node holds nothing of an answer in
memory — only each copy's cohorts and what each holds, 16 bytes a cohort, of
which a copy has some seventeen at the most — so a node's memory does not
grow with its memo, however large its disk; and it keeps no more than 32
cohorts, so a read the memo misses looks in at most that many places before
it runs git. After a restart, what the previous process kept is older than
anything kept or read since, so it goes first unless it is read again; the
node lists each copy's cohorts once, behind the serving, and counts any a
crash left unrecorded. Every read checks the
answer against its own header — the key it answers and the SHA-256 of what it
holds — so one a crash cut short or a disk damaged is computed again rather
than served, and nothing behind the serving hashes the memo. A rebuild keeps
it, and so does a move into the copy's next directory when packs leave it,
since an answer is about objects rather than packs — the whole memo moves into
the new copy in one rename as that copy takes over, so either costs the same
at ten answers or a million — and a request still reading the copy it
replaced simply misses from then on; a log restored or
recreated under the node drops it whole, since it may answer for objects the
history that replaced it does not hold, and nothing a read cut before that
history's packs were swept is kept after. `crewlet.code.cache.memo.bytes` is
what it holds and `crewlet.code.cache.memo.lookups` its lookups by outcome
([Metrics](../reference/metrics.md)).

### The home git node

Every node asks a read of the same git node first: the repository's **home**,
the first of the repository's rendezvous order over every live node with the
`git` role, keyed by the repository's id — an order each node computes alike
from the same set — and then its runner-up
([ADR-0059](https://github.com/crewlet/crewlet/blob/main/adr/0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md)).
So one repository's reads meet on one git node's cache whichever node they are
asked from, and a repository its readers keep warm costs the fleet one copy
for them rather than one on every git node a read went through. A git node
asks its own copy in process at its place in that order, never ahead of it.
The home is a preference, never an owner: nothing is leased, and two nodes
that each take themselves for the home answer the same read alike, since an
object id names the same object on both.

A home that cannot answer in time — its copy still syncing, its share full —
says so, and the read goes on to the runner-up at once; the next read asks the
home first again, since a node warming a copy is the right node a moment
later. A git node whose code host is not built — a company that does not host
code, or one whose first apply has not reached it — answers that it runs none
and is asked last for thirty seconds, and a node that went silent is asked
last of all. The reads travel on a lane of their own on each git node, so a
read waiting on its copy's sync never holds a slot the node answers its other
estate requests through, and each is a signed estate request like any other.
[Engine-built commits](#which-git-node-builds-a-write) are asked of the same
home, on a lane of their own beside the reads'.

**One node fills a copy.** A git node that holds no copy of the repository
starts a download of one only for the read that names it the one node of its
walk that may fill — the home, the first node of the order once those
demotions are applied — and answers every other `not_held` at once, having
started nothing, because a download once begun runs on after the read that
started it stops waiting. So a read of a repository no git node holds costs
the fleet one download, by its home, never one by every git node the read was
asked of. When a home dies, its runner-up becomes the first node every walk
asks and fills the one copy; a read in the seconds that takes is answered
unavailable, as it would be by a cold home, and the copy serves every read
after.

**The fill moves on past a home that will serve no copy.** A home that says
no copy for the read is coming there hands the fill to the next node of the
same walk, which starts a download of one and serves the repository's reads
from it: a home behind the company's configuration, for a seat's read
(`behind_config`) — however long a stuck node stays stuck — a home whose own
sync failed for a reason of its own (`node_failed`), and a home with no code
host. Kept on the home, a repository it alone held stayed unreadable to every
seat for as long as the home stayed stuck, since the nodes after it hold none
and fill nothing for a read not theirs. The home is not demoted for it: a
person's read, which judges no chart, and the next walk still ask it first. A
home that is bringing its copy up keeps the fill — still downloading or
repairing it, its chart moments away at boot, or failing on the object store,
the code log or damage a whole rebuild did not end, which another node's
download would only meet again — so one walk never starts two downloads where
one is coming.

**A busy home keeps its repository's fill too.** A home at its share holds its
copy and is only busy, and the share is the node's, spent by every repository
it serves: handing the fill on for it would answer a fleet busy everywhere with
a download of every repository read on each busy node, each evicting a warm
copy to make the room, and add no capacity anywhere. So one repository's seat
reads are served by the git nodes that hold a copy of it — its home, and any
node a person's fetch, a push or an earlier fill has warmed — and a repository
read harder than half of those nodes' read lanes is answered unavailable for
the rest: give its git nodes more cores and a wider `git.max_processes`, which
widens the share with the lane, or let a second git node hold a copy, as a
person's fetch through it does.

## Who read it

Every read of a `visibility: unit` repository's **contents** is recorded by
the node that served it, and an admin asks who read one over a range of
company days: `GET /code/repos/{name}/readers`
([the readers route](../reference/api-endpoints.md#get-codereposnamereaders)), or the
operator tool `code_repo_readers` from their own assistant — one answer, in
the same pages. A `visibility: company` repository's readers are every seat of
the company by declaration, so nothing records them, and its answer says so and
how to have them recorded: declare it `visibility: unit`, and reads are
recorded from that activation on.

| A read of the repository | Recorded | As |
|---|---|---|
| A clone or a fetch whose answer carried a pack | yes, `via: git` | one read, and the bytes of the answer the reader was sent |
| A fetch's negotiation round that ended with no pack, an `ls-refs`, the advertisement | no | — |
| A fetch the node ended before its answer did — a deadline, a stop | no | — |
| A server-side read that answered — a tree, a file's lines, a diff, a comparison, `landed_in`, a path's [history](#a-paths-history) and a file's blame, a [merge preview](#a-merge-preview), and a change's [revision reads](#a-revisions-facts): its facts, where its anchored lines are, an interdiff — a page from the [memo](#the-memo) included | yes, `via: tool` | one read, and the bytes of the answer |
| Its row — a listing, its detail, its push history, what a ref points at | no | — |
| A [check's box](#check-boxes)'s fetch, or its read of its commit through `/box/code/` | no | — a check's box runs the repository's own checks file and acts as nobody; its attempt's row, naming the commit and who queued it, is its record |

So the count is of reads that sent the repository's contents, which is what
an audit asks: a stateless fetch that negotiates over several rounds is one
read, counted by the round whose answer carried the pack, and that a
repository exists and what its branches point at are no secret
([Visibility](#visibility)).

**A reader** is a credential and the human seat it acted as — a person's
machine token (`pat:<credential id>`), a signed-in session
(`session:<lineage>`), a Tier A token (`token:<id>`, with no seat unless the
identity directory binds it to one) — or a seat, by its handle, each with how
it read (`git` or `tool`). The answer sums each reader's reads, bytes and
newest read over every node and every day of the range:

```json
{
  "repo": "payments",
  "recorded": true,
  "from": "2026-08-25",
  "to": "2026-09-23",
  "readers": [
    {"principal_kind": "agent", "principal": "dev", "seat": "dev", "via": "tool",
     "reads": 41, "bytes": 380112, "last_at": "2026-09-23T14:02:11Z"},
    {"principal_kind": "operator", "principal": "pat:0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e",
     "seat": "jane-founder", "via": "git", "reads": 3, "bytes": 18874368,
     "last_at": "2026-09-22T18:04:00Z"}
  ],
  "readers_elided": 0
}
```

The range is `from` and `to`, company days on the company's clock, both
inclusive — the thirty days ending today where it names neither — and at most
181 days long, which is what the records keep. A page holds at most 32 KiB of
readers, in the order of their kind, their principal, how they read and the
seat; the `next` it carries, handed back as `cursor`, is the page after it,
and the last page carries none.

**How it is kept.** The serving node files each read as it answers it, and
every fifteen seconds publishes **one `code_fetched` event per reader** of
each repository — one per credential or seat, way and, for a server-side read,
kind — counting its reads and the bytes they were sent, into its own event log,
kept out of the activity feed: a CI token fetching on every build would bury
it there. So a CI token fetching every few seconds is one record a flush, not
one a fetch, and no read waits for the event stream to take its record: a
clone completes, and a code tool answers, at the speed of its own work while
the stream elects a leader or is short of its quorum. A record the stream
refuses is published again by the next flush, and the node keeps up to 4 096
of them through an outage, the oldest dropped past that and logged
(`code_fetched_dropped`, with the reads it held). The node's usage publisher
derives each day of them, every fifteen seconds, into one record per
repository naming every reader that day: the `repo_read` kind of the usage
log, kept [181 days](../guides/retention.md) like the rest of it
([ADR-0020](https://github.com/crewlet/crewlet/blob/main/adr/0020-a-nodes-own-day-is-a-compacted-domain.md)).
A read is in the answer within about thirty seconds — the node's flush, then
the publisher's tick. A node stopped in an orderly way publishes its last
reads before it goes; one that dies loses the reads since its last flush, and
any records the stream was refusing.
A question is answered from those records, every node's at once, by whichever
data node serves the usage log, so the readers of a repository include those
of a node that has since left the fleet. One record names at most **1 024
readers** — the most-read of that node's day — and counts the rest: the first
page's `readers_elided` is how many readers that cap left out over the range,
and zero says the answer is complete.

**Who may ask** is an admin, and a person: the answer names the people, the
credentials and the seats that read a unit's code and how much — an audit of
those people as much as of the code. A unit's lead who is not an admin is not
given it, though they read the repository itself, and no seat ever is: a seat
asking who read a repository would be a model watching the people it works
for ([Identity and access](identity-and-access.md)).

## Which push landed a commit

Beside each copy, a git node keeps an index of which push introduced each
commit and every update each push landed on a ref, built from the code
host's push rows rather than from the packs. It names, on every server-side
read, the push that brought in each commit the answer names — who made it,
how it was built and, for a seat's push, the turn and the work item behind
it, as its row says — and it answers whether a commit is in a branch's
history and which push landed it there (`landed_in`) with ancestry checks
against the tip the asker was shown —
never a walk of the log or of the history. (This is a commit's provenance,
not [ADR-0040](https://github.com/crewlet/crewlet/blob/main/adr/0040-content-a-model-reads-carries-its-provenance.md)'s,
which fences text a model reads that somebody else wrote.)

**Eager, so a push just landed is named at once.** Every sync that applies
pushes folds them into the index before it answers anybody, so a reviewer
asking about a change the moment it lands is answered with its push. A full
read of a copy — a cold one, the first after a restart, a rebuild — reads no
pushes, since its answer can span the whole history; what lies between where
the index ended and where that read is the index's **gap**, which a
background backfill reads, a page at a time, on every node that holds a copy:
the pushes made while the node was down. Until it is read, an answer naming a
commit says which range the index does not hold (`coverage`), rather than
naming no push.

**The history is read where it is wanted.** A copy's index begins at its
first full read. The history before it, back to the start of the push history
the data nodes keep ([Retention and history](#retention-and-history)), is read
only by the node that answers a seat's read of the repository as its
[home](#the-home-git-node), and only once a seat has asked: once per fleet,
and only for repositories somebody reads. An index read back to that start is
`complete`; one that is not names its range on every answer. Once a day the
index also asks the rows for the oldest push they keep, and drops what it
holds below it, so it holds the push history the rows do and never grows past
it.

**Paced by the fleet, read where it is whole.** After each page the backfill
waits twice the page's own duration times the fleet's git nodes for each data
node, rounded up, the counts read again after every page: one git node a data
node holds at most a third of one reader of each data node, and the fleet's
backfills hold under half of one however many git nodes share them. A page is
read at a position the index has already seen, so the data node that answers
holds every push in it — a data node behind the log would hold the range in
part, and a commit pushed in the missing part would be answered with no push.
A page that ends inside one record of several pushes leaves that record to
the next page, whole. And a page of pushes, a sync's or the backfill's, is
held to what the data node reads for it as well as to what it answers — 4 MB
of each, every command a push's client sent counted, a refused one too — so
pushes that each landed one ref beside hundreds refused are read in more
pages, never in one too costly to answer.

**Never a wrong push.** `landed_in` names the first move of the branch after
which every move kept the commit reachable, so a commit a force-push removed
and a later push put back is named with the push that put it back. It never
names a move the commit was reachable before: where the commit already stood
on the branch before the oldest move the index holds of it, or before the
first move past its gap, the answer names no push and says which range the
landing lies in. A search runs at most 1 024 ancestry checks, and they share
the read's ten seconds of git with its check against the tip; whichever ends it
first, it names how far it reached and how many forced moves it did not search
past, rather than refusing the read. The count is the most checks that fit
those ten seconds at what one check measured, so a branch forced about five
hundred times since the commit landed on it is searched whole.

**Kept like the rest of the copy.** The index is a set of immutable runs under
`crewlet/` in the repository's directory, merged in the background eight
neighbours at a time — the cheapest eight none of which holds more than twice
the others together — so a lookup reads at most 32 of them, a push synced on
its own costs the node about a seventh of a merge rather than nearly one, and
pushes in bursts of every size keep an index well short of 32; they count
against the budget, leave with the repository, are linked by a rebuild, and
are dropped when the log under the node is restored or recreated, whose
positions then name other records. A run the node has not written since it
started is held to its own SHA-256 checksum before anything reads it — until
then an answer carries no pushes — and every run is checked again by the
scrub; one that fails drops the index, which the backfill reads again. A read
searches a run through memory the node maps from its file, and the kernel
counts every mapping of the engine's process against one setting of the
whole host, `vm.max_map_count` (65 530 unless the host raised it), past which
the process can map nothing — not even memory for itself, which stops it. So
a node keeps at most a quarter of that count of runs mapped (16 382 at the
default, read at boot and named as `mapped_runs` on `code_cache_opened`): a
run its last reader let go stays mapped for the copy's next reader only while
there is room, the one let go longest ago unmapped first, and a read that finds
every mapping held by reads in flight is answered without its pushes.
`crewlet.code.cache.provenance.backfilled` counts the pushes the backfill
read, and `crewlet.code.cache.provenance.behind` the repositories whose index
is still behind on the node ([Metrics](../reference/metrics.md)).

**What it costs.** Measured on four cores of a Core Ultra 7 265U with git
2.56.0: a sync's fold of one push is 0.84 ms — its run written and synced to
disk — a fifth of the cheapest delta sync there is (one ref, its pack from a
store in the process, 4.2 ms) and 1.8% of one whose pack comes from a store
answering in 30 ms (68 ms). The index holds about 64 bytes for each commit a
push introduced — about 190 bytes for a person's push of three commits, and
for a seat's push the bytes of its turn id, work item and model more, about
80, and for a push its coding box made those of its run and coding agent,
about 50 more — inside the cache's budget. A lookup that misses, every run
searched, is 15.5 µs at 32 runs over four million commits, under 1% of a
warm tree listing
(3.1 ms). The backfill reads the data node's push rows in pages of about 7 500
pushes — the 4 MB a page answers at most — each about 115 ms of the data
node's read and 150 ms in all, so a history of 18 million pushes, a large
company's monorepo over 181 days, is read back in about 18 minutes with one
git node for each data node ([Scaling Out](scaling.md#git-nodes) for a larger
fleet). Choosing the backfill's next step costs microseconds however many
copies a node holds — under 3 µs at 10 000 — since the node keeps which
copies have work due as their work changes, rather than looking at every copy
for it. On two E-cores of the same machine, a `landed_in` over a branch of
2 000 moves through 14 000 commits took 32 ms and 11 checks where the branch
was never forced, 56 ms and 17 checks with three forced moves since the
landing, and 4.8 s for all 1 024 checks with 514, answered with how far it
reached. Each check is a git process walking by generation once the node has
written the copy's commit-graph, 6.7 ms at the 99th percentile, so the 1 025
checks of a search at its bound fit the read's ten seconds of git where 2 049
would not; walking by date, without the graph, one check over the same history
took 26 ms.

---

## A revision's facts

Every head a [change](#changes) has had is one of its **revisions**, and what
a revision *is* against the branch it would merge into is three reads a git
node answers from its cache, as it answers every
[server-side read](#server-side-reads): asked of the repository's
[home](#the-home-git-node) first, held to the asker's grant — and, inside a
turn somebody woke, to theirs too
([Reads made for somebody else](#reads-made-for-somebody-else)) — recorded
where the repository is `visibility: unit` ([Who read it](#who-read-it)), one
page each, and named by the whole commit ids the change's rows record — never
an abbreviation, never a ref.

The **facts** of a revision are those of its head commit against its target's
tip, a function of the two commits alone, so every git node answers them
alike:

| Fact | What it is |
|---|---|
| `merge_base` | Where the head's history meets the target's: the **base** the change is measured from, as a merge of it would be. A history that shares no commit with its target's has none, and its change is measured from the empty tree — every file of its head. |
| `head_in_target` | The target holds the head already: the change is merged, by whatever moved the target there, and has nothing left to measure — no path, no tree and no id. |
| `base_tree`, `head_tree` | The trees of the base — the empty tree, for a history that shares none with its target's — and of the head: two revisions naming the same pair hold the same code, byte for byte, which an approval carries across ([Change classes](#change-classes)). |
| `touched` | Every file the change touches: each path the diff from the base to the head changes, in git's order, a moved file as its two paths. Past 1 000 paths, past 48 KiB of the list as JSON, or at a path that is not UTF-8 (which JSON cannot carry exactly), the facts say `touches_all` and give no list — read as every path by everything that reads one, where a list cut at its bound would leave the files past the cut out. |
| `classes` | What each touched Go file's change is — `whitespace_only` or `comment_only` — named by its place in `touched`, and whether every touched path was judged (`complete`): [Change classes](#change-classes). |
| `patch_id` | `git patch-id --verbatim` of the change's cumulative diff from its base, three lines of context around every change: the same for two revisions whose changes are the same lines among the same neighbours, wherever the base put them — a rebase onto a target that moved nothing near them keeps it — and another for any changed byte. Empty for a revision that changes nothing. |
| `patch_id_omitted` | No id was taken: the diff is past 256 MiB, or it and its id ran out what was left of the read's ten seconds of git. An omitted id, like an empty one, equals nothing. |

**Whitespace is a change.** The id is `--verbatim`, never `--stable`, which
strips every space and tab before it hashes: Python's and YAML's indentation,
a Makefile's tab and a string literal's spaces mean something, so a change to
them alone is not the change somebody looked at. And the diff names a binary
file by its blobs' whole ids (`--full-index`), since the id hashes them as the
diff's `index` line writes them, which git otherwise abbreviates to however
many digits the repository at hand needs — two copies of one repository
holding different counts of objects would identify one change apart.

**The base, the paths and the trees are what the read cannot do without;
the id and the classes it can.** The six steps run one after another in the
node's read lane — the merge base, the walk of the touched paths (git's raw
records, whose modes and blob ids say which paths a lexer classes), one
lookup of the two trees and of each such path's two blobs' sizes, the diff
written into a spool (a file with no name on the cache's volume, gone when
the read ends), `git patch-id` reading it back, and one `git cat-file
--batch` writing the Go files' versions for the lexer — never two at once,
which would hold two of the lane's slots for one read. A merge base, a walk
or a lookup that runs out the read's budget of git makes the read too large
to answer, as any read's git does. A diff past 256 MiB, or a diff or an id
that ran out what the budget left, is answered with the id omitted instead:
the safe side, since an omitted id equals nothing, and the base, the paths
and the trees that everything else reads — an interdiff among them — are
there. Classes cut short by their bound or by the budget are answered
incomplete, the same side: a class only relaxes.

**Measured**, over changes rewriting every fourth line of files of 256 KiB,
three reads each on two cores: a revision's facts took 18 ms for a 1 MiB
patch, 170 ms for 16 MiB, 621 ms for 64 MiB and 2.3 s for 249 MiB — some
114 MB of patch a second, so a diff at the bound is identified in under a
quarter of the read's ten seconds, and a node four times as loaded still
takes its id.

### Recorded on the change

A revision's facts are read ONCE, by the repository's [home](#the-home-git-node),
and recorded on the change, where every later reader finds them: an approval
carried from one revision to the next by its code or by an equal `patch_id`,
an interdiff kept to the files two revisions touch, the classes a merge
gate's exemptions read, the change's readiness. The record is
a **revise**, a write of the change's [conversation](#reviews-and-findings):
the home asks it of the repository's committing data node, whose change writer
decides it in one snapshot and publishes it on the change's own subject, as it
does a review — a git node never publishes one — and it names no asker, being
the engine's own.

| Table | A row for | What it holds |
|---|---|---|
| `code_revision_facts` | each revision of a change its home read | The target branch, the tip read and the position of the record that moved the branch there; the base and the trees of the base and the head; the `patch_id` or that none was taken; the files touched or that it touches all, and their [classes](#change-classes); the git node that recorded them; and its path-owner match — the digest of the owners matched against, those matched and those asked no review of, with why |

- **Facts are of the change's head revision against its target now.** A
  revise is decided only while they still are: facts of a revision a push has
  moved past, of a head the change no longer has, against a branch the change
  no longer merges into, or no later than the facts held of that revision
  against that branch — the same tip read twice, by a home that ran again or
  two nodes each taking itself for the home — are refused `superseded`,
  recorded nowhere, for the home's next pass. Facts of a later tip of the
  same branch replace the row, and so do the facts of a retargeted change's
  new branch: one row a revision.
- **A head its target holds has no facts.** Whatever moved the target there
  merged the change, so there is nothing of it left to measure or review; the
  home records the merge itself, and a revise naming its head as its own base
  is refused before anything is read.
- **Path owners are asked from the touched paths.** Each
  [path owner](#path-owners) one of whose globs matches a touched path — every
  owner, where the revision touches all — is asked to review the change: a
  unit of its effective lead, a seat of itself, the ask's reason `path_owner`
  and naming no asker. The HOME matches the globs, up to 64 owners' 32 globs
  each against up to 1 000 paths, which no decide pays on every round it may
  take, and sends the digest of the owners it matched against; the decide
  computes the digest of the owners its own snapshot holds, and facts matched
  against owners changed since are refused `superseded`, for the home to
  match again.
- **An owner who cannot review is skipped, and the facts say why**: a seat
  whose grant is below `review` (`below_review`), an author of the change
  (`author`), a unit nobody leads (`no_lead`), an owner the chart holds no seat
  of (`no_seat`), and an owner past the 32 reviewers a change asks at once
  (`too_many_reviewers`). An owner whose ask the change holds already —
  standing, or withdrawn by somebody's word on the change — is left as it is.
  A path owner grants nothing: whether its review counts is its grant's, as
  any reviewer's.
- **The facts say whom else they concern.** A revision's first facts name
  the reviewers whose standing verdict asks for changes on an earlier
  revision, to look again; and a change's first facts, where nobody who may
  review it is asked, name the lead of the repository's owner — its unit's
  effective lead, or the lead of the unit an owning seat sits in — so a change
  nobody is asked to review still reaches somebody. Each is stamped on the
  record by the writer from its snapshot and its chart, so every data node
  applies it alike, judging nothing again.

A write of facts that is refused appends nothing: `superseded` is the home's
alone, answered to no person and no seat, and a closed or merged change takes
no facts (`change_closed`, `change_merged`). `crewlet.code.conversation.writes`
counts them under `op` `revise`, beside the conversation's other writes.

### Change classes

A change's **class** says what a review of it need not read: a class only
ever *relaxes* — a [merge gate](#merge-gates) may exempt a change of one from
an approval it would otherwise wait for — so a class is given only where it
is true of the program and of every tool that reads the file, and whatever
cannot be shown so gets none. A revision's facts carry two kinds.

**Path classes**, one a touched file, as the language's own lexer judges the
file's two versions — Go's, `go/scanner`, the one lexer there is so far:

| Class | A touched file's change that is |
|---|---|
| `whitespace_only` | Its tokens and its comments the same: only the space between tokens moved — never a newline that ends a statement, which in Go is a token of its own, and never a raw string's spaces, which are its value. |
| `comment_only` | Its tokens the same once its comments are set aside, no comment a tool reads as an instruction changed, and nothing around one. Every `whitespace_only` change is a `comment_only` one too, and is listed as the narrower. |

- **A directive is code, and so is the layout around one.** A comment the
  compiler, cgo, `go generate`, `go test`, the build or a linter reads —
  `//go:` lines, `//line`, `// +build`, `//export`, a cgo preamble, an
  example's `Output:`, `//nolint` and `//lint:ignore` among them — changes
  what the file builds, runs or reports, and whitespace alone changes what
  one does (a `//go:generate` indented off its line's start is ignored), so
  no class holds where one, what follows it or where it sits changed.
- **Only a Go file changed in place is classed**: a path ending in `.go`,
  a regular file on both sides in the same mode. A file added or deleted has
  no second version, a mode change changes what no lexer reads — whether
  the file runs — a symbolic link's blob is a path and a submodule no blob
  at all, so none of them has a class, and neither has a file of another
  language yet.
- **Bounded, and incomplete past the bound.** A version past 1 MiB is left
  unread, with no class — past every hand-written file of Go's own tree. A
  revision lexes at most 16 MiB of Go in all, both versions of every file
  counted, in the touched list's order: the file that would take it past
  that is not read, nor any after it, and the revision's classes are
  **incomplete** — as they are where its lexing ran out what the read's ten
  seconds of git left, and always where the revision touches all, which
  lists no path to class. Incomplete classes keep the classes of the files
  lexed, and say nothing of the rest; complete ones say a file no list names
  has no class. So an exemption that holds only where every path a change
  touches is of a class requires them complete.

The facts record the classes as `classes`: `whitespace_only` and
`comment_only`, each the places in `touched` — counted from 0 — of the files
of that class, and `complete`. Places rather than paths, because the facts
are one page beside a touched list of up to 48 KiB, and a thousand files
reformatted, named a second time, would not fit it.

**`no_code_change` is a revision's class, never a path's**: a revision whose
base and head trees are those of a revision an approval stands on holds the
code that was approved, byte for byte — a message amended, commits squashed
or reordered, a branch pushed again — and the approval carries to it
([Merge readiness](#merge-readiness)), however its history differs and
whatever its `patch_id` says, an omitted one included: a change past
256 MiB of diff, whose id is never taken, keeps its approval across a
revision that changed nothing. The facts record the two trees
(`base_tree`, `head_tree`) rather than the class, because the class is a
relation between two revisions, which the reader of the two rows judges in
its own snapshot, as it compares two patch ids. It is never compared with
the revision's own base — a diff that is empty against it changes nothing,
and holds other code than an approver read — and it is refused in a gate's
`approvals_unless` and `unless`, where an approval already carries across
it.

**Measured** (`BenchmarkClassifyGo`, two cores): two versions of a 1 MiB
file take 34 ms to lex as ordinary code and up to 116 ms as nothing but
comments under a directive, so a revision's 16 MiB of Go is lexed in under
a second at the most — a tenth of the read's ten seconds of git.

### The home's change work

A repository's [home](#the-home-git-node) — the first git node of its order,
which its git reads are asked of first — owes its live changes their facts
and their [mergeability](#mergeability): every git node runs a **home loop**
for the repositories it is home to, and the rows are its work list. A change
is **due** while its head revision holds no facts, or no mergeability, against
the branch it merges into: a push that moves its head, and a retarget, make it
due again, and each record takes its half off. A change whose readiness its
mergeability stores was judged before what the readiness reads moved is due
a refresh ([Merge readiness](#merge-readiness)). A change whose target has moved
past the tip it was merged into is due again too, at the home's own pace
([A moved target asks again](#mergeability)). A change of an archived or a
deleted repository is nobody's work, and neither is one whose target branch
is gone, until the branch is pushed again or the change retargeted.

For each due change the home reads the revision's facts against its target's
tip as the rows hold it, then either

- **lands it**, where the target holds its head already — a push moved the
  target past it — as a record on the repository's subject the committer
  decides against the change's head and the tip the home read: a head pushed
  since, a retarget or the target moved again are each refused `stale`,
  recorded nowhere, for the home's next read; or
- **records its facts**, its touched paths matched against the repository's
  path owners on the home ([Recorded on the change](#recorded-on-the-change));

and where it owes its merge, **merges it** into its target's tip — or lands
it, where the target came to hold its head — and records what the merge did
([Mergeability](#mergeability)); and where it owes no merge but what its
[readiness](#merge-readiness) reads moved since its merge judged it — a
review, an ask for review, an answer to its findings, the facts just
recorded, an edit of the change — **refreshes its readiness**, merging
nothing.

Two things put a repository's work in front of its home:

- **A nudge.** Every git node watches the code log's repository subjects and
  its repositories' checks subjects — each record's subject and position,
  never its body — and a record on a repository it is home to, or on that
  repository's checks, schedules that repository's work at once, read at the
  record's position so the node's copy holds the record before the rows are
  read.
- **The backstop.** Once a minute, and at once when it starts, every git node
  lists the repositories holding a due change and works the ones it is home
  to, judged against one reading of who serves git. A nudge only shortens the
  wait: one lost — a member restarting, the watch's consumer gone, who serves
  git unreadable for a moment — leaves the work for the next pass, a minute at
  the most, and a repository whose home moved is found by its new home's next
  pass.

There is no lease: two nodes that each take themselves for a repository's
home both read, and the records' own guards take one. A node works two
repositories at once, one turn of a minute at the most each, a repository
with more due work than a turn finishing it at the passes that follow.
`crewlet.code.home.passes` counts the turns by what became of them,
`crewlet.code.home.nudges` the code log's word to the loop,
`crewlet.code.home.facts.duration` each facts read and
`crewlet.code.home.merge.duration` each merge (the [metrics
reference](../reference/metrics.md)).

### Mergeability

Whether a change's head revision merges cleanly into its target's tip is a
function of the two commits alone, and its [home](#the-home-git-node) records
it on the change beside the revision's facts: its **mergeability**. The home
merges the two commits with `git merge-tree --write-tree` — the merge `git
merge` would make, from every merge base git finds — in a SCRATCH repository
of its own beside its copy of the repository, never in the copy, whose
objects are its rows' alone: the merged tree, and every blob a conflict
leaves marked, are written into the scratch and removed with it once the
merge has answered. The record is a write of the change's
[conversation](#reviews-and-findings), as a revision's facts are: the home
asks it of the repository's committing data node, whose change writer
decides it in one snapshot and publishes it on the change's own subject, and
it names no asker.

| State | What the merge did |
|---|---|
| `clean` | It wrote a tree and left no path conflicted. |
| `conflict` | It left paths conflicted. The record names each once, in git's order — the first 100, and no more than 16 KiB of the list as JSON — and counts the rest; a path that is not UTF-8, which a record cannot carry exactly, is counted and never listed. |
| `unknown` | The merge ran out of its 30 seconds and decided nothing. It is tried again at the home's refresh, as a moved target is. |

- **Ancestry first.** Before it merges, the home asks whether the target's
  tip holds the head already: a target moved past the change — by a merge
  commit, or any push that took the head in — merged it, and the home lands
  the change ([The home's change work](#the-homes-change-work)) rather than
  merge it again.
- **A history of its own** merges from the empty tree, as its facts measure
  it: a head whose history shares no commit with its target's conflicts in
  every path both sides add differently, and merges every other.
- **One row a change, and monotone.** A mergeability is of the change's head
  revision against its current target, or it is refused `superseded`,
  recorded nowhere: a merge of a revision a push has moved past, of a head the
  change no longer has, or into a branch it no longer targets. For that
  revision against that branch it moves only forward: a merge into an earlier
  tip than the row holds — a home that read before the target moved, slower
  than the one that read after — is `superseded`, and so is the same tip
  merged again once the row is `clean` or `conflict`, since a merge of two
  commits is decided once. Only an `unknown` row's tip is merged again, the
  row's instant moving with it. A closed or merged change takes no merge
  (`change_closed`, `change_merged`).
- **A moved target asks again, at the home's pace.** A NEW head revision — a
  push to the change's branch, a retarget — is merged at once, whatever came
  before it. A target that moves past the tip a change was merged into asks
  that merge again only once the mergeability is ten minutes old, and a home
  merges no more than 64 of one repository's such changes a minute, the
  oldest merge first: a target that moves on every push costs its home 64
  merges a minute at the most, never a merge a change a push, and no change
  waits behind the same first ones every refresh. On a change's page the
  mergeability says which tip it is of — "as of" that tip — until then. An
  `unknown` merge is tried again on the same pace.

| Table | A row for | What it holds |
|---|---|---|
| `code_mergeability` | each change its home merged | The revision merged, the target branch, the tip merged into and the position of the record that moved the branch there; the state; the conflicted paths it names and how many more there were; the git node that merged; the record that merged, with its broker instant, which the change's next recheck is paced by; and the change's [readiness](#merge-readiness) as the last record judged it — whether it was ready, whether it waited on a person — beside the versions of the change's row and of what its readiness reads it was judged at |

The change's own read carries it (`mergeability` on a change's detail): of
its head revision into its target where its home has merged that, and of an
earlier revision or another branch while the home's next merge is owed. Its
conflicted paths are weighed in the change's conversation's text, as its
revisions' facts are, so a [purge](#deletion-and-purge) weighs them by that
one narrow row. `crewlet.code.conversation.writes` counts the records under
`op` `mergeability`, and `crewlet.code.home.merge.duration` each merge by
`work` and `outcome`.

#### A merge preview

What merging one commit into another would do — a comparison's preview,
before any change is opened — is asked of a repository's git nodes as a
[server-side read](#server-side-reads) is: of its home first, held to the
asker's grant and, inside a turn somebody woke, to theirs too, recorded where
the repository is `visibility: unit` ([Who read it](#who-read-it)), and its
two commits named by whole ids the asker resolved, never a ref. It is
answered by THE SAME MERGE a change's mergeability is recorded by, so a
preview and the change it would open say one thing — its state, the
conflicted paths a record would hold and how many more there were — merged in
a scratch of its own, in the node's read lane under one unit of its seat
git-work share ([Fair share](#fair-share)), within a read's ten seconds of git:
a merge that runs out of them answers `unknown`. No route asks for one yet;
a comparison's question arrives with the code host's questions.

### Merge readiness

Whether a live change may merge is answered as a list of **preconditions**,
each judged from the rows of ONE snapshot and the chart of ONE epoch — never
a workflow somebody has to remember to run, and never a decision the tracker
records: what a merge counts is the change's own reviews and findings.

| Precondition | Met when | Waits on, while unmet |
|---|---|---|
| `draft` | The change is open for review, not a draft. | Its owner, who marks it ready — as a principal that maintains the repository or a person may. |
| `facts` | Its head revision's [facts](#recorded-on-the-change) against its current target are recorded. | Nobody: it is `computing` until its home reads them. |
| `approval` | ONE COUNTED APPROVAL stands: an eligible reviewer's standing verdict approves the head revision, or an earlier revision whose code or diff the head carries unchanged. A precondition only where the target's [merge gate](#the-merge-gate) asks one. | The reviewers asked who may approve it; its owner, to ask one, where nobody is. |
| `blocker` | No blocker stands. | Its owner while a blocker is open; each blocker an author answered, its raiser, who judges the answer. |
| `mergeable` | Its head revision merges cleanly into its target, as its home recorded it — "as of" the tip it merged into, whatever the target did since. | Its owner, on a conflict. It is `computing` until its home merges the head revision, and while a merge decided nothing within its deadline. |

- **A counted approval** is the standing verdict — the latest review that
  approves or asks for changes — of a person, or of a seat the chart lets
  review the repository, that is **not an author** of the change: an author's
  approval never counts, and a reviewer who pushes the change's branch after
  approving it becomes one. A reviewer's later request for changes replaces
  their verdict, withdrawing their approval and holding nothing else: a merge
  counts approvals and standing blockers, not requests.
- **An approval carries to a later revision by its code or by its diff**, the
  detail naming which: by its CODE where the two revisions' recorded base and
  head trees are equal (`no_code_change`, [Change classes](#change-classes)) —
  the very code that was approved, whatever the two `patch_id`s say, an
  omitted one included; by its DIFF where their recorded `patch_id`s are
  equal, neither empty nor omitted — the same lines changed among the same
  neighbours, as a rebase that moved nothing near them keeps it ([A
  revision's facts](#a-revisions-facts)). Until the head revision's facts are
  recorded, an approval of an earlier one is `computing`; an approval of a
  revision whose own facts were never recorded — its head moved before its
  home read them — never carries, since a home reads the head revision
  alone.
- **Each standing blocker is named with who can clear it**: its raiser — by
  dismissing it, or by a review of a later revision that does not carry it —
  the seat the chart lets maintain the repository (its owning unit's
  effective lead, or its owning seat, where its tier lets it maintain), and
  any person, by dismissing it. A readiness names the 16 oldest and counts
  the rest; the change's findings are read a page at a time.
- **The first sentence** is the first unmet precondition's, in the order
  a change moves through — the merge gate's own placed among them ([The merge
  gate](#the-merge-gate)) — else the first that is `computing`, else that the
  change is ready. A change whose first unmet precondition waits on persons
  alone **waits on a person**, which `needs=person` lists ([Reading
  changes](#reading-changes)).

**Judged on every read, stored by its home.** A read of a change, and each
row of a listing, judges the readiness again from the read's own snapshot.
Each [mergeability](#mergeability) record judges it too — from its writer's
own snapshot, with the merge it records — and stores whether the change was
ready and whether it waited on a person, beside the versions of the change's
row and of what the readiness reads that it was judged at, and the position of
its head's checks. That stored answer is what a wake that a change became
ready reads, and what `needs=person` seeks. When either version moves past
it — a review, an ask for review, an answer to a finding, a revision's facts,
an edit, a retarget, a draft flag — or an attempt of its head's checks is
queued or ends past that position
([How a head's checks are dispatched](#how-a-heads-checks-are-dispatched)),
the change is due a **refresh** from its home: a mergeability record that
merges nothing, judging the readiness again over the merge the row holds,
which leaves the merge's own instant — what a moved target's recheck is paced
by — as it was. A review is a record on the change's own subject, which no
nudge names, so its refresh comes at the home's next pass, a minute later at
the most. A refresh whose readiness is judged at its inputs already, or of a
change whose head revision its home has not merged yet, is refused
`superseded`, recorded nowhere. The chart moves no version: a stored answer
is as of the chart that judged it, until the change's next record.

### The merge gate

A change is judged under the **merge gate** of the branch it targets ([Merge
gates](#merge-gates)) — every reader of its readiness and every decision
about its merge judges it by one function, from the rows of one snapshot and
the chart of one epoch. Every protection rule whose pattern matches the branch
holds it, as every matching rule holds a push to it, so the gate is the
rules' combination, the strictest of each term:

| Term | Combined over the rules matching the branch |
|---|---|
| `merge_by` | The strictest of the rules' — a rule with no gate reading as `writers` — and `code.merge_by_floor`. A branch no rule matches has no floor: whoever may push it merges into it. |
| `approvals` | `one` where any gate asks one; `approvals_unless` exempts a change only by a class every such gate names. |
| `human_paths`, `checks` | Every gate's, each entry as its gate wrote it. |
| `owners_approve` | On where any gate turns it on. |
| `landing` | `post_land` only where every gate requiring checks lands so. |

Its preconditions are [Merge readiness](#merge-readiness)'s and the gate's
own, each listed only where the gate holds the change to it, in the order a
change moves through — opened, measured, approved, cleared, checked, merged,
merged by somebody, and landed by its queue:

| Precondition | Listed where | Met when | Waits on, while unmet |
|---|---|---|---|
| `draft`, `facts` | Always. | As in [Merge readiness](#merge-readiness). | As there. |
| `approval` | The gate asks one: an ungated branch merges on none. | One counted approval stands — or every path the head revision touches is of a class `approvals_unless` names, its classification complete. | As in [Merge readiness](#merge-readiness). |
| `human_approval` | The gate names human paths. | No human path holds the head revision — none matches a path it touches, or every path one matched is of a class its `unless` waives — or a **person's** counted approval stands. | The persons asked to review it; its owner, to ask one, where none is. |
| `owners_approval` | The gate sets `owners_approve`. | The head revision touches no owned path, or ONE counted approval comes from somebody eligible for **every** [path owner](#path-owners) it touches: a unit owner's effective lead and every lead above it, a seat owner and its unit's lead chain — or any person. | The seats on every touched owner's chain that may review it, and the persons asked; its owner, to ask one, where none is. |
| `blocker` | Always. | As in [Merge readiness](#merge-readiness). | As there. |
| `check` | The gate requires checks and lands `pre_land`. | Every required check passed — or was skipped, its paths untouched — on the head commit checked for the change's own target, by its latest attempt, whatever ran it. An attempt checked for another branch ran that branch's checks file and counts for nothing. | Its owner, on a check that failed, errored past the home's requeue or was cancelled. |
| `mergeable` | Always. | As in [Merge readiness](#merge-readiness). | As there. |
| `merge_authority` | A merger is named: whoever reads, on a read; the principal, on a decision. | A person; on a protected branch, a principal holding what `merge_by` asks — `writers` write the repository, `maintainers` maintain it, `persons` are persons; on a branch no rule protects, a principal that may push it. | The seat that maintains the repository, where one may merge it. |
| `queue` | The branch's [merge queue](#the-merge-queue) holds the change. | Never: it is `computing`, naming the change's place, while the queue lands it. | Nobody. |

- **Every approval a term counts is counted by one rule**: an eligible
  reviewer's standing verdict, never an author's, on the head revision or
  carried to it unchanged by its code or its diff ([Merge
  readiness](#merge-readiness)). An approval of an earlier revision whose
  code moved since counts for none of the terms — and two approvals each
  eligible for one of two touched owners are not the one approval
  `owners_approve` waits for.
- **No path owner's glob is matched in a judgment.** The owners a revision
  touches are its facts' recorded match, made by the repository's home
  ([Recorded on the change](#recorded-on-the-change)) against the owner rows
  whose digest it records: up to 64 owners of 32 globs each against a
  thousand paths, which no decision pays on every round it takes. Where the
  owner rows a snapshot holds digest to another value — owners added or
  removed since — `owners_approval` is `computing` until the home matches the
  paths again.
  The gate's human paths, at most 32 a rule, are matched against the paths
  the facts list, and a glob this build cannot read holds the change.
- **A required check with no attempt is `computing`**: one the target's
  [checks file](#the-checks-file) defines waits for the repository's home to
  queue it, and one it does not for a person's CI to report it. So is one
  queued or running, and one that errored for its runner's reasons below its
  third attempt, which the home queues again. A `post_land` branch's checks
  hold its tip, never a change into it.
- **What a change's home stores is its readiness for whoever merges it**: a
  [mergeability](#mergeability) record judges the gate with no merger, so
  `merge_authority` is a read's alone, judged for whoever reads.

### The merge queue

A branch whose merge gate requires a check is a **queue target**: a change
merges into it through its **merge queue** — `pre_land`, the queue testing
each change merged on the branch before it lands, or `post_land`, a lane that
lands changes ahead of the checks the branch's newest tip then runs ([Merge
gates](#merge-gates)'s `landing`). A change joins it by an **enqueue**, which
names the change at the head and on the target its asker was shown, how it
merges (`merge`, `squash` or `fast_forward`), whether its landing deletes its
source branch and closes its work item, and — where the gate allows
`auto_revert` — the enqueuer's consent to a revert of a landing that breaks
the branch.

An enqueue is a record on the repository's own subject, the one its pushes and
its changes' lifecycles are on, decided by the repository's
[committer](#the-committer) alone — so a change's entry, its close and the
pushes that move its head have one writer, and a change is never both queued
and closed. **The enqueue is the gate**: it is decided from one snapshot by the
change's [merge gate](#the-merge-gate), its principal the merger — every
precondition met but `mergeable`, which the queue's candidate proves on the
branch it is built on, and on a `post_land` branch no check, whose checks hold
its tip. Its landing judges the gate again from its own snapshot. **The chart
judges every enqueue, whoever asks** — who may approve the change, the lead
chains above the owners it touches, a seat's merge authority — so a node that
holds no chart yet, or one [behind the company's
configuration](#a-seats-push), decides no enqueue, and no seat's dequeue, and
hands it to the repository's next committer.

| Refusal | Why |
|---|---|
| `repo_importing`, `repo_mirrored` | The repository is being imported from another host, or is a pull mirror, which nothing merges into: a dequeue of a pull mirror's entry still lands, so a repository that became a mirror sheds its queue. |
| `gate_unmet` | The change does not meet its target's gate: the answer names every precondition not met and whom each waits on. |
| `change_not_open`, `change_draft` | The change is closed or merged, or a draft, which merges nowhere. |
| `stale`, `head_moved` | The change targets another branch, or its head moved, since its asker read it: read it again. |
| `no_queue` | The branch's gate requires no check: a change merges into it directly. |
| `no_auto_revert` | The enqueue consents to a revert, and the branch's gate allows none. |
| `already_queued` | The change holds a live entry already, naming it. |
| `queue_full` | The branch's queue holds 1 024 waiting changes: a queue more than a day of its fastest capacity behind is broken, not busy. |

An entry's **place** is its order in its branch's queue. While a queue holds a
change, its readiness lists a `queue` precondition, `computing` and naming
the place — the queue lands it, and a change waiting in a queue is merging,
not ready to merge. `crewlet.code.queue.proposals` counts every enqueue and
dequeue a node's committer was asked to decide, by what became of each:
`gate_unmet` climbing is askers queueing changes before their gate is met,
and `queue_full` a queue landing slower than it is fed.

**An entry leaves its queue with its change.** Its enqueuer, the change's
owner, a principal that maintains the repository or a person may **dequeue**
it. And every record that leaves the change no longer what was enqueued ends
its live entry in its own apply: a close, a draft, a retarget and a landing of
the change dequeue it, each naming which, and a push that moves its head
**ejects** it, `head_moved` — the queue would otherwise test a head nobody
enqueued — or dequeues it where the push merged or closed the change.

**The queue's own records** are what it does with what was enqueued — each a
record on the repository's subject, built by the repository's [home git
node](#the-home-git-node) and decided by its committer from one snapshot,
naming no asker: the queue's work is the engine's, and a landing lands each
change as whoever enqueued it.

- A **candidate** records a `pre_land` queue's **train**: up to `depth`
  candidates of up to `batch` live entries each — 64 entries at most — every
  candidate its entries merged onto the one before, the first onto the
  branch's tip, its own tip what the branch's required checks then run on.
  Its commits are stored in ONE pack (`kind: candidate`), a live pack of the
  repository, and a candidate is rows and that pack — never a ref, advertised
  or not. A branch holds one train: a candidate abandons the one before it,
  whose candidates are removed and whose entries wait again.
- An **eject** puts live entries of one branch out — `check_failed` (naming
  the checks), `conflict` (naming up to 100 paths), `gate_unmet`,
  `head_moved` or `not_fast_forward` — and abandons the train they were in.
  The node whose committer landed it announces a `code_queue_ejected` event
  for each, fed to the activity feed. A dequeue of an entry a train tests
  abandons the train too.
- A **land** lands up to 64 entries in ONE record: a `pre_land` train's
  tested **prefix** — its candidates from the first, in order, up to one
  whose tip every required check passed on — or a `post_land` lane's chain,
  landed ahead of its checks with a pack of its own (`kind: land`). **Its
  decide proves every entry's gate again**, from its own snapshot, with the
  entry's ENQUEUER as the merger: the enqueue was the gate, and the landing
  is the record that moves the branch, so a review asking for changes, a
  blocker or a merge authority lost since the enqueue keeps the whole landing
  off the branch, its answer naming each entry it would not land for the home
  to eject. Its apply marks each change **merged first** (`merged_via: queue`
  or `lane`, merged by its enqueuer — the queue never acts on its own
  account), lands **one push row `via: queue` a change**, naming the enqueuer,
  so [which push landed a commit](#which-push-landed-a-commit) names each
  change's own landing, deletes each source where the entry asked it and a
  push deleting it as the enqueuer would be taken, and writes the branch's
  landing rows in chain order — green, and the branch's green row moved to
  the tip, where it lands `pre_land`.
- A **green** moves a protected branch's **green row** onto a landing record
  whose required checks passed on its tip — at the landing's own position, so
  the `landings` read lists what came after it — and a **broken** marks a
  landing above the green row broken, a required check having failed on it.
  The green row **only ever advances on green**: no record moves it on red,
  and none moves it back. It is the commit a mirror or a deploy follows.

| Refusal | Why |
|---|---|
| `repo_importing`, `repo_mirrored` | The repository is being imported from another host, which takes none of these records, or is a pull mirror, which takes no candidate and no landing: an eject, a green and a broken still land on a mirror, moving no ref, so a repository that became one sheds its queue. |
| `target_moved` | The branch is no longer at the tip the train or the landing was built onto: the home builds it again. |
| `not_queued` | An entry is not live in the branch's queue — ended since, or another branch's. |
| `stale` | What the record names is not what the rows hold: a commit its entry's method does not make, a pack it does not need or lacks, a landing that is no prefix of the branch's train, a policy the branch does not land by. |
| `gate_unmet`, `head_moved`, `change_not_open` | An entry of a landing no longer meets its gate with its enqueuer as the merger, or its change moved or ended: the answer names each such entry. |
| `not_green` | A `pre_land` landing's tip, or a green's landing, has a required check not passed by its latest attempt checked for the branch. |
| `not_red`, `no_landing` | A broken names a check that did not fail, or a green or a broken names no landing above the branch's green row. |
| `record_full` | A landing's rows — its own, its pushes' and the live changes they move — would pass a record's: land fewer at once. A green marking more than 2 000 landings names an earlier one first. |
| `upload_expired`, `repo_size`, `compaction_behind` | The train's or the chain's pack, held to a push's rules: a busy queue waits for compaction rather than outrunning it. |

**Every record that moves a protected branch leaves its landing row**: a
merge's (`kind: direct`), a queue's (`pre_land`, `post_land`) and a person's
own push under `direct_push: persons` (`kind: push`, naming their human seat)
— green, and the branch's green row moved with it, where the branch's gate
requires no check. The push's decide stamps its landings from the rules its
snapshot holds, so every node's apply writes the same rows without matching a
rule itself.

**A queue's history keeps pace with its records.** Ended entries and the
landings below each branch's green row are kept 30 days: every candidate,
eject and land removes its repository's oldest past that, up to 128 rows a
record — as many as a landing ends and lands. A repository whose queue
stopped is the [code-retention duty](#retention-and-history)'s: a pass lists every
repository holding such history past 30 days by a day's grace, and a
`queue_sweep` record on its subject removes it, back to 30 days.

Three reads answer a queue to a person or a seat, each held to the asker's
grant on the repository: `queue`, the live entries of one repository's queues
— or of one of its branches, or of every repository the asker reads — in
queue order with their places, beside the trains testing a named
repository's branches; `landings`, one protected branch's landings above its
green row; and `green`, each protected branch's newest commit whose required
checks passed. The repository's home reads a fourth, `queue_due` — the active
repositories holding a live entry, and one repository's entries — which no
person or seat may read.

### Tip checks and code_green

**Every protected tip is checked.** A `pre_land` landing proved its branch's
required checks on its candidate before it landed, and lands green; a merge or
a push into a branch whose gate requires no check is green as it lands. Every
other landing — a merge a person's override landed, a person's own push under
`direct_push: persons` — lies **above the branch's green row** until a check
of a tip proves it: two changes each green on its own head can land a merged
result nothing tested, and a person's push was tested by nobody at all. The
repository's [home git node](#the-home-git-node) owes each such branch its
**tip work**, read from the rows in one snapshot — the branch's green row, the
checks its gate requires, and its landings above the green, oldest first, each
with the latest attempt of each required check on its commit for the branch —
and worked by its `checks` worker:

1. **The newest landing is dispatched**, and only the newest: a required
   check it holds no attempt of is queued there (purpose `tip`). Landings that
   are no longer the newest are never dispatched, so a burst of pushes costs
   one round of checks. A fast-forward landing is the very commit its change's
   head checks measured, and its head's attempts count.
2. **Every required check passed there**: a `green` record moves the branch's
   green row onto it — and every landing below it green with it. A branch whose
   gate requires no check is green at its newest landing as it stands.
3. **One failed there**: the failed checks are **bisected over the landing
   rows** — never over git's history, whose merge commits lead into change
   branches the target never held. Each round probes the landing midway between
   the last one they passed on (the green, at first) and the first they failed
   on (purpose `bisect`, those checks alone), until the two are adjacent; a
   `broken` record then marks the first red landing broken, naming the checks,
   and the node that landed it announces one `code_target_broken` event. The
   green row stays where it was: it **only ever advances on green**. The next
   landing — a revert, or a fix forward — is dispatched as step 1, and its
   green tip moves the green row past the broken landing.

An attempt its infrastructure ended is queued again up to three attempts, as a
head's is; one still queued or running, or ended in a way only a person's rerun
moves (`cancelled`, errored past its requeues), waits.

**What a tip check runs is the base's file.** Every tip check and every probe
is defined by `.crewlet/checks.yaml` at the **base of the first landing above
the green** — the green itself, or before any tip passed, the commit the first
landing moved the branch from — never at the tip, which holds every landing's
edits: a landing that rewrote the file would otherwise define the check its own
tip is judged by ([ADR-0068](https://github.com/crewlet/crewlet/blob/main/adr/0068-a-check-is-a-measurement-of-a-commit-defined-by-the-target-branch-and-run-where-no-author-can-reach-it.md)).
Each attempt records the file's blob (`defs_oid`) and its diff base. A base
whose file does not run, or defines no check the gate requires, errors that
check on the tip (`errored`, `defs_invalid`) and keeps the green where it is
until the green moves — a queue's green landing, or the gate changed: fix the
file through the queue, or have a person change the gate. A required check
that ran and failed beside it is bisected all the same, so the landing that
broke it is still named.

**A bisect reads 256 landings above the green at most** (eight rounds). A
branch holding more is read as its oldest 256 and its newest: its newest is
still dispatched, and a culprit past those 256 is named once a green tip clears
the landings before it — or a person reverts to the last green; the home logs
`code_tip_culprit_past_the_window` meanwhile.

**The home finds the work from the rows.** A repository with a landing
neither green nor broken is listed by the home's `checks_due` read, over an
index holding exactly those landings, so its backstop pass finds a branch
whose nudge was lost within a minute, and never reads the thirty days of green
landings behind it. A branch whose culprit is named stays listed until a tip
passes; its home's pass reads its window and finds nothing owed.

### How a candidate is built

A queue never builds its changes one merge at a time. The repository's [home
git node](#the-home-git-node) builds a run of waiting entries as one
**chain**: in one scratch beside its copy, each entry is merged onto the
commit the entry before it made, the first onto the branch's tip, by the very
build a [merge](#how-a-merge-is-built) runs — its method, its one `git
merge-tree --write-tree` and its commit authored as a merge's is (U43,
answered (a)): a merge commit by **who enqueued it**, a squash by **the
change's owner** with a `Co-authored-by:` trailer for each other author. A
chain is at most **64 entries**, the most one record lands.

- **Each entry gets a merge's budget.** Every entry's merge is given the
  thirty seconds of git a merge is given, and its commit's author date is
  **its entry's own instant**, when it was enqueued, never the build's — its
  committer date that or its later parent's. So a chain built again from the
  same entries, on any git node, after any crash or as a bisect builds a
  prefix twice, is the same commits byte for byte.
- **A conflicting entry is skipped, and leaves nothing.** An entry that
  conflicts with what the chain made so far is skipped, naming its paths as a
  merge's conflict does, and the next entry is merged onto the same tip. So
  is a `fast_forward` the branch's history refuses, a head the chain already
  holds, and a merge that runs out its thirty seconds. Whatever the skipped
  merge wrote into the scratch is removed before the next entry, so the
  chain's pack holds exactly the commits it made.
- **One pack, stored once.** Every object the chain made is packed in one
  pack, checked by `git index-pack --strict` as every engine write's is,
  within the time a merge is given and at most 256 MiB, 4 MiB an entry. It
  is stored as one bundle. A chain past either bound, or one holding an
  object git's own check refuses, is refused whole with nothing stored.
- **Nothing enters the cache before its record applies.** The chain's
  commits are a candidate's: the record that names them and their pack is
  the queue's, proposed after the build. The pack is taken
  into the home's cache only once that record has applied; a record that does
  not apply leaves the cache as it was, and its stored bundle, which no row
  names, is the collector's to remove. No ref names a chain:
  `refs/crewlet/**` stays reserved and is never advertised, and a check's box
  fetches a candidate's commit by its id.

A chain holds one unit of the home's seat git-work share for its whole build,
the one unit each of its merges would have held alone, and is logged
`code_chain_built` with its entries, how many it skipped and its pack's size.

### Where lines are in another revision

A finding or a comment is written against lines of one blob. Placed in
another revision, up to 64 such ranges at a time, each is found by the diff
between the blob it was read in and the file at its path now — on the new
side of the change's diff, the revision's head; on the old side, its base —
read as git's zero-context hunks, never by searching for the lines' text,
which repeats in any file of size:

- **Moved** where hunks changed lines above it, by the lines they added less
  the lines they removed — and where it was, in a file that is still its blob.
- **Outdated** where a hunk changed one of its lines, or inserted lines between
  two of them, or the file is binary on either side, so nothing says where its
  lines went; it is answered with where the range would be had only the lines
  above it moved.
- **Gone** where the revision holds no file at its path — deleted, or a
  directory or a submodule now.

One `git diff-pairs` diffs every distinct pair of blobs the read names, each
handed as a numbered pair rather than by its path, so no path reaches git and
every hunk names the pair it is of.

### What changed between two revisions

The **interdiff** of two revisions is the diff of their heads kept to the
files either revision's change touches, as their facts list them: a rebase
between the two brought in what the target changed meanwhile, and the files
neither revision touches differ between the heads without being anything the
change did. It is read as a diff is — its stat a page at a time, or one file's
patch — and a file is kept by its whole name, so the touched path `a*` keeps
the file `a*` and never `ab`. The paths are judged by the node over git's own
records, never handed to git as a pathspec ([A path is data](#server-side-reads)).
Where either revision touches all, or the two share their base — whose heads
then differ only in files one of them touches — it is the heads' whole diff.

---

## Who a change wakes

Every record about a change carries **whom it concerns**: who made it, and the
people and seats each reason it routes under reaches, as the deciding node's
rows and chart held them when it decided the record. The node that decides
the record stamps it — the repository's committer for an open, an edit, a
close, a landing and a push; the change's writer for the conversation and
the home's facts and merges — through one function, so every writer answers
the question alike, and whatever tells somebody about the record afterwards
reads that snapshot and nothing else. The rows move on — a reviewer is
withdrawn, an author pushes, a thread grows — and an audience derived from
them minutes later on another node would tell different people about the same
record.

| Reason | Who hears it | On |
|---|---|---|
| `mention` | Every handle the text names that the chart holds — at most 32 a write | An open, an edit, a review, a comment; a push's options |
| `review_requested` | A reviewer asked who was not asked already; a path owner a revision's facts ask | An ask for review; a revision's facts |
| `changes_requested` | The change's authors | A review asking for changes |
| `conflicted` | The change's authors | The merge that **turns** into a conflict — not every recheck that finds it one still |
| `finding_replied` | The raiser of the finding a comment replies to, or an answer answers | A reply to a finding; an answer to findings |
| `ready_to_merge` | The change's owner | The mergeability whose stored answer goes from not ready to ready |
| `merged` | The change's authors | The push that sets the target to the change's head; the home's landing |
| `review_submitted` | The change's authors | A review that approves or comments |
| `re_review` | The reviewers whose standing verdict asks for changes on an earlier revision | A revision's first facts |
| `thread` | Everybody who said something in the thread | A comment |
| `watcher` | The change's participants: everybody asked, who reviewed, commented or was mentioned in its conversation | A close, a reopen, a retarget, a draft's state |
| `lead_fallback` | The lead of the repository's owner, when nobody who may review the change is asked | A change's first facts |

- **The order of the table is the precedence**: a handle a record names
  under two reasons hears under the first — mentioned and watching is told
  it was mentioned — and once a record, however many changes it names: a
  push that merges one change and opens another tells somebody named in both
  once, under the stronger reason. `mention`, `review_requested`,
  `changes_requested`, `conflicted`, `finding_replied`, `ready_to_merge` and
  `re_review` **ask** something of whoever hears them; the rest are news to
  absorb.
- **Nobody hears of a change they made.** A reviewer is not told of their own
  review, nor a pusher of the merge their push made — whoever wrote it.
- **A push that moves no change concerns nobody.** A head move, a close by a
  deleted source branch and a push to a branch no change uses are what a
  change's page shows, never what anybody is told; only a merge and an open
  route.
- **`ready_to_merge` is announced by one record.** Whether a change may merge
  ([Merge readiness](#merge-readiness)) reads rows on two subjects — the draft
  flag and the revisions on the repository's, the approvals and the answers
  to blockers on the change's — and two records on the two, landing together,
  are decided from two snapshots that each miss the other: an approval and an
  undraft that complete the readiness between them would each find it
  incomplete, and neither would tell anybody. So the change's
  [mergeability](#mergeability) record — its home's merge, or the refresh
  that follows any readiness input that moved — is the one that announces it,
  where the answer it stores goes from not ready to ready.
- **Every recipient is a row**: the record's applier writes one notice a
  handle it tells, on every data node — a person's inbox reads them, a
  change's page says whom its records reached, and each committed batch tells
  the dashboards watching an inbox that it moved. A person's notice is kept
  the company's `inbox.retention_days`, a seat's 30 days
  ([Retention](../guides/retention.md#old-data-is-not-cold-data)); a seat is
  woken from the record itself, never from its row.
- **Whom a change reached is read from those rows** (`code_change_routing`,
  [A change's questions](../reference/api-endpoints.md#a-changes-questions)):
  its records that reached anybody, newest first, each with every recipient
  and the reason it heard under. A change that lists nobody is one of two
  facts, and the answer says which: opened within the last 30 days, every
  notice it could have written would still be kept, so its records reached
  **nobody** — each concerned only whoever made it; opened before, its notices
  may have been **swept**, which is no evidence either way.
- **Each role is bounded**: at most 32 mentions, 32 reviewers asked, and 64
  of the authors, the participants, a thread's participants or the raisers an
  answer replies to, each read in its key's order — a change whose audience
  is larger than that is read in its listings rather than woken.

A mention is resolved against the chart where it is written, and held to the
deciding node's chart again, which also says who is a person: a write that
mentions anybody is decided only by a node that holds the company's current
chart, as a seat's write is. Whoever a comment or a review mentions joins the
change's participants, and hears what its watchers hear from then on; an
open's or an edit's mentions are told and join nothing, since those records
are the repository's and the participants are the change's own.

### How a seat is woken

A seat hears about a change by a **wake derived from the code log**, the way
it hears about a work item or a page
([Who a change concerns, and who it wakes](event-system.md#who-a-change-concerns-and-who-it-wakes)):
the log's own consumer, the group `crewlet-code-feed`, reads every committed
record on whichever data node takes it, and the notification service routes
each wake from the record's own routing, on any node — never from a row, and
never from the goroutine of the node that decided the record, so a node that
stops after its record committed loses no wake.

- **Only seats are woken.** A person reads their code notices in their inbox,
  from the rows above, and is never woken as a seat is; a record that tells
  only persons, or only whoever made it, is no wake at all.
- **A seat the company no longer employs is not woken**, and keeps its row:
  the chart is asked at the wake, so a reviewer who left after being asked
  hears nothing. The lead's copy (`lead_fallback`) wakes only where nobody
  else the record tells is still employed — a person among them, whose
  notice counts as somebody hearing.
- **One wake per seat per record**, its id derived from the record and the
  seat, so a record the feed handles twice — its claim fails open when the
  coordination store cannot be reached — wakes nobody twice. Every wake
  about one change is one conversation, keyed by the change's id: a rename of
  its repository splits nothing.
- **The wake names the change's work item**, and the turn it starts is
  charged to that item from its first record
  ([ADR-0067](../../adr/0067-a-change-implements-one-work-item-and-carries-no-spend.md)):
  the review a change was asked for, the fix its findings asked for and the
  answer to its conflict are all work on the item the change implements. A
  change carries no spend of its own — what a turn spent is on the turn and
  its item, never summed on the change.
- **Who wrote it bounds what the seat reads**: a review a colleague seat
  asked for is that colleague's errand, and a code read in the turn is
  decided for both grants ([Reads made for somebody else](#reads-made-for-somebody-else));
  the home's own records — a conflict, a change ready to merge — are nobody's.
- **What the seat is told** is the change by name, why it hears, whatever
  the record says in full — a comment, a review's summary, an ask's note, a
  description — and the tool that answers it: `code_review_change` for a
  review asked, `code_resolve_finding` and `code_comment_on_change` for
  findings and requests for changes, `code_comment_on_change` for a mention or
  a thread. It always reads the change itself first, with `code_get_change`:
  the wake says what happened, not what the change is. `mention`,
  `review_requested`, `changes_requested`, `conflicted`, `finding_replied`,
  `ready_to_merge` and `re_review` ask for an answer, which the seat delivers
  through a code write; the rest are news it may absorb in silence.

The feed holds the code log's trim: a record it has not acknowledged is one
no seat has been woken for, so the log keeps it
([The six terms](../guides/retention.md#the-six-terms)). Turned on over a
log that already holds records, the consumer starts from the first record the
log still has, and none from before a change existed carries a routing, so
that replay wakes nobody.

---

## A push, end to end

A person pushes with git's own smart HTTP protocol, presenting their own API
token as the password. The engine checks everything it can before it stores a
byte, receives the push with real git in a repository of its own, stores the
push's one pack, and has the repository's committer decide it — and tells git
a ref moved only once the record that moved it has been applied.

```mermaid
sequenceDiagram
  autonumber
  participant G as git push
  participant H as code host
  participant S as scratch receive-pack
  participant P as proc-receive hook
  participant O as object store
  participant C as committer
  participant L as code log
  G->>H: GET info/refs (receive-pack)
  H->>L: barrier: the repository's refs, now
  H-->>G: the advertisement — or a refusal git prints
  G->>H: POST git-receive-pack (commands, pack)
  H->>H: read the commands; refuse what needs no object
  H->>L: sync the cache through the barrier
  H->>S: receive-pack in a scratch repository
  S->>P: the ref updates git accepted, and the push's options
  P->>H: relayed over the node's hook socket
  H->>H: forced updates, blob sizes, new commits
  H->>O: upload the push's one pack bundle
  H->>C: propose the push
  C->>L: one record, arbitrated on repo.<id>
  L-->>C: applied — or durable, not yet applied
  C-->>H: the push's row, or pending
  H->>L: pending: read the push's row again, up to a minute
  H-->>P: ok or ng per ref
  P-->>S: the report
  S-->>G: what git prints
```

**The advertisement is read at the log's head.** Every answer that names a
ref — the refs a push or a protocol v0 clone is shown, a protocol v2
`ls-refs` — is read through the code log's barrier, so a node cut off from
the fleet answers 503 rather than an old head. An object fetch names ids that
mean the same bytes on every node, so it takes what the node's cache holds; a
fetch of an object the node has not seen yet — the client learned of it from
a node further ahead — makes the node read the log's head once before it
answers. The host does not offer protocol v2's `ref-in-want`: a client
offered it fetches by ref name, and git's own server and client disagree on
the order of a shallow fetch's answer when it does, so every
`git clone --depth=1` failed with
`fatal: expected 'packfile', received 'shallow-info'`. A client lists the
refs and fetches the ids it was shown instead, as it does from any server
that does not offer it.

**receive-pack never runs in a cache.** git moves a push's objects into the
repository it runs in *before* any hook can refuse them, so a push received
in a node's cache would leave a refused push's objects there — fetchable by id
from that one node, and a base a later push could build on that no other node
holds. So a push is received in a throwaway **scratch** repository whose one
alternate is the cache's packs — never the packs an expunge is dropping
([the cache on a git node](#the-cache-on-a-git-node)), so no push relies on
one — and its pack enters the cache only once its row has
applied ([ADR-0054](https://github.com/crewlet/crewlet/blob/main/adr/0054-a-repositorys-refs-move-only-by-records-and-a-cache-is-derived.md)).
The scratch is removed when the request ends, and a node that crashed with one
open sweeps it at its next boot.

**The hook is the engine's own binary.** receive-pack hands its ref updates to
the engine through a `proc-receive` hook, and that hook is `crewlet code hook
proc-receive` — this binary, started once a push, which relays them over the
node's hook socket and exits. Go initialises every package a binary links
before its first statement, so the engine's own initialisation is on every
push's path: measured at about 14.5 ms a start on a 4-vCPU machine
(`BenchmarkTheHookStartsUp`), nearly all of it the packages' initialisation,
beside about 2 ms for receive-pack's own start — a few per cent of a push that
also stores a bundle and waits for its record. What the engine's own packages
do at initialisation is held small by a test of their init trace, so a table a
package could build at first use is not built on every push.

**Its options are read where git hands them to the hook.** A push's options
(`git push -o`) reach the engine from receive-pack, after the ref updates it
accepted, and the engine reads a change from them there ([A change a push
opens](#a-change-a-push-opens)) — never from the request's own bytes, which are
what the client wrote rather than what git read. Every option but the
`change` ones is a forge's, read by nobody. receive-pack keeps every option a
client sends in its memory, so the node holds a push's command list and its
options together to 1 MiB before receive-pack reads either, and refuses a
push past it — far past the 33 KiB a change's options take at their longest.

**Checked before it is stored.** A push naming more than 1 000 refs, a credential
bound to no seat, an archived or deleted repository, a deletion or a direct
push a protection refuses — each is refused before receive-pack runs, and
nothing is stored. A forced update of a ref a protection keeps from forcing,
and a blob larger than `code.limits.max_blob_bytes`, are known only once the
objects are, so the hook checks them — and refuses the push before uploading
anything. A ref keeps the first refusal it meets: a force-push of a protected
branch is answered `protected_force` even when its pack also carries a blob
over the limit, and a push every ref of which is refused by then is not
scanned for blobs at all. A blob over the limit refuses every ref still
standing, since the pack is stored whole or not at all, and names the path any
ref of the push reaches it by. A push carrying objects into a repository that
holds the most live packs one may ([Limits](#limits)) is refused
`compaction_behind` before anything is uploaded too, so a retry whose own sync
already finds the repository there uploads nothing — but only once the node
has read the repository's row again, from a data node's copy at least as new
as that sync (its own, where it holds one): a compaction that copy has applied
never refuses the push, while one the repository's committer has applied and
the copy not yet still can, and the push's next run meets it. A read that
cannot be made, or finds the name claimed by another repository or the
repository archived or deleted since, leaves the push to its committer, which
judges it for good, as it does every push whose own sync found the repository
below the bound: one it refuses, because pushes that landed since took the
repository there, has uploaded its pack, which no row names and the
[object collector](object-store.md#collection-and-audit) deletes a day later.
A [coding box](#coding-boxes)'s push whose pack holds a commit committed as the
engine, which the repository does not already hold, is refused
`forged_committer` before anything is uploaded too: the committers of a box's
pack are read before its upload begins, never beside it.
Only a push that passes every check is uploaded, as **one** object: its pack
with the indexes the receiving git wrote.

**A clock that is behind.** git walks a repository's commits newest first by
date wherever it has no generation number to stop on, so a commit dated before
its parent — a laptop's clock that is behind, a `GIT_COMMITTER_DATE` — is a
reason to walk the whole history. The engine's own checks of a push walk by
generation once the node has written the repository's commit-graph, which it
does first for every copy it reads whole, so they cost a few milliseconds
whatever the commit's date; git's own connectivity check in receive-pack
still walks by date, reading the graph. Measured over a gigabyte of source
history (455 680 commits) on the node pin, git 2.56.0, on two E-cores of a
Core Ultra 7 265U, each push's bundle put to the in-memory store, such a push
is 0.30 s of the node's time with the graph against about 0.09 s for any other
push, and 6.7 s in the seconds after a node has read the repository whole and
before its graph is written. No walk
is refused for what it takes — each is given 11 µs for every object the
repository holds besides what the push itself is given. The person's own git
pays too: a clone with no commit-graph of its own spends about 1.6 s over the
same gigabyte checking that such a push is a fast-forward, and
`git commit-graph write --reachable` in the clone spares it.

**The commands are read as receive-pack reads them.** Every check before
receive-pack runs is a check of the ref updates the engine read from the
request, and receive-pack moves the ones *it* read — so the engine reads them
by git's own rules: an object id in either case, the capabilities whichever
line asks for them, every ref to the end of its line, and the commands up to
the packet that ends them. A client built by hand that writes its ids in
capitals is held to every check git's own client is. A **push certificate** —
a signed push — is refused before anything runs: the engine never advertises
one, so `git push --signed` stops in git itself, but receive-pack acts on the
ref updates a certificate names whatever it advertised.

**An `ok` means the row applied.** The committer answers with the push's row
once its record has been applied. A record that is durable on the log but not
yet applied on the node that answered is **pending**: the host reads the
push's row at once and again — after a quarter of a second, then at
doubling intervals up to two seconds — for up to a minute (the state log's
own stall grace), and answers git from the row if it appears. It never
reports a pending push as
`ok`, because a durable record can still be dropped — by an eviction, on
every node — and then the push landed nowhere. While a push waits, the hook
prints a line every five seconds naming the step it is on, so a balancer's
idle timeout does not cut a push that is merely waiting.

**What a refusal names.** A push answered `pending`, `unknown` or
`not_applied` is not a push that failed in the ordinary sense — each says what
is known about it:

- `pending` — the record is durable at the position named and was not applied
  here within the grace. Run the same `git push` again: a push that landed
  reports `Everything up-to-date`.
- `unknown` — the answer was lost (the node committing it went silent, or the
  client's connection ended first): it may or may not have landed. Fetch, then
  run the push again.
- `not_applied` — the record was durable and every node dropped it. Nothing
  landed; run the push again.

Every push — applied, refused or failed, at the advertisement or at its body —
is one `operator_acted` record on the node that received it, with transport
`git`, the author and the credential exactly as every record a write leaves
names them — the author the human seat the principal holds (a credential bound
to none, whose push is refused, under its own login), the credential
`pat:<credential id>` for a machine token or `token:<id>` for a Tier A token —
the repository and what became of the push. That holds however the push's
request ends: a push the node answered `503` because its sync could not read
the code log, one stopped by the node's own failure on its way to
receive-pack, and one whose body the node could not decode are each recorded
`failed`, and one whose request declares more than `code.limits.max_push_bytes`
allows is recorded `refused` (`push_too_large`) as whoever sent it. A request
is recorded once the node knows who sent it and to which repository: one
refused before that — a request that is not git's, its credential refused or
unresolved, a repository the node does not have, the node stopping — names
nobody and nothing, and is not recorded. A fetch is not recorded.

**Large fetch requests.** git's own HTTP server holds a fetch request in
memory and refuses one past 10 MiB, and a partial clone breaks that: a
checkout after `git clone --filter=blob:none` asks for every blob it lacks in
one request, fifty bytes a blob, so a tree of more than about 210 000 files
sends more than 10 MiB. The engine holds a request in memory as large as it
is, up to a mebibyte — a clone's negotiation is a few kilobytes, and costs
that — and spools a larger one, whole, to an unnamed file on the cache's
volume, removed when the request ends however it ends. What every request
holds in memory at once comes out of a node-wide allowance, a mebibyte for
each transfer slot, so a burst of fetches waiting for their slots is bounded
by the node; a request that finds it spent is spooled rather than refused. A
request is bounded by **its repository**: fifty bytes for every object the
repository's packs hold and every live ref, beside the mebibyte — the most a
request can legitimately name — and one past it is refused 413. The spools
come out of an allowance of their own, 64 MiB for each transfer slot and never
the cache's budget, and a spool takes its room as its client sends — a
mebibyte at a time, given back to what it holds the moment its body ends — so
it holds what its client sent, never what its repository's bound would allow.
While its body arrives a request also **claims** the most it can grow to: at
most 64 MiB, about 1.3 million objects, or the length its client declared,
which git does not declare for a request this large. The node keeps the
requests arriving able to finish, oldest first: a request is given room only
while every request older than it could still reach its claim once the spools
whose bodies have ended are given back. So two requests can never each hold
part of the room waiting for the other's, and nothing counts requests — as
many may be arriving at once as their bytes leave room for, and a slow client
(the floor every transfer is held to is half a minute a mebibyte) holds what
it has sent, and keeps one claim free of the requests behind it. They wait
for it only once the requests arriving hold all but that much of the room:
960 MiB of a four-core node's gibibyte. One credential may hold a quarter of
the room at once — 256 MiB on a four-core node — and the same rule keeps its
own requests arriving able to finish within its quarter. A request that finds
no room, or its credential's quarter spent, **waits its turn for it, up to
55 s**, and is answered 503 only if its turn has not come by then — because
git does not retry a lazy fetch, and a blobless checkout whose fetch is
refused fails. The turns are first come, first served within a credential;
across credentials the node's room goes in arrival order, and a credential at
its quarter holds back only its own requests, so a CI fleet behind one token
never makes a person wait behind it, and a request that can start at once
starts at once, whatever else waits. A request waiting to start holds the
part of it already in memory in a place of the node's waiting room — four
places a transfer slot, a mebibyte each: 64 MiB on a four-core node and
256 MiB on sixteen cores — never in the memory every other request shares,
and once it starts that part goes into its spool, so a spooled request
waiting for its sync holds no memory at all. A wait is bounded by the idle
timeout a balancer gives a quiet git request: a waiting fetch has been sent
nothing, and the deployment guide's minute less one keepalive interval
answers its 503 before the balancer would cut it. A request that must wait
and finds the waiting room full, or its credential's quarter of it — a lane's
width of one credential's requests waiting — is answered 503 at once. A
waiter whose client goes is seen to go when the node next reads its
connection, at its turn: the engine's listener speaks HTTP/1.1, where a client
that hung up is heard only by reading from it.
`crewlet.code.host.spool.waits` counts every wait by how it ended.

A spooled request holds its room until the answer's first byte — through its
sync, git's read of it, and, for a fetch, its `pack-objects` before the pack
begins (at most a progress line or one keepalive, 5 s, for a client using
side-band) — so what a request behind it waits for is that sync: a moment on a
node that holds the repository, and on a node that does not, a **cold** sync —
about 27 s of downloads alone at the 2 048 live packs a repository may hold
([Limits](#limits)) from S3's slower first bytes, and more over a large
repository's bytes or a restarted node's checks of its indexes, where 10 000
packs took 70 to 130 s — plus each fetch's pack-objects, which together can
outlast the wait. Behind one token, as many checkouts large enough to spool run
at once as their requests fit in the token's quarter of the room, and the rest
wait for them; on a cold node, once that outlasts the wait, the ones past the
quarter fail. N runners asking for
B bytes each fail their checkouts there once N × B passes the quarter:
thirteen lazy fetches of 20 MB fill a four-core node's 256 MiB, and the
fourteenth and every one after it are answered 503. So one token behind many
runners costs more than latency: a token for each CI runner moves the limit to
the node's whole room, fifty-three such fetches on a four-core node, which one
cold sync can fill too, and routing each repository's requests to the nodes
that hold it (the [deployment guide](../guides/deployment.md)) is what keeps
the syncs warm. A request past what the node spools for one request —
at most 64 MiB, about 1.3 million objects, a quarter of the allowance on a
node of fewer than four transfer slots — is answered 413 at once, since this
node will never take it: fetch fewer objects at once, as a sparse checkout
does. A request's memory and its spool are given back at the answer's first byte —
for a fetch, its pack's, after `pack-objects` has run (at most a progress line
or one keepalive for a side-band client) — not when a slow client finishes
downloading. A
blobless clone needs protocol v2, git's default since 2.28 — under v0 a fetch
of a blob the client never saw advertised is refused.

**The partial-clone filters served.** A cache serves the filters git can
answer from a repository's bitmap and no others: `blob:none`, `blob:limit=<n>`,
`tree:0`, `object:type=<type>`, and a `combine:` of them. git refuses the rest
in its own words — `filter 'sparse:oid' not supported`, and
`tree filter allows max depth 0, but got <n>` for `tree:<n>` past 0 — because
git answers those by walking every object the clone reaches, whatever the node
holds. The served ones are read from the bitmap where the node holds one, and
it does not always: until the node's first background index of its copy of a
repository, a rebuilt copy included — which waits for the runs ahead of it and
for room in the node's memory, so a node that has just joined indexes the last
of its repositories only after all the others, on four cores a quarter of an
hour or more for 50 GB of them — and for a repository whose bitmap is past the
node's memory, which it indexes without one (the
[`code_maintenance_refused`](../reference/alarms.md) alarm says which;
[background indexing and memory](#the-cache-on-a-git-node) say what each
costs). There every clone, filtered or not, walks the history it reaches,
bounded only by the node's transfer slots and their deadlines — so plan the
memory of a node serving blobless clones of its largest repositories from the
bitmap's figure.
No way of working is lost by the refusals: a blobless clone with a sparse
checkout fetches the files a `sparse:oid` would have named, as it needs them.
It is housekeeping, not a security boundary — a shallow clone also does without
the bitmap and is served, held like every transfer to the node's transfer slots
and their deadlines.

---

## A merge, end to end

A change merges into its target by **one merge record**
([ADR-0066](../../adr/0066-a-protected-branch-moves-only-by-a-merge-whose-preconditions-one-snapshot-proves.md)):
the record that marks the change merged is the record that moves its target,
decided by the repository's [committer](#the-committer) from one snapshot. A
merge is built elsewhere — its commit and its pack written by the repository's
[home git node](#the-home-git-node) onto the tip it read of the target — and
proposed with what it was built from: the target's tip (its **base**), the
change's head, the commit the target moves to and how it merged (`merge`, a
merge commit; `squash`, one commit; `fast_forward`, the target moved to the
head itself, building nothing). The proposal's operation id names the merge:
its push id is derived from it, so a merge asked again — on the same node, or
on the repository's runner-up after silence — is the same merge, collapsed by
the log's ledger onto the record that landed.

```mermaid
sequenceDiagram
  autonumber
  participant B as home git node
  participant C as committer
  participant L as code log
  participant A as every data node
  B->>C: merge: the change, its head, the base, the commit, the pack
  C->>C: one snapshot — the change, the target's tip, the gate
  C->>L: the merge record, arbitrated on the repository's subject
  L->>A: apply: the change merged, then its push via merge
  C-->>B: applied, from the change's row
```

**The decide proves every precondition in one snapshot**, in this order, and a
merge it refuses appends nothing:

| Refusal | Why |
|---|---|
| `repo_deleted`, `repo_archived` | The repository is gone, or read-only. |
| `repo_importing` | The repository is being imported from another host, and takes no write until its import is done. |
| `repo_mirrored` | The repository is a pull mirror, which moves only by its syncs from its source: a merge its source made is recorded once a sync brings it in. |
| `change_not_found`, `change_not_open`, `change_draft` | The repository holds no such change, or it is closed or merged, or a draft. |
| `stale`, `head_moved` | The change targets another branch, or its head moved, since its merge was built: read it again. |
| `target_moved` | The target is no longer at the base the merge was built onto — read off the snapshot's refs, never off the proposal: its builder builds it again onto the tip the branch holds. |
| `not_person` | The merge overrides the gate and its asker is no person. |
| `queue_target` | The target's gate requires a check, so a change merges through its [merge queue](#the-merge-queue): only a person's override merges into one directly. |
| `gate_unmet` | The change does not meet its target's [merge gate](#the-merge-gate) with the asker as the merger — every precondition but `mergeable`, which the merge's own build proved against its base: the answer names every one not met and whom it waits on. |
| `source_refused` | The merge deletes its source, and a push deleting it would be refused — a protection that keeps the branch, or, for a source outside the seats' namespace, the merger's grant. Asked keeping its source, it merges. |
| `upload_expired`, `repo_size`, `compaction_behind` | The merge's pack, held to a push's rules: uploaded too long ago for a record to name, past `code.limits.repo_max_bytes`, or into a repository whose compaction has not caught up with its packs. |

**A person may merge past the gate**: a merge carrying their **override** —
their reason, at most 1 KiB — is decided without the gate, on a queue target
too, and the reason is kept on the change's row and announced with the merge.
A seat's override is refused `not_person`, and so is a token bound to
nobody's, which merges nothing at all. **The chart judges every merge, whoever
asks** — who may approve the change, the lead chains above the owners it
touches, a seat's merge authority — so a node that holds no chart yet, or one
[behind the company's configuration](#a-seats-push), decides no merge and
hands it to the repository's next committer.

**The record does everything a merge is, in one apply**, every row at its one
position:

1. **The change merged first** — `merged_via: direct`, `merge_oid` the commit,
   `merged_by` who merged it (a seat by its handle, a person by their human
   seat, with the credential they merged with), and a person's override
   reason. First, so the push that follows finds it merged: a push that sets a
   target to a live change's head merges that change, and one that deletes a
   live change's source closes it — and this change is neither.
2. **Its live queue entry ended**, where a person's override merged a queued
   change directly: dequeued, `merged`.
3. **The target's landing row**, where a rule protects it (`kind: direct`, the
   base and the commit, who merged it) — and **its green row** moved to the
   commit, where its gate requires no check, so the landing is green as it
   lands.
4. **The push**, through the one apply every push takes: the target moved from
   its base onto the commit; the source branch deleted where the merge asks it
   — a seat's own branch by default (`delete_source: seats`); the merge's pack
   (`kind: merge`); and a push row **`via: merge`** naming who merged it and
   the commit it introduced — none for a fast-forward, whose head the change's
   own pushes brought — so [which push landed a
   commit](#which-push-landed-a-commit) answers for a merge as for any push.

The node whose committer landed the record announces it once: a
`change_merged` event naming the change, the branch, the method, the commit,
who merged it and any override — fed to the activity feed, and never sent for a
retry the ledger collapsed. `crewlet.code.merge.outcomes` counts every merge a
node's committer was asked to decide, by what became of it: `target_moved`
climbing is a branch moving faster than its merges are built onto it.

### How a merge is built

A merge is an [engine write](#engine-built-commits) of kind `merge_change`,
asked of the repository's [home git node](#the-home-git-node) like any: the
change by its number, the head its caller read, the method, and — for a
person — an override's reason, at most 1 KiB.

1. **Judged before anything is built.** The node's copy is synced to the
   caller's floor and the change read there, as its caller: a change that is
   gone, closed or merged, a draft, into another branch or at another head is
   refused with the record's own codes (`change_not_found`,
   `change_not_open`, `change_draft`, `stale`, `head_moved`), and a seat's
   override `not_person`. For a merge its gate holds, the change's
   readiness — judged with its asker as the merger — must be met but for the
   merge its build proves, or it is refused `gate_unmet`, naming every
   precondition and whom it waits on. Every refusal is judged again at the
   log's head before it is answered, and a change found merged is first
   looked up as this merge's own: asked again, a merge that landed is
   answered from its row, nothing built. So an unready merge costs a read,
   never a build and an upload.
2. **A queue target enqueues.** A target whose [merge gate](#the-merge-gate)
   requires a check lands its merges through its [queue](#the-merge-queue):
   the merge builds nothing, and the change is enqueued at the head its
   caller read — the enqueue's decide its gate — answered with its entry and
   its place. A person's override merges directly instead.
3. **Built onto the tip the copy holds**, in a scratch beside the copy, under
   one unit of the node's seat git-work share and **thirty seconds of git** —
   the budget a change's [mergeability](#mergeability) is merged in, since it
   is the same merge. A `fast_forward` builds nothing: the tip must be an
   ancestor of the head, or it is refused `not_fast_forward`. A `merge` or a
   `squash` runs one `git merge-tree --write-tree` of the tip and the head —
   histories sharing no commit merge from the empty tree, as the
   mergeability measures them — answering a `conflict` with its paths, or
   `nothing_to_change` where the target holds the head already.
4. **Its commit's author** (U43, answered (a)): a **merge commit** — the tip
   and the head its two parents — is authored by **who merged**, the person
   or the seat; a **squash** — one commit on the tip, titled with the
   change's title and reference (`Retry the upload (web!42)`) and carrying
   its description — by **the change's owner**, with a `Co-authored-by:`
   trailer for each of its other authors in the order they first wrote it,
   a seat that has since left the chart named by its handle at the identity
   domain. The engine commits it, `Crewlet <engine@<code.identity_domain>>`,
   its author date the instant the merge was asked and its committer date
   that or its later parent's, so a merge asked again builds the same commit.
5. **Landed as a merge record** through the one landing every push takes: its
   blobs held to `code.limits.max_blob_bytes`, its pack to 4 MiB and the
   repository's bounds, its bundle stored, the record proposed to the
   repository's committer — whose decide is the authority on every
   precondition above — and its pack taken into the node's cache once the
   record applied.
6. **Built again onto a target that moved.** A target that moved between the
   build and the decide is refused `target_moved`, and the merge is built
   again onto the tip a sync at the log's head finds — at most **four
   times**; moved under the fifth build too, it is answered `target_moving`:
   the branch moves faster than a merge is built onto it, and its queue, or a
   quieter moment, lands it.

A router gives a merge **102 seconds** on one git node — an engine write's 82
with its ten seconds of build replaced by the merge's thirty — and every other
write its 82. `crewlet.code.host.writes` counts it under `merge_change`, and
`crewlet.code.host.write.duration`'s `build` stage times its builds, every
rebuild included.

**A protected branch so moves only by a merge record whose preconditions one
snapshot proved**, or by a person's own push under `direct_push: persons` —
that person's recorded override, their push row naming them. No seat's push
moves a protected branch: every [protection rule](#protection-rules) refuses
it.

---

## Engine-built commits

A change a seat or a person makes through a tool — files committed, a commit
reverted or cherry-picked, a branch created or deleted, a review's suggestions
applied — is built by the code
host itself, on a git node holding the repository ([its home](#the-home-git-node)
first), and lands as a **push**: the same landing a person's `git push` takes
from its pack on, held to the same rules, one record on the code log, and
answered applied only from that record's applied row. Every node asks a write
of the repository's home git node first ([Which git node builds a
write](#which-git-node-builds-a-write)); a seat's code tools and a person's
assistant's ask for one, each call one write ([The company's
code](../guides/tools-and-mcp.md#the-companys-code)).

| Kind | What it does |
|---|---|
| `commit_files` | One commit of up to 1 000 file operations onto a branch's head — or onto a start commit, creating the branch, or onto nothing, a root commit. A `put` makes a path a file of the text it carries, or of a blob the repository holds by its id, with mode `100644`, `100755` or `120000` (a link, whose target is its text, never a blob); an `edit` replaces text that occurs in the file **exactly once**; a `delete` removes a file, a link, a submodule or a whole directory; a `move` moves an entry to another path, sending no bytes; a `mode` flips a file between `100644` and `100755`. What is judged is the tree as the whole call leaves it, so deleting `a` and putting `a/b` is one change. |
| `revert` | The inverse of one commit onto a branch's head, with git's own message — `Revert "<subject>"` and `This reverts commit <id>.` — unless the write gives one. |
| `cherry_pick` | One commit's change onto a branch's head, with the commit's message and `(cherry picked from commit <id>)`, as `git cherry-pick -x` writes it, unless the write gives one. |
| `create_branch` | A branch at a commit the repository holds. |
| `delete_branch` | A branch, at the head its caller read. |
| `apply_suggestions` | Up to 64 findings' [suggestions](#findings) put in place of the lines they are anchored to, on a change's source branch at the head its caller read, as one commit — all of them or none ([A review's suggestions applied](#a-reviews-suggestions-applied)). |
| `merge_change` | A change merged into its target — a merge commit, one squashed commit or a fast-forward — at the head its caller read, landed as one **merge record** rather than a push; or, on a target whose merges land through its queue, enqueued ([How a merge is built](#how-a-merge-is-built)). |
| `rebase_change` | A change's commits its target does not hold replayed onto the target's tip — each keeping its author — and its source branch moved to them from the head its caller read by one forced push ([A change rebased](#a-change-rebased)). |

Every write that moves a branch names the head its caller read, so a write
onto a branch that moved since is refused `stale` rather than land on a
history its caller never saw, and one that creates a branch needs it absent.

### How a write is built

1. **Judged before anything is built.** The node's copy of the repository is
   brought to the caller's floor — a person's linearizable; a seat's its own
   writes, its turn's read of the repository and where every ref the turn
   read on it last moved, so a write onto a head the seat read with
   `code_resolve`, or naming a commit by a branch, finds them in the copy
   after one sync — and the write is judged on it: the repository's status, the
   writer's [grant](#who-may-read-and-push), the [protections](#protection-rules)
   — a seat's write to a ref `direct_push: persons` protects is refused
   `protected_direct`, as its push would be ([A seat's push](#a-seats-push)) —
   where the branch is, by its exact name, and what every object the write
   names by its id is. A head that is not the one the caller read is first
   looked up as **this write's own push**, landed by an earlier ask of it, and
   found, it is the answer. Every refusal is judged once more at the code log's head
   before it is answered, so a copy that has not yet read a protection's
   removal refuses nothing a current one would let through. A seat's write on
   a node [confirmed behind the company's
   configuration](control-plane.md#where-the-gate-sits) is answered by that
   node as one it cannot serve, so another git node builds it; a person's
   write is built there.
2. **Built beside the copy, over what it touches.** In a scratch repository
   whose one alternate is the copy — never in the copy, which stays a function
   of the rows — by git's own plumbing over exactly the directories the change
   touches: one read of what every path the write names and every directory
   above one is, one read of those directories and the files an edit
   rewrites, one `hash-object` of the new files, one `mktree` a level of
   directories — at most 64, since a path a write names is at most 64
   components deep — deepest first, every entry the change does not touch
   passed through as it stands, and one `commit-tree`; a revert or a
   cherry-pick runs one `merge-tree --write-tree`, whose cost follows the
   trees' differences. Never an index of the whole tree or a checkout, so what
   a commit costs follows the entries of the directories it touches, never the
   files the repository holds: a commit beside sibling directories of ten
   thousand files asks git exactly what one beside directories of ten does.
   The build takes a unit of the node's [seat git-work
   share](#server-side-reads), given back before it lands, and one budget of
   ten seconds of git run time that every process of it shares. The read of
   what the named paths are is the build's, never a quick check before it: git
   walks each path from the root, a directory at a time, so its cost grows
   with the square of a path's depth — a thousand paths at the depth bound,
   each with every directory above it, took 19 s of git on two E-cores of an
   Intel Core Ultra 7 265U — and a write too wide or too deep for the budget is
   refused `too_large`, never taken for a git node that failed and asked of
   the next.
3. **Checked as a push is.** The new objects are packed into one
   self-contained pack, indexed by `git index-pack --strict` — the check
   receive-pack runs on every person's push (`receive.fsckObjects`) — so a
   write stores nothing the code host would refuse a `git push` of at the
   door: a `.gitmodules` whose url or path git refuses, a `.gitattributes` past
   git's bounds, a malformed tree.
4. **Landed.** From here it is [a push](#a-push-end-to-end), its row saying
   `via engine`: the bundle stored, the proposal to the repository's
   committer, the applied row, the pack adopted into the copy. A caller whose
   time left cannot hold the bundle's upload and the proposal after it is
   answered `busy` before anything is uploaded, so a slow object store costs a
   refusal, never a stored bundle nobody proposes.

### Who a commit names

- **Its author is whoever acted**: a seat's role `name` and the `email` its
  role states, as the company document writes it, or `<handle>@<domain>` on
  [`code.identity_domain`](../getting-started/configuration.md#code) where the
  role states none a commit can name — a `${VAR}` reference is never resolved
  into history. A person writes as the human seat their credential is bound
  to, by that seat's role and handle, whichever of their credentials they
  used; a credential bound to no seat writes nothing.
- **Its committer is the engine**, `Crewlet <engine@<domain>>`. That marks, in
  a cooperating client's history, the commits the code host built, and
  authenticates nothing: any git client can write the same committer into a
  commit it pushes. Who built a commit is the push row's — `via engine`, its
  principal and, for a seat, the turn that made it. A [coding
  box](#coding-boxes)'s push carrying a commit under it that the repository
  does not already hold is refused (`forged_committer`), while a person's or
  an import's may carry one.
- **Its author date is the instant the writer acted** — the instant the
  write's operation was minted, to the second — and its committer date is
  that instant, or its parent's committer date where the parent is dated
  later (a person whose clock runs ahead), so no commit is dated before its
  parent: git's walks by date, merge bases and the commit-graph read the
  committer date alone, and a child dated before its parent sends every walk
  by date through the whole history. git's own `rebase` and `cherry-pick`
  keep the author date and set the committer date the same way.

Nothing else is added to the message — no trailer naming the seat, its run or
its work item: the push row carries those, and only what the row says is the
record of who made a commit.

### A retry is the same push

The push a write lands under is derived from the caller's operation, never
minted, and the commit is a pure function of the operation and its parent:
the names and dates above, the tree, the message. So a write asked again —
on the same git node, on another after the first went silent, after a crash
at any step — is the same push, which the repository's committer lands
once, carrying the same commit. A second ask on a node still building the
first waits for it and is answered what it is; asked again after it landed,
and after the branch moved on, a write is answered from its push's row with
nothing built; and a write that cannot learn whether it already landed is
answered `busy` — make the same call again — never `stale`.

### Which git node builds a write

A write is asked of the repository's [home git node](#the-home-git-node) first
— the node its reads are asked of, whose copy they keep warm — then of its
runner-up, in the order every node computes alike, whichever node the write was
asked through
([ADR-0059](https://github.com/crewlet/crewlet/blob/main/adr/0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md)).

- **A git node that runs none of it is passed over.** One whose code host is
  stopping, or that holds no configuration yet to sign the commit by, answers
  that it ran nothing: the write is asked of the next git node at once, and the
  node that refused is asked last for thirty seconds. One that cannot build it
  in time for a reason of its own — its copy still syncing, its seats' share of
  the read lane full, already holding every write its writes' lane holds, or
  behind the company's configuration, for a seat's write — is passed over the
  same way, and asked first again next time.
- **One that goes silent is passed over with the same write.** The write's push
  is its caller's operation's ([A retry is the same
  push](#a-retry-is-the-same-push)), so the next git node builds the same push
  and the same commit: where the silent node had landed it, the next answers
  from the push's row with nothing built, and where it had not, the
  repository's committer lands the one push once.
- **A write waits at most 82 seconds on one git node**, whatever its caller
  allows: the waits of every stage at their whole budgets — a unit of the node's
  share (2 s), three reads of the rows (6 s), the build's git (10 s), the
  bundle's upload (30 s), the proposal (33 s) and the answer's way back (1 s).
  A git node fits the write into that, answering `busy` before anything is
  uploaded where the proposal would no longer fit, so a healthy node always
  answers inside it and only a wedged one costs it — after which the next git
  node is asked.
- **Writes travel on a lane of their own** on each git node, beside the reads':
  a write holds its slot through its upload, its proposal and the wait for its
  row, so on the reads' lane a burst of writes would hold the slots every read
  of the node's repositories is answered through. What bounds the git a node's
  writes run is its [seat git-work share](#server-side-reads), never the lane.
- **A git node holds at most 64 writes for other nodes at once, and answers
  the next at once** that it is busy, without building it, so its walk moves on
  to the next git node that holds a copy of the repository — or ends, saying
  the node is busy — within milliseconds. Left to wait for a slot, a write
  waited in the node's broker client, which drops what it holds past 64 MiB:
  the asker of a dropped write heard nothing for 82 seconds and then took the
  busy node for a gone one, sending every repository it is home to elsewhere
  for thirty seconds. A held write takes about 3 MiB of the node's memory at
  the largest a write may be, so the 64 take about 200 MiB. A node's own
  seats' writes are built in its process and count against none of the 64.
- **A write no git node would take is refused before any is asked** — a branch
  git refuses, a path a checkout cannot make, a seat's write that names no
  turn — naming what is wrong.
- **A write that landed is in the next read made through the same node.** It
  raises that node's floor on its repository, so the node's next read of the
  repository, on whichever git node answers it, includes the write.

### What a write is refused

A write the request itself gets wrong is refused before anything is read:
more than 1 000 operations, more than 1 MiB of operations or suggestions and
message — or, for a revert or a cherry-pick, of message — as JSON spells them
(where `<`, `>`, `&` and most control characters take six bytes each, so a
message of them reaches the bound at a sixth of its length), a path it names
more than 64 components deep, two operations on one path, a message with a NUL
byte or text git would rewrite, and a new path git or a checkout would refuse or misread —
past 4 096 bytes, a component past 255, an empty, `.` or `..` component, a
control character, a backslash, a code point HFS+ ignores, `.git` in any
spelling git reads as one (any case, trailing dots and spaces, an NTFS stream
such as `.GIT::$INDEX_ALLOCATION`), and a link named `.gitmodules`,
`.gitattributes`, `.gitignore` or `.mailmap`, which git refuses or ignores.
Paths a write does not touch are never judged.

A refusal of the **push** — `stale`, the grant's `not_permitted`, a
protection's, `blob_too_large`, `repo_deleted`, `busy` — says what a push's
says. One the git node reaches stores and records nothing; one the
repository's committer reaches is recorded as any push's is. A refusal of the
**change** stores and records nothing:

| Refusal | Means |
|---|---|
| `conflict` | A revert or a cherry-pick does not apply cleanly, or a merge does not merge cleanly. The answer names each conflicted path, git's word for the conflict (`CONFLICT (contents)`, `CONFLICT (modify/delete)`, …), the ids of its base, ours and theirs — each in the repository, readable whole by its id — and the first 2 KiB of the file with its markers; as many as fit the answer, the rest counted. |
| `nothing_to_change` | The change leaves the tree as it is; no commit is made. |
| `edit_not_found`, `edit_ambiguous` | An edit's text occurs nowhere in its file, or more than once (the sentence names the first two lines). |
| `edit_binary` | An edit of a file git reads as binary: a NUL in its first 8 000 bytes. |
| `edit_line_endings` | An edit spanning a line, in a file holding a carriage return. |
| `path_conflict` | A file where a directory is needed or the reverse, or one path beneath another the same call makes, keeps or moves. |
| `path_not_found` | An edit, a delete, a move or a mode of a path the tree does not hold. |
| `not_a_file` | An edit or a mode of a directory, a link or a submodule. |
| `merge_commit` | A revert or a cherry-pick of a merge, or a rebase of a change whose history holds one: a merge needs a mainline. Make it in a coding box. |
| `not_fast_forward` | A `fast_forward` merge of a change whose head does not hold its target's tip: merge or squash it, or rebase it onto its target first. |
| `too_many_commits` | A rebase of a change holding more than 1 000 commits its target does not. |
| `too_large` | The write's pack is past 4 MiB, or its build ran its ten seconds of git — a merge's thirty, a rebase's forty. |
| `rejected_object` | An object the build made fails `index-pack --strict` — the sentence is git's own first error, naming the object and the check — or a commit a rebase would replay that git would not write back as it was: an author in a form git trims, or a message in another encoding than UTF-8. |
| `suggestion_outdated` | A suggestion whose finding's lines changed since they were read — a hunk between the blob they were read in and the head's touched them — or whose file is gone or no regular file at the head, or holds fewer lines than it names; every such finding is named, and nothing is applied. |
| `suggestion_overlap` | Suggestions whose lines, placed in the head's file, overlap; every finding in an overlap is named beside one it overlaps, and nothing is applied. |

### A review's suggestions applied

A finding may carry a **suggestion** — the lines to put in place of its own —
and `code_apply_suggestions` applies the suggestions of the findings it names,
on the change's source branch, at the head its caller read, as one engine
write ([The company's code](../guides/tools-and-mcp.md#the-companys-code)).

- **Each suggestion goes where its finding was shown.** Its lines are placed in
  the head's file by the rule a finding is shown on a later revision by — the
  diff between the blob its lines were read in and the head's, every hunk above
  them moving them, one touching them outdating them — through one `git
  diff-pairs` of every distinct pair of the two blobs, whose hunks are placed
  as they stream, never held whole. A suggestion read in the head's own blob is
  where it was read.
- **All of them or none.** One outdated suggestion, or two that overlap,
  refuses the write naming each finding, and nothing is committed: nothing a
  reviewer did not see is put anywhere.
- **The file is rewritten as it streams**, the lines a suggestion replaces left
  out and its text put in their place, in one pass over the suggestions in
  order, and committed as a `commit_files` is; its last line ends with a
  newline exactly where the head's did.
- **The commit is the applier's**, its author whoever applied it, so the push
  that lands it makes the applier an [author](#changes) of the change, whose
  approval of it then counts for nothing.
- **Each finding is then answered `resolved`** with the commit, by a second
  write — a step of the same operation, decided at or past the push's record,
  where the applier is an author. An answer leaves a blocker standing for its
  raiser to judge ([Findings](#findings)): applying a suggestion answers a
  finding, it never clears one.

### A change rebased

`code_rebase_change` rebases a change onto its target's tip as one engine
write of kind `rebase_change`: the change by its number, and its source branch
at the head its caller read. The build:

1. **Lists what to replay**: one `git rev-list --topo-order --parents` of the
   head less the target's tip, oldest first — at most 1 000 commits
   (`too_many_commits` past it), none a merge (`merge_commit`).
2. **Replays each commit** onto what is rebased so far: one `git merge-tree
   --write-tree --merge-base=<its parent>` of the tip and the commit — a
   cherry-pick of it — and one `git commit-tree`. The **first** that conflicts
   stops the rebase with nothing stored, its answer naming the commit (`2 of
   5`) and its conflicted paths as a revert's are. A commit whose change the
   tip already holds is dropped, as `git rebase` drops it.
3. **Keeps each commit its author's** (U43, answered (a)): its author's name,
   address and date — with the zone it was written in — and its message byte
   for byte; its committer is the engine, at the instant the rebase was asked,
   or its new parent's committer date, or its own author date, whichever is
   latest, so no commit is committed before it is authored or dated before its
   parent, and a rebase asked again builds the same commits. A commit git would
   not write back as it was — an author in a form git trims, a message in
   another encoding than UTF-8 — is refused `rejected_object` rather than
   rewritten.
4. **Lands one forced push** of the source branch from the head its caller
   read to the last replayed commit, through the [one landing](#how-a-write-is-built):
   the branch's protections and the caller's right to force-push it hold the
   rebase as they hold a `git push --force`, and the push's apply records the
   change's new revision.

The commits are read sixteen at a time, so what a rebase holds of their
messages is bounded whatever its length. A rebase's build is given **forty
seconds** of git (BenchmarkEngineRebase measures it), and a router gives it
**112 seconds** on one git node — an engine write's 82 with its build term
replaced by the rebase's.

### The bounds

| Bound | Value | Why |
|---|---|---|
| A write's operations or suggestions and message | 1 MiB, as JSON spells them | An engine write is a document or a configuration fix, never a codebase: a larger change belongs in a coding box, whose push has no such bound. Measured as it crosses between nodes, because that is what every node holding the write holds. |
| Operations in one `commit_files` | 1 000 | Past every document or configuration change, and — with a path's depth — what bounds the directories one build rewrites: every directory above a path the write names, at most 2 × 1 000 × 63 and the root, since a move names two paths. |
| A write's pack | 4 MiB | Four payloads' worth: a commit's new files compressed and the directories it touches, each written whole — about 24 bytes an entry, measured, so 2.3 MB for a file put into a directory of 100 000, and a directory of some 175 000 entries the widest one write can touch. |
| A write's git | 10 s | Every process of a build together, one read's deadline. |
| A path | 4 096 bytes | Linux's `PATH_MAX`: a path no checkout can create is a tree no client can use. |
| A path's depth | 64 components | An engine write is a document or a configuration fix, whose paths run from a few components to a couple of dozen even in a deep tree; a deeper change belongs in a coding box. It bounds every cost of a write that grows with depth, which 4 096 bytes alone leave at 2 048 components: the directories its build rewrites, its one `mktree` a level, and its read of what each path it names and each directory above one is, which git walks from the root at a cost growing with the square of the depth — a thousand paths at the bound took 19 s of git, which the build's ten seconds refuse `too_large`. Only the paths a write names are held to it: a repository's own deeper paths are never judged. |
| A conflict's start | 2 KiB | Enough to see what collided; each stage is readable whole by its id. |

Every write a git node builds or answers is counted
(`crewlet.code.host.writes`, by `kind` and `outcome`) and timed
(`crewlet.code.host.write.duration`, by `kind` and `stage`: `sync`, `build`,
`land` and `total`) ([Metrics](../reference/metrics.md)), and logged
`code_write` ([What the node logs](#what-the-node-logs)).

### What an engine write costs

Measured from the call of a code write tool in a seat's turn to the tool's
answer — which follows the push's applied row and its pack adopted into the
copy — on one git node whose broker writes every record to disk, in two runs
of ten writes an arm, the range given. The figures are from **in-process
object stores** — the in-memory one and an S3 fake answering every request
100 or 200 ms late — and no real bucket, on two E-cores of an Intel Core
Ultra 7 265U shared with other work, Go 1.27.2 and git 2.56.0. Two cores give
the node a read lane of two slots, so the seats' [share](#server-side-reads)
of it is one unit.

| One write at a time, p50 | In memory | 100 ms first byte | 200 ms first byte |
|---|---|---|---|
| `commit_files` of one file | 44–78 ms | 141–197 ms | 239–251 ms |
| `commit_files` of ten files | 40–79 ms | 139–198 ms | 239–249 ms |
| `revert` | 35–68 ms | 136–196 ms | 234–270 ms |
| `create_branch` | 19–43 ms | 18–53 ms | 30–62 ms |

- **Where the time goes.** A one-file commit from memory is 3–5 ms of the
  tool's own (its arguments and the repository's name), 7–12 ms bringing the
  copy to the caller's floor, 11–28 ms of build, 9–17 ms for the proposal
  and its row and about a millisecond adopting the pack. Against a bucket,
  the one added wait is the bundle's single upload, a first byte's worth.
- **One upload, no downloads.** Every write that stores a pack makes one PUT,
  and no write reads anything back from the object store: the node adopts the
  pack it built. A branch created stores nothing.
- **Many at once.** Eight writes at once take 0.15–0.5 s at the median;
  sixty-four at once fill the node's write lane, every one in flight and
  none queued, and take 1.2–1.8 s at the median and up to 3.7 s at the p99.
  With a share of one unit, 50–62% of sixty-four writes into one repository
  waited past the share's two seconds and were answered unready — the
  tool's signal to ask again — and up to 39% of sixty-four spread over as
  many repositories. A node with more cores has a wider share.
- **Far inside the attempt.** The slowest p99 at a 200 ms first byte was
  2.9 s, a twenty-eighth of the [82 seconds](#which-git-node-builds-a-write)
  a write may take on one git node, and every stage stayed inside its own
  budget, so none of them was changed.

The build alone follows the change, never the tree: in a tree of 10 000 files
and of 100 000, git is asked the same requests, and a commit six directories
deep builds in 56–89 ms for one file, 33–84 ms for ten, 59–145 ms for a
hundred and 0.24–0.52 s for the 1 000 operations a write may carry — a
fifteenth of the build's ten seconds at the p99 — in a pack of 4 KB, 9 KB
and 58 KB. The one build that grows with what the repository holds is a
file put into a wide directory, which is written whole: 74–78 ms and 240 KB
into a directory of 10 000 entries, 0.6–0.85 s and 2.3 MB into one of
100 000.

**Beside a person's pushes.** A person pushing one commit to the same
repository every 100 ms took 0.1–0.3 s at the p99 alone and 0.95–1.4 s
beside sixty-four writes at once. Most of that tail stayed once the writes
had stopped (1.2–1.5 s against the S3 fake): it is the hundreds of packs a
burst like this leaves in one repository until its next
[compaction](#compaction), not the writes running beside the push. What
remained from memory is CPU: the share keeps the person's scans a read slot,
and on two cores the writes' landings still take the processor, which no
slot bounds. A git node expected to take bursts of engine writes beside
people's pushes wants more than two cores.

---

## Fair share

Every clone, fetch and push runs its git in one of the node's **transfer
slots** — four a core, and no more than its memory backs — and a slot is held
for as long as the client takes, so one credential can hold every slot: a CI
system checking out behind one token, a mirror catching up, a blobless
clone's lazy fetches one blob at a time. In arrival order, a person's clone
behind sixty-four of them would wait for sixty-four of them. So when every
slot is held, the node hands the next one released by **share**:

| Who asked | Class | Weight | One principal is |
|---|---|---|---|
| A person, through any credential of theirs — their own machine token, a session — or a Tier A token or service account the identity directory binds to a human seat nobody else holds | `person` | 4 | the human seat, by its handle: a person's laptop, scripts and browser are one |
| A credential bound to no seat — a Tier A token (`token:<id>`) or a service account's machine token (`pat:<id>`) | `machine` | 2 | the credential: each of a service account's tokens is one, so a token per CI runner is a turn per runner |
| A [coding box](#coding-boxes), with its run's credential | `run` | 1 | the run's launch: each job is a turn of its own, and a relaunch starts a fresh one |

The released slot goes to the class furthest below its weighted share of what
the lane has spent lately — with persons and machines waiting, people take two
thirds, and with boxes waiting too, four sevenths to the machines' two and the
boxes' one — and
within that class to the principal that has spent least, the oldest request
first on a tie. A pipeline that pushes acts as the human seat the directory
binds it to — one nobody else holds — so it is a `person`, but one principal
among the people, and the one that spent most goes last: its burst still
waits behind every person who has asked less lately. A run is charged the
greater of the CPU its git spent and the time it held its slot, each second
of a slot counted as the share of a core that slot stands for: the node's
cores divided among its transfer slots — a quarter of a core at four slots a
core, a sixteenth where `git.max_transfers` gives sixteen a core — and never
more than a whole core, which is what a slot stands for wherever memory or
`git.max_transfers` leaves the node fewer slots than cores, since one
transfer's git runs on one core. So a slow clone holding a slot for an hour is
charged for the hour, a storm of short, CPU-heavy fetches for the CPU, and on
a node with fewer slots than cores — 32 cores whose 8 GiB back 16 slots —
where slots rather than cores are what runs out, two credentials holding the
same slots as long are charged alike however fast their clients read. What a
principal has spent fades over **a minute** (a burst still counts while it
lasts, and a twentieth of it is left three minutes on), so a person's clone
this morning does not slow their fetch now.

The share decides only **who goes next**. A slot is never left free while a
person or a machine credential waits, so either class alone — a CI system on
a quiet node — has every slot, and nobody waits longer than **ten seconds** for
one, whatever order it is granted in: a request still waiting then is answered
`503` with `Retry-After`, as it always was. **Coding boxes are the one
exception: together they hold at most half the lane**, even with the other
half free, and a box waiting at that bound is granted the first release that
leaves the boxes under it. The share orders only who is granted next, so a
class that holds every slot with transfers its clients pace keeps them as long
as its clients read — a burst of coding-run launches is a clone a repository
each, minutes apiece on a large one, and a box reading slowly holds its slot at
the paced floor of thirty seconds a mebibyte — and every person's push or
clone would wait out its ten seconds behind them and be refused. A box is
generated code, so the lane cannot count on its manners: half is the bound the
engine already holds seat work to on a git node's read lane, and the other
half is what persons and machines share. Persons and machines keep the whole
lane when no box waits. Inside that bound the order is not arrival
order. With both classes waiting, people get about two thirds of what the
lane spends and credentials acting as nobody about one third, so a person's
clone does wait behind CI's when people have spent more than twice what the
machine credentials have lately. And the credential that has spent most in
its class goes last: while others who have asked less keep arriving, it can
wait out the whole ten seconds and be answered `503` where arrival order
would have served it — the burst the share exists to hold back is its own.
The weights are the same on every node, so which node a balancer picks never
decides who waits.

`crewlet.code.git.share_wait` is how long transfers waited for their slot, by
class: `machine` climbing while `person` stays flat is the share doing its job
— CI waiting behind CI — `run` climbing alone is boxes waiting behind each
other at their half, and `person` and `machine` both climbing is a lane too
narrow for its traffic (more memory and `git.max_transfers`, or more git
nodes).
`crewlet.code.git.share` is what each class has spent lately, as the next
grant reads it ([Metrics](../reference/metrics.md)).

**Lazy fetches.** A blobless clone (`--filter=blob:none`) fetches each blob
it lacks when a command first needs it, and `git blame` needs every version
of a file the lines it explains passed through. Measured on a file of 200
lines rewritten by 1 000 commits, one line a commit
(`internal/code/host`'s `BenchmarkLazyBlame`, by a credential bound to nobody,
git 2.56.0 on both ends, one count of three blames on four E-cores of an
Intel Core Ultra 7 265U): one blame was 201 lazy fetches — a version of the
file each, each an advertisement and an upload-pack, so 402 runs of the
node's git — bringing 412.5 KiB, costing the node 1.1 s of CPU (about 5.5 ms
a fetch) and taking 7.2 s. Every one of those fetches is charged to the
credential that made it, so a CI job blaming its way through history waits
behind every machine credential that has asked less, and goes ahead of
people's clones only for the machines' third of a busy lane — once people
have spent more than twice what the machine credentials have lately; the
node keeps no limiter of lazy fetches beside the share. A seat is steered
off that path altogether: `code_blame` and `code_log` answer who changed a
line and how a file's history went on the node, one git process over the copy
it holds and no fetch at all ([Who changed a line](#who-changed-a-line),
[A path's history](#a-paths-history)).

**Measured** on four E-cores of an Intel Core Ultra 7 265U with the transfer
lane as wide as the cores (`internal/code/gitproc`'s `BenchmarkTransferShare`,
git 2.56.0, real upload-packs of an 18.2 MiB pack to a reader that never
waits, one count of twenty clones an arm, so the 99th percentile is the
slowest of the twenty): a person's clone alone took 56 ms at the median and
62 ms at the 99th percentile; beside sixty-four clones at once by one CI
token, 87 ms and 125 ms, of which the wait for a slot was 60 ms at the 99th
percentile — one release's turn, the rest the clone sharing the cores with
the clones beside it, which no order of granting changes. Granted in arrival
order instead, the same clone took 1.19 s at the median and 1.54 s at the
99th percentile, 1.46 s of it waiting behind the sixty-four. The CI token
alone ran 56 clones a second with the share and 55 without it. A person whose
own burst of sixty-four clones was a minute old waited 187 ms at the 99th
percentile behind the token's sixty-four, and three minutes old 67 ms, about
as someone who had spent nothing does — on that lane, as wide as its cores,
each second a slot is held is charged a whole core.

**With coding boxes beside them** (the same benchmark's `runs` arms, on two
E-cores of the same machine, so a lane of two slots, one count of five hundred
of a person's clones an arm): the CI token's sixty-four clones held the lane
for a minute, then sixty-four boxes arrived — each a run of its own, each
cloning as the coding recipe does, a blobless fetch and then its checkout's
files, again and again, half of them reading at the slowest pace a transfer is
waited on, thirty seconds a mebibyte — and once the boxes held what the lane
let them, the person cloned, pushing once a second beside. A person's clone
alone took 42 ms at the 99th percentile; with every class contending it took
83 ms, none of 500 clones or 36 pushes was refused, and the boxes never held
more than one slot of the two, three tenths of what the lane spent. With the
boxes' half lifted, the slow boxes came to hold both slots, and three of five
clones and two of three pushes were refused after their ten seconds: **the
half is what keeps people served**, and that is measured.

**The boxes' weight of 1 is not.** It is a judgment, as the person's 4 and the
machine's 2 are: the search it is to be decided by — the person's clone held
to twice its idle 99th percentile, trying 4 : 2 : 1, then 8 : 4 : 1, then
16 : 8 : 1 — cannot tell the candidates apart on a lane of two. There the
boxes' half is one slot and a person shares the other with the CI token, so
their clone waits behind at most one CI clone as long as their own, and twice
the idle figure is the lane's floor whatever the weights: one count of five
hundred clones read 4 : 2 : 1 at 1.97 times idle (83.0 against 42.2 ms) and
8 : 4 : 1 at 2.03 (85.8 ms), 16 : 8 : 1 at 97.4 ms, and an earlier count of a
hundred read 4 : 2 : 1 at 2.04 and 8 : 4 : 1 at 1.96, so which weight passes
changes from one count to the next. A node's lane is four slots a core,
where the weights decide who is granted a release and the width does not;
the search has not been run on one. The benchmark takes the lane's width for
these arms from `CREWLET_BENCH_SHARE_SLOTS`, so it can be run at such a width
on a machine with the cores to back it, and the boxes' weight is pinned from
that run.

---

## What git prints, and what it means

git shows a refusal at the advertisement as `fatal: remote error: …` and a
refusal of one ref as `! [remote rejected] <ref> (<code>: <sentence>)`. An HTTP
error's text — a 503's, a 404's — git prints after `remote:` when it comes
from the advertisement; when it comes from a request in the middle of a
transfer git prints only `error: RPC failed; HTTP <status>`, never the text,
which the node's log line for the request keeps. Every 503 carries `Retry-After`, and says the same in
words, since git does not read the header.

| What happened | What git prints | What to do |
|---|---|---|
| No credential, or a wrong one | git asks for one (a 401 challenge) | Give your token as the password. |
| Your token acts as a seat the org chart no longer holds as a human seat | `seat "<handle>" isn't in the org chart anymore`, or `seat "<handle>" is a "agent" seat now, and only a "human" seat can be held` (403) | Ask an admin to move you to a human seat; signing in again changes nothing. |
| A session that may only enrol a second factor (where `api.auth.totp` is `required`) | `This company requires two-step verification. Set it up (POST /auth/totp) before you do anything else.` (403) | Enrol an authenticator in the dashboard, then push or fetch with your own token. |
| The node cannot tell who your token is — it cannot read its identity estate or the org chart, or is behind on identity changes | `this node cannot tell who this credential is right now: retry in 2 s, or against another node` (503) | Retry, or against another node. |
| The company hosts no code | `this company does not host code (code.native is not set)` (404) | Turn the code host on ([`code.native`](../guides/code.md#turn-it-on)). |
| The node is applying the configuration the fleet activated | `this node is applying the configuration the fleet activated and answers once that apply ends: retry in 5 seconds, or against another node (applying)` (503) | Retry. |
| The node could not apply the configuration the fleet activated | `this node could not apply the configuration the fleet activated, which hosts code (GET /fleet and this node's log say why): ask another node, or activate a corrected configuration (config_apply_failed)` (503) | Ask another node. An operator reads why in `GET /fleet` and the node's log, and activates a corrected configuration. |
| The node could not read the configuration the fleet activated | `this node could not read the configuration the fleet activated, so it cannot say whether this company hosts code (GET /fleet and this node's log say why): ask another node (code_unknown)` (503) | Ask another node. An operator reads why in `GET /fleet` and the node's log. |
| The node's native runtime was running before `code.native` was set | `this node's native runtime was running before code.native was set, and it serves the code host once restarted: ask another node, or restart this one (native_backend_needs_restart)` (503) | Ask another node, or restart this one. |
| The node is draining for a shutdown | `this node is draining for a shutdown and begins no new fetch or push: retry in 30 s, against another node or once it has restarted` (503) | Retry; a balancer sends you to another node. A fetch the node had already begun is served to its end. |
| The repository is not declared | `repository "<name>" not found` (404) | Check the name in `code.repos`. |
| The repository was renamed ([Repositories made by tools](#repositories-made-by-tools)) | a clone or a fetch of the old name runs against the repository — git prints `warning: redirecting to <base>/git/<new>.git/` where it presents its credential up front, and nothing where a credential helper answers its challenge; a push of the old name is told `remote: "<old>" was renamed to "<new>" on <instant>; set your remote to /git/<new>.git at the same address (the old name serves clones and fetches until <instant>)` (404), and pushes nothing | Set your remote to the new name before the old one is released, a month after the rename. |
| The repository is deleted | `fatal: remote error: repository <name> is deleted; declaring it again in code.repos restores it …`, or for a repository a tool made, `…; a restore through the code tools brings it back …` | Declare it again — or, for one a tool made, restore it through the tools — before its purge, to restore it as it was. |
| The repository is being purged | `fatal: remote error: repository <name> is being purged, which cannot be undone; once the purge completes, declaring its name in code.repos creates a new, empty repository`, or `(repo_deleted: repository <name> was purged and holds nothing; …)` on a push the purge overtook | Nothing brings it back. Once its purge completes, declaring the name again creates a new, empty repository. |
| The repository is archived (a push) | `fatal: remote error: …archived…`, or `(repo_archived: …)` per ref | Fetch only; unarchive it in `code.repos` to push. |
| The repository is being imported from another host (a push) — `code.repos` `import`, a pull mirror's first fetch from its `mirror_from`, or a person's import | `fatal: remote error: repository <name> is being imported from another host, and takes no write until its import is done: …`, or `(repo_importing: …)` per ref | Fetch it meanwhile; push once its import is done. |
| The repository is a pull mirror (a push) — it names a `mirror_from` | `fatal: remote error: repository <name> is a pull mirror (code.repos mirror_from): it moves only by the code host's syncs from its source, …`, or `(repo_mirrored: …)` per ref | Push to its source, whose commits its next sync brings in; or remove its `mirror_from` — a person's to remove, never a seat's — to push here. |
| A token bound to nobody (a push) | `fatal: remote error: this token is not bound to a person: …`, or `(not_permitted: this token is not bound to a person: …)` on every ref of a push sent without its advertisement | Push with a token that acts as your human seat — your own machine token (`crewlet iam token -login`). |
| A command the pusher's grant does not cover ([who may read and push](#who-may-read-and-push)) | `(not_permitted: …)`, saying what the pusher holds, what the command needs and what would admit it | Ask for what the sentence names, or push where it says. |
| More than 1 000 refs in one push | `(this push names more than 1000 refs; push them in parts …)` | Push the refs in groups. |
| A command list past 1 MiB — the most refs one push takes, at the longest the engine takes (255 bytes), come to about 340 KiB, so it holds refs past that or lines that are no ref update | `(this push's command list is over 1 MiB)` | Push the refs in groups, each at most 255 bytes. Nothing of the push was run or stored. |
| Push options that take the push's command list past 1 MiB — far past what any change's options take ([A change a push opens](#a-change-a-push-opens)) | `(this push's options take its command list past 1 MiB; push with fewer or shorter options (git push -o))` | Push with fewer options. Nothing of the push was run or stored. |
| More ref updates than one push's record holds — some six hundred of the longest refs whose characters the code log spells long (`&`, `<`, `>`) | `(this push's ref updates take up to <size> of its record, past the 1023.5 KiB one push may; push them in parts …)` | Push the refs in groups. Nothing of the push was run or stored. |
| A push carries a push certificate — a signed push, from a client built by hand (`git push --signed` stops in git itself, since the engine advertises none) | `this push carries a push certificate (a signed push), which this engine never advertises and does not take: push without --signed` (400) | Push without `--signed`. Nothing of the push was run or stored. |
| A ref moved since you fetched | `(stale: refs/heads/main is at <id>; fetch first)`, or `(stale: was at <id>, which this repository does not hold; fetch first)` when the commit you pushed from was never pushed here | Fetch, integrate, push again. |
| A protection refused it | `(protected_force: refuses a force-push (protection <pattern>: allow_force))`, `(protected_delete: may not be deleted (protection <pattern>: allow_delete))`, or `(protected_direct: takes no direct push (protection <pattern>: direct_push none))`; for a seat's push to a ref a rule takes a person's push to alone, `(protected_direct: takes a person's push only (protection <pattern>: direct_push persons))` | The rule named says what it allows; a change to it is a change to `code.repos`. A seat's work reaches a ref under `direct_push: persons` — every rule that does not say `none` — when a person pushes it ([A seat's push](#a-seats-push)). |
| A tag moved, a reserved ref — outside `refs/heads/` and `refs/tags/`, under `refs/crewlet/`, or `refs/heads/seats` or `refs/heads/seats/<name>` as a branch | `(tag_moved: …)`, `(reserved_ref: …)` | Create a new tag; push under `refs/heads/` or `refs/tags/`, and not as `seats` or `seats/<name>`, which hold seats' own branches. |
| A ref the engine does not take — longer than 255 bytes, or one git's rules refuse | `(bad_ref: not a ref: git's check-ref-format rules, at most 255 bytes of UTF-8 under refs/<namespace>/)` | Name the ref within 255 bytes. git takes longer refs; the engine does not. |
| One ref of an `--atomic` push refused | the refused ref's reason, and `(atomic: …)` on every other | Fix the refused ref; nothing of the push landed. |
| A blob over the limit | `(blob_too_large: blob <id> (<path>) is <size>, past code.limits.max_blob_bytes …)` | Remove it from the history you push. |
| A push that would store more than one push may — the pack it sends completed against the repository, with its index | `(pack_too_large: the push stores <size>, past the <size> one push may store; push fewer commits at once, or without thin packs (git push --no-thin))` | Push fewer commits at once, or with `git push --no-thin`. Nothing was uploaded. |
| A [coding box](#coding-boxes)'s push whose pack holds a commit committed as the engine — `engine@<code.identity_domain>`, in any case — that the repository does not already hold, whether or not an update that lands reaches it | `(forged_committer: <id> is committed as the code host's engine (engine@<domain>), which only the host's own writes are: cherry-pick it, which rewrites the committer, or make the change with a code_* tool)`; or, for a pack holding more than 1 000 such commits, `(forged_committer: this push carries more than 1000 commits committed as the code host's engine (engine@<domain>), more than one push is checked for: push the history in parts, each landing before the next)` | Commit the change as the box: a cherry-pick, an amend or a rebase writes the box's own identity as the committer. Or make the change through a `code_*` tool, which the engine commits itself. Nothing of the push was stored: drop the commit from every ref the push sends, a refused one included. A history of the engine's commits the repository already holds — a branch deleted since the box checked it out — lands, up to 1 000 of them a push: push a longer one in parts. A person's push is never refused for its committers. |
| A [coding box](#coding-boxes)'s run — or a [check's box](#check-boxes)'s attempt — has sixteen requests in flight on the node | `this run has 16 requests in flight on this node; retry in 10 s` (503, `run_busy`), `this check has …` for a check's | Retry: the box's own requests end and make room. A box that meets it often sends requests faster than they are served — a loop, not a checkout ([a box's re-syncs and requests in flight](#a-boxs-re-syncs-and-requests-in-flight)). |
| A coding box's fetch wants an object its run's floor does not hold, its run's re-syncs at the log's head spent | `fatal: remote error: <oid> is not in <repo> at the position this run has been shown; it has not been pushed yet, or ask code_resolve (not re-judged at linearizable: this run's re-syncs are spent until <instant>)`, `this seat's` where the seat's are | Fetch what was pushed: resolve the branch again (`code_resolve`) and fetch its tip. After the instant named the box's next fetch looks at the log's head again. |
| A check's box's fetch wants an object its check's session does not hold, its re-syncs spent | `fatal: remote error: <oid> is not in <repo> at the position this check was queued at; a check's box fetches the one commit it checks (not re-judged at linearizable: this check's re-syncs are spent until <instant>)`, `its repository's checks'` where that bucket's are | Fetch the commit the check checks, which its session holds; anything else its file asks for is not the check's to read. |
| A new ref past the most live refs a repository holds | `(repo_refs: <repo> holds <n> live refs, the most one repository may)`; for a seat outside the owning unit, past the half it may fill ([A seat's push](#a-seats-push)), `(repo_refs: <repo> holds <n> live refs, and a seat outside unit <owner>, which owns it, creates refs there only while <repo> holds fewer than 50000: half of the 100000 one repository may, the other half kept for its owners)` — or `a seat other than <owner>, which owns it,` for a repository a seat owns | Delete the refs nobody needs, in the same push as the new ones or before it: a deletion stops counting at once, even against the push that makes it, and the refs a repository deleted never refuse a push. A seat outside the owning unit deletes its own branches it no longer needs; the other half of the refs is the owners'. |
| A new ref beside a live ref of the directory it would make — `feature/x` beside the branch `feature`, or `feature` beside `feature/x` ([a ref and a directory of refs](#a-ref-and-a-directory-of-refs)) | `(ref_conflict: refs/heads/feature is in the way: a ref is never also a directory of refs, so a clone holding one could not fetch the other; delete it first, or choose another name)` | Delete the ref in the way in the same push (`git push origin :feature HEAD:feature/x`), or choose another name. A clone holding the old ref fetches the change with `git fetch --prune`. |
| A pack that would take the repository past its size | `(repo_size: <repo> would hold <size> of packs, past <limit> (code.limits.repo_max_bytes))`; for a seat outside the owning unit, past the half it may fill ([A seat's push](#a-seats-push)), `(repo_size: <repo> would hold <size> of packs, past <half>: a seat outside unit <owner>, which owns it, fills at most half of code.limits.repo_max_bytes (<limit>), the other half kept for its owners)` | Raise `code.limits.repo_max_bytes`, or push less. What a repository holds is its packs as they were pushed ([Limits](#limits)). Nothing landed. A seat outside the owning unit is held to half of the limit whatever it is raised to: the other half is the owners'. |
| The push's pack was uploaded too long before its commit | `(upload_expired: the pack was uploaded at <instant>, 12h0m0s or more before this decision; run git push again)` | Run the push again; nothing landed. |
| The push is pending | `(pending: durable at <position>, unresolved here; re-run git push …)` | Run the push again. |
| The outcome is unknown | `(unknown: outcome unknown (op …); fetch, then re-run git push)` | Fetch, then push again. |
| The push was recorded and every node dropped it | `(not_applied: the push was durable at <position> and applied nowhere; re-run git push)` | Run the push again; nothing landed ([what a refusal names](#a-push-end-to-end)). |
| The repository is taking pushes faster than its committer commits them | `(busy: <n> pushes are queued for this repository on this node, the most it holds)`, or the same of the bytes queued for it or held across the node's repositories; `(busy: the push's asker stops waiting in <t>, and a batch started now may take 30s and its answer 1s more; run git push again)`, or `(busy: the push waited <t> behind this repository's earlier pushes on this node, …)`; or `(busy: the repository kept changing for every round the write may take; run git push again)` | Run the push again; nothing of it landed. One repository's pushes meet at one node, which commits them a batch at a time ([The committer](#the-committer)). A full queue, a node at its whole bound, or a push that waited in the queue until its asker could wait no longer raises `code_commit_busy` on that node for fifteen minutes, naming the repository with the most refusals; a push that reached the committer with too little of its asker's attempt left raises nothing. |
| A push carrying objects into a repository holding the most live packs one may until a compaction merges them — 2 048 ([Limits](#limits)) | `(compaction_behind: <repo> holds <n> live packs, and a push adds none past 2048 until compaction catches up: nothing was decided; run git push again in a minute)` | Nothing of the push was decided, and no row names anything it uploaded; it lands once the repository's home git node has merged its packs, which the home tries every minute ([Compaction](#compaction)) — run the push again a minute later. A push carrying none — deletions, refs moved onto commits the repository already holds — still lands. Refused for longer, the home's `code_compaction_behind` alarm stands and its log says what keeps the merge from landing. |
| A seat's push past the pushes one seat may land in a day on the company's calendar, every repository together — `code.limits.pushes_per_seat_day`, 1 000 unless set ([A seat's push](#a-seats-push)) | `(push_quota: seat <handle> has landed <n> pushes since <day> began on the company's calendar, and code.limits.pushes_per_seat_day lets a seat land <limit> pushes a day: nothing was decided; the count starts again at <the company's next midnight>)` | Nothing of the push was decided and no record carries it; run it again once the instant named has passed. A seat that meets it is pushing in a loop: look at what its turns are doing before raising the bound. A pack the push stored is named by no row, and the [object collector](object-store.md#collection-and-audit) deletes it a day later. A person's pushes are never counted. |
| A push moving the branches of more open changes than one record can carry beside its own refs — hundreds of change branches force-pushed at once ([Revisions](#revisions)) — or a person's push landing on hundreds of protected branches, each landing a row and its green row ([The merge queue](#the-merge-queue)) | `(too_many_changes: this push moves <n> live changes[ and lands on <m> protected branches], and their rows beside its own would take its record to <rows> of the 4000 rows one record may write: nothing was decided; push their branches in fewer pushes)` | Push the branches in groups: each group's revisions are written in its own record. Nothing of the push was decided. A push whose changes fit a record of its own, but not beside the pushes batched before it, is never refused: the committer decides it first in its next record. |
| A push its node checked against the repository just before an expunge of the repository's unreachable objects began — which nothing in this build begins yet ([what it does not do yet](#what-it-does-not-do-yet)) | `(expunging: an expunge of <repo>'s unreachable objects began after this push was checked against it (drop epoch <n>, now <m>): nothing was decided; run git push again)` | Run the push again: nothing of it was decided, and its node checks it again against the repository as the expunge left it. A pack it uploaded is named by no row, and the [object collector](object-store.md#collection-and-audit) deletes it a day later. |
| Nothing took the push | `(unavailable: no data node took push <id>: …)` | Nothing landed; push again, or once the fleet has a data node serving. |
| The object store has no room — the fleet's NATS bucket holds as many bytes as it [reserves](object-store.md#its-byte-reservation), or an S3-compatible bucket's provider [refuses the write for room](object-store.md#an-s3-bucket--s3) | `(store_full: the object store has no room for the <size> this push stores, and nothing was stored; it lands only once <room>)`, the room the store names: on NATS, `an operator makes room — moves the files to a bucket (crewlet objects migrate -to s3), raises the bucket's byte reservation on an external broker, or lets the collector remove what nothing names`; on S3, `an operator makes room in the bucket — frees space on the storage its provider keeps it on, or raises the quota the provider holds it to — or lets the collector remove what nothing names` | Nothing was stored, and pushing again is refused the same until room is made: tell an operator. The node logs `code_push_store_full` at `WARN` with the store's own answer; on NATS, the [`objects_bucket_near_ceiling`](object-store.md#alarms) alarm says how full the bucket is. |
| The object store failed | `(unavailable: object store unavailable: …)` | Nothing was stored; push again. |
| The engine stopped mid-push | `crewlet: the engine went away during this push; its outcome is unknown …` | Fetch, then push again. |
| The engine could not finish checking the push | `crewlet: the engine could not check this push: <why>` | Nothing landed; push again, or against another node. The node logs `code_push_unchecked`. |
| Every git slot on the node is busy | `every git slot on this node stayed busy for 10 s: retry in 10 s` (503) | Retry, or against another node. |
| The node ended git before it answered | `git on this node was stopped before it answered, stalled (…): retry in 10 s, or against another node` (503), or `past its deadline` | The node is too loaded to answer in time; retry, or against another node. |
| The node cannot read the code log — no quorum for a read at the log's head (on a log one member stores, that member's write stalled), a copy out of service, no data node answering | `the code log could not be read here: …; retry in 10 s` (503) | The node is cut off from the fleet, or a single member's broker is not completing its writes; retry, against another node where there is one. Only the code log's own failure says this. |
| The object store did not hand over a pack (a fetch) | `<repo>'s packs could not be read from the object store here (…): retry in 10 s, or against another node` (503) | Retry: a timeout or a throttle passes. The node logs `code_object_store_unavailable`; a store that stays unreachable is the operator's ([the object store](object-store.md)). |
| The node itself failed — its disk refusing a write into its copy of the repository, or anything else the engine did not name | `this node could not serve <repo> (…): retry in 10 s, or against another node` (503) | Retry against another node. The sentence says what failed; the node logs `code_push_failed` or `code_fetch_failed` with `reason` `node`. A full disk is the operator's to free. |
| The node is stopping | `this node's code host is stopping: retry in 30 s …` (503) | Retry against another node. |
| A pack the rows name is gone, or damaged in the object store | `<repo>'s pack <id> is missing from the object store (the daily audit raises objects_missing for it); retry in 10 s`, or `<repo>'s pack <id> is damaged in the object store (…)` (503) | An operator's problem: see [`objects_missing`](object-store.md#alarms). |
| A pack's bytes are not the bundle its row describes | `<repo>'s pack <id> in the object store is not the bundle its row describes (code_pack_checksum_mismatch): retrying does not end it — an operator must act on this node's code_pack_checksum_mismatch line, and until then only the node that received the push, which holds its own copy, can serve it` (500, no `Retry-After`) | Do not retry: every retry downloads the whole bundle again and fails the same way. The store handed back exactly the bytes the push stored, and they are not what the push's row says, so the row or what was uploaded is wrong. It is an operator's problem: the node logs `code_pack_checksum_mismatch` at `ERROR`. |
| The rows cannot be read into a cache | `<repo>'s rows cannot be read into a cache on this node (code_bad_snapshot): <why>; retrying does not end it — an operator must act on this node's code_bad_snapshot line, and a node running a newer build may serve it` (500, no `Retry-After`) | Do not retry this node: it reads the same rows again. The rows hold something this build cannot install (a ref, an id or a pack's sizes) or a push cannot be judged against (a protection rule's pattern), and the sentence names the row; a node of a newer build may install it. It is an operator's problem: the node logs `code_bad_snapshot` at `ERROR`. |
| A damaged copy is still damaged after its last repair — a pack or an index git could not read, an object no pack holds, or a copy the node will not run git in because it is not the shape the engine writes | `<repo>'s cache on this node is still damaged after its last repair (<why>): retry in 10 s, or against another node` (503) | Retry against another node. On this node, an operator checks the copy ([`POST /code/repos/{name}/verify`](../reference/api-endpoints.md#post-codereposnameverify)), which says what the repair could not end. A copy damaged once is repaired under the request that finds it and never shows this. It shows when git meets the same damage on the copy just repaired, and while the node holds back another rebuild ([damage and repair](#the-cache-on-a-git-node)): for a damaged pack or a missing object, one that did not end the damage is not repeated for up to a day unless something writes into the copy, and for a shape that reading the copy whole in place did not end, the wait starts at ten minutes and doubles with every rebuild in a row, up to a day. |
| The node refused its own call serving the request — the engine's own call is wrong | `this node refused its own call serving this request (<the refusal>): the engine's call is wrong, and retrying does not end it — an operator must act on this node's code_git_refused line` (500, no `Retry-After`) | Do not retry: the request changes nothing about it. It is a bug in the build the node runs — a git configuration key or variable the engine never allows, a call without a deadline, output captured that the call should have streamed, the cache handed an argument it never takes, a read asked at a level no reader serves — or a node whose git should never have started (git's HOME not empty, a git below the floor). The node logs `code_git_refused` at `ERROR`; report it. |
| git failed on the node, for a reason the engine does not classify | `git <command> failed on this node (<kind>): <git's last line>` (500) | git's last line says why. A hand-built push of a line git's protocol does not have reads `protocol error` there. The node logs `code_push_failed` or `code_fetch_failed`. |
| git's answer could not be read | `git's answer on this node could not be read (…); retrying does not end it` (500, no `Retry-After`) | git exited as if it had answered and its output stopped mid-line, or it wrote a line longer than the engine reads. Report it with the node's `code_push_failed` or `code_fetch_failed` line. |
| A partial clone asks for `--filter=sparse:oid=…` | `fatal: remote error: filter 'sparse:oid' not supported` | Clone with `--filter=blob:none` and a sparse checkout: the files are fetched as the checkout needs them ([the filters served](#a-push-end-to-end)). |
| A partial clone asks for `--filter=tree:<n>` with `<n>` past 0 | `fatal: remote error: tree filter allows max depth 0, but got <n>` | Clone with `--filter=tree:0` or `--filter=blob:none`. |
| A fetch request past its repository's bound | `this fetch request is larger than any repository <name> holds could need …` (413) | Fetch in parts. |
| A fetch request past what the node spools for one request (64 MiB, about 1.3 million objects) | `this fetch request is larger than this node spools for one request …` (413) | Fetch fewer objects at once: a sparse checkout asks only for the files it checks out. |
| A fetch large enough to spool waited 55 s for its turn at the node's room for large requests, or had to wait and found the node's waiting room full, or its credential's quarter of it | `this node's room for large fetch requests is in use: …` (503), saying what held it back — mid-transfer, so git prints `error: RPC failed; HTTP 503` and the node's `code_fetch_spool_unserved` line keeps the sentence | Retry, or against another node. When the sentence says this credential's own requests held it back, the requests are several CI runners behind one token: give each runner a token of its own. |
| The request's body could not be read (a gzip stream that fails its checksum; for a fetch, a body cut short too) | `the push's body could not be read: …`, or `the push's command list could not be read: …` when it failed before the pack or was not pkt-lines at all, or `the request body could not be read: …` for a fetch (400) | Nothing was stored; send it again, and if it fails the same way the client is sending a broken stream. |

**What to watch.** `crewlet.code.push.duration` by `outcome` (`pending`
growing is a code log slow to apply here; `unknown`, a committer that stopped
answering), `crewlet.code.push.bytes` against the push limit,
`crewlet.code.fetch.duration` by `command` and `sync`, and
`crewlet.code.host.spool.waits` by `outcome` — `granted` climbing is a token
behind several CI runners, or a node whose spooling is full, before any
checkout fails; `expired` and `refused` are checkouts that did, and their
`code_fetch_spool_unserved` lines say which held them back ([metrics
reference](../reference/metrics.md)).

### When a push asked for a change

A push that asked for a change (`git push -o change`, [A change a push
opens](#a-change-a-push-opens)) is told in one line what became of it, beside
the line of each ref. None of these is a refusal of the push, which lands or
not by its refs alone. Where the push landed and its change did not open, a
push of the same commit again moves nothing and asks for nothing: fix what the
line says and push the branch's next commit with `-o change`.

| What happened | What git prints | What to do |
|---|---|---|
| The push opened a change | `remote: crewlet: change <repo>!<n> opened` | Nothing: the change proposes the branch into its target, and the branch's next push is its next revision. |
| The branch already headed a live change, which the push revised | `remote: crewlet: change <repo>!<n> updated` | Nothing: the push is the change's next revision. The options shaped nothing — they shape a change a push opens. |
| The options are no change's — a key the `change` options do not include, which the line names, a value its option refuses, a key given twice — or the push carries so many refs beside the branch that its record cannot hold the change too | `remote: crewlet: change not opened: bad_option: …` | Fix the option the line names; push the change's branch alone. |
| The push set no branch — it pushed only tags or deletions — or its update of the branch was refused or found the branch already there, which the line says | `remote: crewlet: change not opened: no_source: …` | Push a commit to the branch with `-o change`; for a refused update, its own line says why. |
| The push set several branches, and a change proposes one | `remote: crewlet: change not opened: ambiguous_source: …` | Push the change's branch alone with `-o change`. |
| The branch pushed is the change's target: the repository's default branch, unless `change.target` named another | `remote: crewlet: change not opened: same_ref: …` | Push a branch of its own, or name another target with `-o change.target=<branch>`. |
| The target is no live branch of the repository | `remote: crewlet: change not opened: no_target: …` | Name a branch the repository holds with `-o change.target=<branch>`. |
| The branch is at its target's commit, so it holds nothing to merge | `remote: crewlet: change not opened: no_difference: …` | Push a commit to the branch. |
| The push was refused or failed, which its refs' lines say | `remote: crewlet: change not opened: nothing of this push landed` | Fix what the refs' lines say and push again, with `-o change`. |
| The push is pending, or its outcome unknown, here | `remote: crewlet: change not known yet: this push has not applied here, and the change it asked for lands with it or not at all` | Fetch: where the branch moved, the push landed, and its change with it. |
| The push had landed before this answer, which a node gave again after the first was lost | `remote: crewlet: change not known: this push landed, and the answer it was given does not say what became of its change` | Nothing for the push. A change it opened is open on the branch, and the branch's next push with `-o change` names it. |

---

## What the node logs

| Line | Level | Means |
|---|---|---|
| `code_repos_applied` | `INFO` | A reconcile wrote something: how many repositories it `created`, `changed`, `restored` and `deleted` (or `written`, where the listing before it could not be read), each with the first 32 of their names in `created_names`, `changed_names` and so on. Nothing is logged when nothing was written. |
| `code_repo_awaits_purge` | `INFO` | A declared name belongs to a repository still being purged; the `code-retention` duty creates it when the purge completes. |
| `code_repos_superseded`, `code_repo_superseded` | `INFO` | A later activation owns the repositories, or this one: this node is applying an older configuration, or has since been asked to reconcile a later one. |
| `code_repos_interrupted` | `INFO` | The node stopped before its reconcile finished; the next boot reconciles. |
| `code_repo_not_applied`, `code_repos_not_applied`, `code_repos_not_listed` | `ERROR`, `ERROR`, `WARN` | A reconcile could not apply one repository, any, or delete what the document no longer declares; the node runs the reconcile again (`code_repos_retrying`). Two of a repository's are not run again for, and say what to change instead: its name is held by a repository the configuration does not manage — rename the declaration — or the code log refuses its declaration as written. |
| `code_repos_retrying` | `INFO` | The reconcile of `activation` left something undone — said in the lines before — and runs again in `retry_in`: five seconds after the first such run, doubling with each run in a row (`attempt`) to five minutes, until one applies or a newer activation replaces it. |
| `code_retention_pass` | `INFO` | A pass that swept retired packs or an idle repository's checks, purged, failed a repository, held a purged name, released a renamed repository's old name, expired a scratch repository or kept one a live turn holds, expired, kept or held a seat's idle branch or failed to, could not read the live turns' holds, or stopped at its budget (`sweeps`, `swept_rows`, `checks_sweeps`, `checks_swept_rows`, `purged`, `steps`, `remain`, `failed`, `unhanded`, `released`, `expired`, `scratch_held`, `scratch_expiry_unread`, `refs_expired`, `refs_kept`, `refs_held`, `expiry_failed`, `expiry_unread`); `DEBUG` when nothing was due. |
| `code_refs_expired` | `INFO` | One push of [the ref lifecycle](#the-ref-lifecycle) applied on a repository (`repo`, `push_id`, `position`): how many idle seat branches it deleted (`expired`) and how many its committer kept (`kept`). |
| `code_ref_expiry_skipped` | `WARN` | The live turns' holds on a page of idle seat branches could not be read (`error`, naming which; `repo` and `first`, the page's first branch), so this pass deleted none; the next pass asks again ([The ref lifecycle](#the-ref-lifecycle)). |
| `code_refs_near_limit_unread` | `WARN` | The trim's quarter-hour tick could not read which repositories are near the most live refs one may hold (`error`), so this node raises no [`code_refs_near_limit`](#the-ref-lifecycle) until the next tick reads the rows again; a create past the bound is still refused `repo_refs`. |
| `code_retention_pass_failed` | `WARN` | A pass ended on an error; the next pass lists what is due again. |
| `code_scratch_expired` | `INFO` | A scratch repository idle past `code.scratch.idle_days` (`idle_since`) and held by no live turn was deleted, restorable for `code.deleted_retention_days`. |
| `code_scratch_expiry_skipped` | `WARN` | The live turns' holds on a page of idle scratch repositories could not be read — the run store, the seat leases, or a seat holder that did not answer within two seconds (`error`, naming which) — so this pass expired none; the next pass asks again ([A scratch repository's idle expiry](#a-scratch-repositorys-idle-expiry)). |
| `code_retention_duty_declined`, `code_retention_duty_resumed` | `WARN` or `DEBUG`, `INFO` or `DEBUG` | This node does not take the duty, and gives it back if it holds it (`code_retention_duty_given_back`). It logs a `_declined` line each time the reason it declines for changes, at that reason's level — so a decline that keeps its reason is one line however many ticks it lasts — and one `_resumed` line when it stops declining, `INFO` if any reason of that decline warned and `DEBUG` otherwise. `WARN` where the reason is a fault: its copy of the code log is out of service (`domain`), or it does not serve the revision the fleet is on (`reason`: `behind`, with this node's `applied_epoch` and the `fleet_epoch`, once the lag is confirmed — its apply of the revision failed, or it has been behind for longer than the 45 seconds of propagation; or `unknown`, with the `error` that kept it from telling, as when the activation pointer could not be read). `DEBUG`, and asked again a reconcile interval (15 seconds) later rather than a tick, while it is no fault: `reason` `propagating` (with the `fleet_epoch`, and the `applied_epoch` once its reconciler has read the pointer) on a node still applying the fleet's revision or still starting — every restart's first turn runs before its reconciler is built — and `not_activated` while the fleet has activated no revision yet, which a fresh deployment meets for the seconds before its first activation. A node that fails one activation and then applies the fleet's next logs four lines: `propagating` at `DEBUG` while it applies the first, `behind` at `WARN` once its failure confirms the lag, `propagating` at `DEBUG` when the next activation arrives, and `_resumed` at `INFO` once it serves that one. |
| `code_push` | `INFO` | One push this node received, whatever became of it: the repository, the push's id, the credential it came through (`operator_id`) and the human seat its principal holds (`seat`, empty for a credential bound to none) — for a [coding box](#coding-boxes)'s, no credential, the seat whose run it is, and the run's `turn_id` and `launch_id` — the `outcome` (`applied`, `pending`, `unknown`, `refused`, `failed`) and any `refusal`, how many commands and how many went through, the bundle's bytes, the position its record landed at and how long it took. |
| `code_box_challenged` | `DEBUG` | A [coding box](#coding-boxes)'s request presented no credential this fleet signed — none (`reason`: `none`), a user name other than `run` (`user`), or a token that does not verify (`token`) — and was challenged `401`, with its `path`: by the code host's admission, or, under the component `api`, by a node serving none of the code host now, which tells a box why only past this challenge. Debug, because the route is public and anybody who reaches it can produce one. |
| `code_box_refused`, `code_box_unavailable` | `INFO`, `WARN` | A coding box's signed token was refused `403` (`reason`: `run_unknown`, `run_superseded`, `run_ended`, `not_granted`, `read_only`, `seat_stopped`, `not_permitted`, `holds_full`), or answered `503` because this node could not judge it or hold what its push names (`run_unread`, `pauses_unread`, `no_chart`, `behind_config`, `floor_unraised`, `holds_unwritten`) or its run had sixteen requests in flight here (`run_busy`, before the run's row is read, so with no `seat`): with the run's `turn_id` and `launch_id`, the `seat` and the `repo`, never the token. |
| `code_box_resyncs_spent` | `INFO` | A coding box's fetch wanted an object its run's floor does not hold, and its run's — or its seat's — re-syncs at the log's head on this node were spent, so it was answered at its floor with no barrier: the `repo`, the bucket's `scope` (the seat, or `check:<repository id>` for a check's box) and `principal` (`run <launch id>`, or the check's attempt: its commit, target, name and number), the `oid` and the bucket's `retry_after` ([a box's re-syncs](#a-boxs-re-syncs-and-requests-in-flight)). |
| `code_box_floor_unraised` | `WARN` | A coding box's push applied, and its run's floor could not be raised to the push's row in the run store: the push was answered `ok` all the same, and what the box may read behind is its own push, whose objects it holds. Or a box's fetch took a barrier for a want its floor lacked, and its floor could not be raised to it: the fetch was served all the same, and the box's next fetch of those objects on another node may take a barrier again. |
| `code_box_floor_unreadable` | `WARN` | Under the component `engine`: a coding run's row names a code-log floor this build cannot read, or one on another log, so its box reads through the barrier until its next answer raises the row to a readable one. |
| `code_write` | `INFO` | One [engine-built commit](#engine-built-commits) this node was asked for, whatever became of it: the repository, the write's `kind`, who wrote it (`principal_kind`; the `seat` it is the commit's author for — a person's human seat, or the agent's own — with the credential a person came through, `operator_id`, or the turn behind a seat's, `turn_id`), the `push_id` it lands under, the `outcome` (`applied`, `pending`, `unknown` from its push's row; `refused`; `unready`, this node could not answer and another git node is asked; `failed`), any `refusal` of the change or of its push, and how long it spent in `sync_ms`, `build_ms`, `land_ms` and `total_ms`. No `operator_acted` is recorded for it: a person's tool call is one already, and a seat's write is its push row. |
| `code_write_outcome_unread` | `WARN` | An engine write found its branch at another head and could not learn, from the code log, whether that head is its own push landed by an earlier ask; it was answered `busy` — make the same call again — rather than `stale`. |
| `code_commit_failed` | `WARN` | A repository's [committer](#the-committer) could not answer a record it published: the code log failed the write for a reason that is no refusal of it, or the record applied and the rows it wrote could not be read back. By `kind`: `push`, a batch of pushes, each answered with the `error`; or `compaction`, a git node's merge of the repository's packs, which the node that built it plans again. With the `repo` and the record's `op_id`. |
| `code_commit_busy` | `WARN` | Proposals to a `repo` (its id) that this node's [committer](#the-committer) refused `busy` in one `minute`, written once the minute has closed — at the committer's next refusal or the node's next alarm evaluation, ten seconds at most: how many were `refused`, and how many of them at the repository's own `queue`, at the `node`'s whole bound, after they `waited` in the queue until their asker could wait no longer, too `late` for their asker when they arrived, and after every compare-and-set round (`contended`). One line a repository a minute, never one a refusal. The first three raise the [`code_commit_busy`](../reference/alarms.md) alarm; the last two raise nothing. |
| `code_fetch_spool_unserved` | `WARN` | A fetch large enough to spool — a partial clone's lazy fetch, which git does not retry — was answered 503 without being served: its wait for its turn at the node's room for large requests reached 55 s (`reason`: `expired`), or it had to wait and found the node's waiting room full, or its credential's quarter of it (`reason`: `refused`). The `operator_id` is the token that sent it, `spool` the credential it waited as — `runs` for every coding box, which share one — and the `error` says what held it back; its credential's own requests holding it back means several CI runners share the token, or, for `runs`, that boxes held their share. git itself prints only `error: RPC failed; HTTP 503`, so this line is where the reason is. |
| `code_fetch_retried`, `code_push_retried` | `INFO` | git found this node's cache damaged during a fetch's or a push's request — a fetch's in an advertisement, in the node's check of which of the fetch's wants the copy lacks, or in the fetch's own git, before its pack began — read from the formats of git's own reports, never their words, which repeat the request's (`error` quotes git's words) — or the node would not run git in it for its shape (`reason`: `corrupt`, `derived`, `missing`, `shape` — a push's only ever `shape`, since its git runs beside the cache rather than in it); the cache was repaired — a shape or a damaged derived file (`derived`: a multi-pack-index, a commit-graph) in place, damage to its packs or objects in place first, one download a damaged pack, and by a rebuild only when that found nothing — or, for a request whose git began before the node last wrote a repair into the copy, left as it stands; and git run again over the same request. |
| `code_git_refused` | `ERROR` | The node refused its own call serving a request because the call is wrong — a git configuration key or variable outside what the node allows, a call without a deadline, a malformed command, output captured that the call should have streamed, the cache handed an argument it never takes, a read asked at a level no reader serves — or because its git should never have started; answered 500 with no `Retry-After`. A bug in the build the node runs: report the line. |
| `code_fetch_failed`, `code_push_failed` | `WARN` | git failed serving a request on this node — `code_push_failed` a push's (its advertisement, its receive-pack, a process its sync ran), `code_fetch_failed` a fetch's — by itself or ended by the node (`reason`: `deadline`, `stall`, or its `input` or `output` failing here, beside the pool's own `code_git_killed`), before its first byte (the client was told why, a push with its `push_id`) or after it (git's own words ended the answer, or, for a process the node ended, the connection was cut); or the request failed on the node itself before git answered (`reason`: `node` — its disk refusing a write into the repository's copy, or anything else the engine did not name; the `error` says what), answered 503. |
| `code_git_unavailable` | `WARN` | A git request could not read the code log here (no quorum, a single member's stalled write, a copy out of service, no data node answering), answered 503. Only the code log's own failure is logged so. |
| `code_object_store_unavailable` | `WARN` | A fetch's sync could not read a pack from the object store for a reason that is no answer about the pack — a timeout, a throttle past its retries, the store unreachable — answered 503. A pack the store does not hold, or holds damaged, is `code_pack_missing` instead. |
| `code_cache_sized` | `INFO` | The node built its code host and sized its cache: the cache's `dir`, its budget in `bytes`, whether it was `derived` from the volume or set as `git.cache_max_bytes`, and every figure it was decided from — the `room` the cache can use, which is the volume's `free` space plus what the cache already `held`, less what an embedded `broker` on the same volume may still write under its limit and the request `spools`' allowance (64 MiB for each of the pool's `transfers`), and whether the volume is `dedicated` to it (half the room where it is shared, seven eighths where it is the cache's own). `WARN`, with a `detail` naming what to change, when a derived budget comes out under 1 GiB or an explicit one is more than the room. A room of nothing is never logged here: the node refuses to build its code host, and the error names the same figures and what to change. |
| `code_git_ready` | `INFO` | The node's git is 2.55 or newer (the node floor, `gitproc.NodeMinVersion`) and its process pool is built: the git's `path` and `version`, its `read_slots`, `maintenance_slots` and `transfer_slots`, the empty `home` every git process runs with, the node's `memory` the lanes were sized by and where it was read (`memory_from`: `/proc/meminfo`, a cgroup's limit file, or `sysctl hw.memsize`), and the format of the trace2 event stream its git writes (`trace_evt`), from which a failure's damage is read. |
| `code_git_killed` | `WARN` | The node ended a git process (`class`, `command`): past its deadline (`reason`: `deadline`), or having read and written nothing for its stall bound (`stall`) — a minute for a transfer, twelve of git's keepalives. `DEBUG` when its caller ended it (`caller`): a client that hung up, a node stopping. |
| `code_git_straggler_killed`, `code_git_pipe_held` | `WARN` | Something a git process started outlived it: still in its process group, and killed; or outside it and holding git's output open, which the node closed after five seconds. |
| `code_git_signal_withheld`, `code_git_signal_failed` | `WARN` | The node could not read the kernel's record of a git process's group, so sent it no signal rather than risk signalling a stranger; or sending one failed. |
| `code_git_cpu_unread` | `WARN` | Ending a git process, the node could not read every process record under `/proc` for what its group had spent (a `/proc` mounted `hidepid=1` hides other users' records), so the run's `crewlet.code.git.cpu` is short by whatever a record it could not read held. |
| `code_git_protocol_dropped` | `DEBUG` | A client's `Git-Protocol` header was not one the node passes to git, and git ran without it. |
| `code_git_abandoned` | `DEBUG` | A git request ended because its client hung up (`by`: `client`), answered nothing, or because the node is stopping (`by`: `stop`), answered 503 — never the code log failing. A push abandoned after its proposal is recorded as its session concluded it (`unknown`, `applied`). |
| `code_pack_missing` | `WARN` | A git request failed because a pack the rows name is gone from the object store or damaged there (`damaged`), answered 503 naming the pack. The [cache](#the-cache-on-a-git-node) logs the same line once for the sync that found it (below); the alarm is [`objects_missing`](object-store.md#alarms). |
| `code_pack_checksum_mismatch`, `code_bad_snapshot` | `ERROR` | A git request failed because a downloaded bundle is not the one its row describes — the cache logs that line too, once for the sync (below) — or the rows cannot be read into a cache: an operator's problem, answered 500 with no `Retry-After`, since no retry ends it. |
| `code_pack_rejected`, `code_push_invalid` | `ERROR` | The receiving git wrote a pack the engine's own checks refuse, or the engine built a proposal its own check or its committer refused: a bug, and the push fails. A proposal is checked as soon as the commits it introduces are listed, and its bundle's upload is ended there; a bundle already stored is named by no row, and the object collector removes it after its grace. A push past what one push may carry is never this: it is refused before its work, in git's words. |
| `code_compaction_pass` | `INFO` | A git node's minute's poll of the repositories a compaction may be due in that compacted, refused or failed something: how many it `listed`, how many of them it `maintained`, how many were `due`, `compacted`, `refused` and `failed`, and how many are `behind` — at the most live packs a repository may hold; `DEBUG` when it did none of those. |
| `code_compaction_pass_failed` | `WARN` | A poll's listing of the repositories a compaction may be due in failed: nothing was compacted, and the alarm reading stays as the last pass left it until the next poll, a minute on. |
| `code_compaction_maintainer_unknown` | `WARN` | Who serves git, and what each git node advertises, could not be read — or the listing carries no git node at all, this one included, its own presence lapsed — so the poll compacted nothing rather than guess which repositories this node maintains ([Who maintains a repository](#who-maintains-a-repository)); `listed` is how many repositories its listing named. |
| `code_compaction_applied` | `INFO` | This node's compaction of a `repo` landed: its `compaction` id, how many `inputs` it merged into how many `outputs`, the `objects`, `bytes_in` and `bytes_out`, the record's `position` and how long it `took`, from its plan to its answer. |
| `code_compaction_refused` | `WARN` | A compaction this node built was refused — by the repository's committer (`refusal`: `repo_deleted`, `upload_expired`, `pack_too_large`, `not_applied`) or by an object store with no room for its outputs (`store_full`: the repository is held back until the store has the room, and the line names the room to make). `pack_too_large` and `upload_expired` are a build past its own bounds or longer than twelve hours, and say so in `detail`. |
| `code_compaction_failed` | `WARN` | A compaction this node began did not land: the `step` that failed — `sync`, `admit`, `build`, `upload`, `propose`, `adopt` — and why. `reason` `outputs`: the build wrote more packs than one compaction records; `bundle_too_large`: an output stayed past the bundle rule at a smaller cut too. Either uploaded nothing and halves what the repository's next plan merges. Tried again at the next poll. |
| `code_compaction_rejected` | `ERROR` | An output of a compaction failed its own seal on its way to the object store — the bytes the store took are not the bundle the build laid out: a bug, or a disk returning other bytes than were written. Nothing is proposed, and the object, named by no row, is the collector's. |
| `code_compaction_adopt_failed` | `WARN` | A compaction this node built landed and the node could not link its outputs into its own copy: its next sync downloads them instead. |
| `code_compaction_store_unread` | `WARN` | The object store's room could not be read, so every repository held back for a full store waits for the next poll. |
| `code_compaction_stop_unfinished` | `WARN` | The node stopped while a compaction or a publication was still running past the stop's wait: its build was killed and its uploads abandoned, and what it stored, named by no row, is the collector's. |
| `code_publication_pass` | `INFO` | A git node's minute's pass of the repositories a publication may be due in that published, refused or failed something ([When a repository is published](#when-a-repository-is-published)): how many it `listed`, how many hold a layer this node refused (`damaged`, each a repair), how many of them it `maintained`, how many were `due`, `published`, `refused` and `failed`; `DEBUG` when it did none of those. |
| `code_publication_pass_failed` | `WARN` | A pass's listing of the repositories a publication may be due in failed: nothing was published, until the next pass a minute on. |
| `code_publication_maintainer_unknown` | `WARN` | Who serves git, and what each git node advertises, could not be read — or the listing carries no git node at all, this one included — so the pass published nothing rather than guess which repositories this node maintains; `listed` is how many repositories it named. |
| `code_publication_applied` | `INFO` | This node's publication of a `repo` landed: its `publication` id, the `layer` it put on top of the chain over its `base`, how many layers it `replaces`, how many packs it `repacked` into how many `outputs`, the layer's `objects` and `bytes`, the record's `position` and how long it `took`, from its plan to its answer. |
| `code_publication_refused` | `WARN` | A publication this node planned or built was refused: by the repository's committer (`refusal`: `repo_deleted`, `upload_expired`, `pack_too_large`, `not_applied`, with its `detail`); by an object store with no room for its parts or outputs (`store_full`: the repository is held back until the store has the room, and the line names the room to make); or before its build, a layer whose files would pass 16 parts of 1 GiB (`layer_too_large`, with its `objects`, its `estimate` and the `most` its parts hold), not planned again until the repository moves. |
| `code_publication_failed` | `WARN` | A publication this node began did not land: the `step` that failed — `sync`, `store`, `admit`, `build`, `upload`, `propose`, `adopt` — and why. `reason` `no_bitmap`: git wrote the layer without a bitmap even over one more layer, since no pack it covers holds a commit; `outputs` and `bundle_too_large`: its repack wrote more packs than one record names, or an output past the bundle rule, as a compaction's would. Tried again at the next pass. |
| `code_publication_base_lost` | `WARN` | The object store no longer holds every part of a `layer` this node holds installed — a restore, which carries no layer, or a bucket's own deletion — so the publication replaces it, and every layer above it, rather than build over a base no other git node can install. |
| `code_publication_rejected` | `ERROR` | A part of a publication's layer or one of its repack's outputs failed its own seal on its way to the object store: a bug, or a disk returning other bytes than were written. Nothing is proposed, and what was stored, named by no row, is the collector's. |
| `code_publication_adopt_failed` | `WARN` | A publication this node built landed and the node could not link its outputs or its `layer` into its own copy: its next sync downloads the outputs, and its installer the layer. |
| `code_publication_store_unread` | `WARN` | The object store's room could not be read, so every repository the publication loop holds back for a full store waits for the next pass. |
| `code_compaction_invalid`, `code_publication_invalid`, `code_compaction_unanswerable` | `ERROR` | Under the component `code.compact`: the node that built a compaction or a publication found it breaking a rule its committer holds it to before storing any of it — a bug in that node's build, so nothing was stored or proposed. Otherwise a repository's committer refused a compaction before its record because its outputs hold more objects than the packs it replaces — or a publication, a compaction carrying a published multi-pack-index layer, because the chain it leaves would hold more than 32 layers or its layer names a pack or the id of a layer below it — a bug in the build of the git node that made it (`node`), never the repository's: nothing was recorded, and what it stored, named by no row, is removed by the object collector after its grace; or a compaction's record applied and this node's rows do not hold the outputs or the layer it added (`missing`) — a bug in the build that wrote the record, answered `unknown`, so the node that built it adopts nothing. Each with the `repo` and the `compaction_id`. Report any of these lines. |
| `code_layer_not_applied` | `WARN` | Under the component `code.apply`: a publication's record applied as nothing on this node, because the layers it replaces are not the top of the repository's published chain or its base is not the layer below them (`chain`, `base`, `replaces`) — a record no correct committer forms, which every node applies alike, so the chain stays as it was. With the `repo`, the `layer` and the record's `position`. Report it. |
| `code_pack_adopt_failed` | `WARN` | The receiving node could not link a push's pack into its cache once its row applied; its next sync downloads it instead. |
| `code_push_blob_path` | `WARN` | The path of a blob over the limit could not be found; the refusal names the blob without it. |
| `code_push_store_full` | `WARN` | A push's bundle was refused by the object store because it has no room: the fleet's NATS bucket holds as many bytes as it [reserves](object-store.md#its-byte-reservation), or an S3-compatible bucket's provider refused the write for room (MinIO's `XMinioStorageFull`, a quota spent). The push's `bytes`, and the store's own answer naming its bucket (`error`). The push is refused `store_full` and recorded `refused`, and nothing was stored or proposed; every push that stores a pack is refused the same until an operator makes room. |
| `code_push_unchecked` | `WARN` | The engine could not finish its own work on a push once receive-pack had written its pack: a git process over the push's objects on this node failed or ran past its deadline (the force check's merge-base, the blob scan, the listing of the commits it introduces), or the push's pack could not be read. The push fails — git prints the `words`, the push is recorded `failed` — and nothing is proposed. Its bundle may already be stored, since the listing runs beside the upload: named by no row, the object collector removes it after its grace. `DEBUG` when the push's request ended under it — its client gone, the node stopping, receive-pack ended — which `code_git_abandoned` or `code_push_failed` records. |
| `code_hook_accept_failed` | `WARN` | The hook socket failed to accept a connection; it goes on accepting. |
| `code_hook_protocol`, `code_hook_refused` | `WARN` | A push's hook spoke out of turn — git's version block or its commands not what git sends — or a connection to the hook socket named no session the engine started; the push fails and nothing is stored. A failure of the push's own work after the hook spoke is `code_push_unchecked`. |
| `code_hook_panic` | `ERROR` | The engine failed while serving one push's hook: that push was refused and its stack logged, and the node goes on serving. |
| `code_host_stop_unfinished` | `WARN` | The node stopped with git requests still running past its stop's wait. |
| `code_fetched_unpublished` | `WARN` | Under the component `code.readaudit`: the event stream refused a flush of this node's records of [who read](#who-read-it) its unit repositories. The records it held back (`records`, and the `reads` they count) are kept, sealed, and published again by the next flush, fifteen seconds later. |
| `code_fetched_dropped` | `WARN` | Under `code.readaudit`: the stream refused this node's records of reads for longer than the node keeps them (4 096 records): the oldest `records`, and the `reads` they counted, are dropped, and who read those repositories answers without them. |
| `code_fetched_lost_at_stop` | `WARN` | Under `code.readaudit`: the node stopped while the stream was refusing its last records of reads; those `records` and their `reads` are in no answer. |
| `code_git_unparsed` | `ERROR` | Under the component `code.query`: a [history](#a-paths-history) or a [blame](#who-changed-a-line) read answered output that does not read as git writes it — a record past its fields, an id that is no id, a porcelain line out of its place (`kind`, `error`). The read fails and nothing of that page is answered: a bug in the reader, or a git writing what this build does not read — never a message's doing, since every field ends in a byte no message holds. |
| `code_git_reads_stop_unfinished` | `WARN` | Under the component `code.query`: stopping its code host, the node cut its [server-side reads](#server-side-reads) in flight — each answered unready, so its asker took it to the next git node — and some had still not returned when the stop's wait ended. A read of a `visibility: unit` repository that answers after this line is recorded after the code host's stop, and so after the node's stop has flushed what [who read it](#who-read-it) is kept from. |
| `code_status_answerer_not_withdrawn` | `WARN` | Stopping its code host, the node could not finish withdrawing its answers to the fleet's code-status question (`crewlet.code.status`), which it stops answering first so that no peer is told of a cache that is going: the broker refused to drop its subscription, or answers already in flight outlasted the stop's wait — the `error` says which. Either way the node starts no answer after this line, and the host's stop goes ahead. |
| `code_verify_finished` | `INFO` | A check of this node's copy of a repository ended: the `packs` it checked, how many files the sync before it had found damaged and `repaired_by_sync` in place, the `problems` it found, and how a repair put it right: `repaired_in_place`, its own directory read whole in place, and `rebuilt`, a new copy (both, a rebuild of what the read in place left). Its report is read at `GET /code/repos/{name}/verify/{id}` for an hour after. |
| `code_verify_failed` | `WARN` | A check of this node's copy of a repository could not end — a stop cut it short, a pack the object store lacks stopped its repair — with what the sync before it had `repaired_by_sync` and the `problems` it had found by then. |

A git node's [cache](#the-cache-on-a-git-node) logs under the component `code.cache`. Every line at `INFO` and above is here; its `DEBUG` lines (each sync, each background index written, a run stopped from outside) are left out.

| Line | Level | Means |
|---|---|---|
| `code_cache_opened` | `INFO` | The node opened its cache at boot: the `dir`, the `repos` and `bytes` it already held, its byte `budget`, and how many leftovers it moved aside (`trashed`); and the node's `memory` as its git read it, where from (`memory_from`: `/proc/meminfo`, a cgroup's limit file, or `sysctl hw.memsize`), what it keeps for everything but its background work (`memory_reserved`), and what that leaves the work (`maintenance_budget`); and the most runs of its indexes of pushes it holds mapped at once (`mapped_runs`: a quarter of the kernel's `vm.max_map_count`, [Kept like the rest of the copy](#which-push-landed-a-commit)). |
| `code_cache_map_count_unread` | `WARN` | The node could not read how many mappings the kernel lets its process hold (`/proc/sys/vm/max_map_count`; `error`), so it bounds the runs of its indexes of pushes it maps as if the count were Linux's default (`assumed`, 65 530). A sandboxed kernel that hides `/proc/sys`; nothing to do unless the host lowered the count below its default, which lowers what every process on it may map. |
| `code_cache_memory_changed` | `INFO` | The node's memory read differently from the last reading — a container resized in place — and the background work's `maintenance_budget` moved with it; `was` is the reading before. The git lanes keep their boot sizes until a restart. |
| `code_cache_memory_unreadable` | `WARN` | The node's memory could not be read again; the last reading and its budget stand until a read succeeds. Once until then. |
| `code_cache_trashed` | `WARN` | At boot, something in the cache that is not a repository's current copy was moved aside to be removed — a copy an interrupted rebuild left, a stray, or a copy recorded in another build's format; `why` says which. Nothing is lost: the rows and the object store bring the repository back at its next fetch. |
| `code_cache_state_unreadable` | `WARN` | A repository's copy has no readable record of what it holds. It is kept, and its next fetch reads the repository whole, reusing every pack it already holds. Recurring, it is the disk. |
| `code_cache_evicted` | `INFO` | A repository's copy was evicted to bring the cache back under its byte budget, least recently used first; `idle_for` is how long it had gone unused. Its next fetch is a cold sync. Frequent, the budget is too small for the repositories this node serves. |
| `code_cache_over_budget` | `WARN` | A repository larger than the cache's whole byte budget is being served. It is evicted as soon as no request holds it, so every fetch of it is a cold sync: give the cache more room than the repository holds. |
| `code_pack_missing` | `WARN` | A pack the code log names could not be had from the object store: absent, and still named by a read at the log's head made after the store said so of that pack, or held `damaged`. The sync that needed it fails naming the pack, and the collector's daily audit raises [`objects_missing`](object-store.md#alarms) for the same object — restore it as that alarm says. |
| `code_pack_checksum_mismatch` | `ERROR` | A pack's bytes arrived whole and matching their checksum, but are not the pack its row describes: a section's size or the pack's own checksum disagrees. No retry can help — the row or the push that stored it is wrong — so the sync fails at once. Keep the line and report it. |
| `code_cache_log_recreated` | `INFO` | The code log was recreated under the cache (provisioned anew, as by a restore of the broker), so every repository on the node is read whole at its next fetch. |
| `code_cache_log_rewound` | `WARN` | A read at the log's head found the code log behind what this node had already read from it: the log was restored from an older copy while the node ran. Every repository on the node is read whole at its next fetch and goes back to what the log holds. A restore is meant to be done with the fleet stopped; restart every node that ran across one. |
| `code_cache_stream_changed` | `INFO` | A sync asked for what changed since its last read and was answered from a different code log than the one it last read — the log was recreated under it — so it reads the repository whole instead. |
| `code_sync_restarted` | `INFO` | A repository's rows moved under a read that spans pages — the log under it re-anchored — and the read began again from its first page (`restart`); it begins again at most three times, and a sync whose history moves a fourth time fails. |
| `code_sync_damaged` | `INFO` | The node's own sync of a repository — peeling the tags it sets, in the repository's copy — found the copy damaged, or the node would not run git in it for its shape, or still would not once the sync had read it whole (`reason`: `corrupt`, `derived`, `missing`, `shape`, with git's `error`; `swept` true when the sync had read the copy whole; `packs`, those git named that, hashed, are not what their push stored); the copy is repaired as a request's would be — a shape the sync did not read whole, and a damaged derived file the sync did not just drop, in place; a pack's or an object's damage in place first, one download a damaged pack (the pack git named, once its hash confirms it), then by a scrub, and by a rebuild only when neither finds anything to fetch again; a shape the sync had read whole by a rebuild — and the sync answered by the repair. A check's own sync repairs nothing: what it met is the check's first finding. |
| `code_cache_reshaped` | `WARN` | A repository's copy the node would not run git in because it was not the shape the engine writes was read whole again in place, every pack it holds reused; `moved` is how many files the engine never writes there went to the trash. One is a disk's bad moment or a person's edit; a run of them is something outside the engine writing into the cache's directory. Counted as `crewlet.code.cache.rebuilds{reason=shape}`. |
| `code_cache_check_damaged` | `WARN` | A repository's copy held a `file` — its commit-graph, multi-pack-index, bitmap or reverse index, a file of its installed published chain (a layer's `.midx`, `.bitmap` or `.graph`, or one of the two chain files, held line for line to the layers installed), a run of its index of pushes (`crewlet/provenance.<n>`, held to its SHA-256 trailer before anything reads one this process did not write, and by every scrub), or a pack, its index or its reverse index — that failed its own checksum, or that the disk could not read (`error`), or a run of the index of pushes gone from the disk while the index names it — repaired the same way, since git cannot serve it either: before the copy answered, or in a scrub behind the serving (`trigger`: `first`, the copy's first sync since the node started, or the scrub after it; `unvouched`, a later sync reading a file replaced since; `scrub`, a used copy's daily scrub; `report`, a scrub a git process's report or crash asked for; `forced`, the repair a git failure of a pack or an object asked — a request's, or the node's own sync's — which reads every index file whatever was checked before; `request`, the scrub that repair runs when it found nothing, or when the sync's git met the damage again past it). `action` says what was done: `dropped`, a derived file the node writes again at once over the same packs — for a run of the index of pushes, the whole index, which the full sync that repairs the copy makes again at its own position and the backfill reads back below it as far as it is wanted; `reinstalled`, the pack fetched again (`code_cache_pack_reinstalled`); `refetched`, a published layer's file, the chain cut below that layer and the layer fetched again with every one above it ([Published layers](#published-layers)); `rewritten`, a chain file, written again from the layers installed. One is a disk's bad moment; a run of them is a disk losing data, or something outside the engine writing into the cache. Counted as `crewlet.code.cache.checks{outcome=damaged}`. |
| `code_cache_pack_reinstalled` | `WARN` | A pack of a repository's copy was fetched again and put in over its own files, in the same copy (`pack_id`): `found` says why — `trailer`, the pack, its index or its reverse index failed its checksum; `size`, a file of it was not the size its row says; `hint`, a request's git or the node's own sync's named it and it is not what the push stored; `absent`, a sync found the copy had lost it — named by the rows at a position the copy had read, whichever sync put it back: a request's or a sync's own repair, or any that read the copy whole — put in rather than over anything — and `trigger` what looked, as `code_cache_check_damaged`, or `request` for a pack git named. One download, counted as `crewlet.code.cache.rebuilds{reason=stored}`; a request already reading the pack finishes on the files it opened. |
| `code_cache_scrub_failed` | `WARN` | A scrub of a repository's copy could not list its files (`error`): it decides nothing, and the next scrub, a day later or at a report's word, lists them again. A disk refusing reads under the cache. A file the scrub lists and cannot read is no failure of the scrub's: it is `code_cache_check_damaged`, with its `error`, and repaired. |
| `code_cache_pack_reinstall_failed` | `WARN` | A pack of a repository's copy that failed its checksum or could not be read (`found`, as `code_cache_pack_reinstalled`) could not be fetched again (`error`): the copy keeps serving as it is, its other repairs go on, and the pack is tried again ten minutes later, then twice as long each time up to a day. Until then a request or sync that meets the pack's damage is answered 503 rather than read again or rebuilt, since either would ask the store for the same pack. The `error` says why — most often the object store not answering ([the object store](object-store.md)). |
| `code_cache_derived_repaired` | `WARN` | A repository's copy whose derived files git could not read — its multi-pack-index (`MIDX is likely corrupt`), its commit-graph, or a chunk-format file of either — named by any git process over the copy, had those files dropped in place (`files`), its packs kept, none downloaded; the node writes sound ones over the same packs, and requests are served meanwhile from the packs themselves. `trigger` says what named them: `request`, a request's git that failed on them; `round`, the node's own sync's git; `report`, any git run that named them, failing or not — counted as `crewlet.code.cache.reports`; `check`, their own checksum, before the copy answered; `scrub`, their own checksum, in a scrub behind the serving. A request's or a sync's is counted as `crewlet.code.cache.rebuilds{reason=derived}` — or under the reason the copy was already being read whole for. One is a disk's bad moment; damage that survives it is a pack's, repaired as one — in place first — and the copy rebuilt (`code_cache_rebuilt`, `corrupt`) only when that finds nothing to fetch again. A copy serving from an installed published chain drops nothing on a git process's word: the report makes the copy's scrub due, which cuts the chain at a layer whose files fail (`code_cache_check_damaged`, `refetched`). |
| `code_cache_derived_damaged_again` | `WARN` | A derived file (`part`) was named damaged again within a day of its last drop for damage, whatever the node wrote of it between: it is dropped and left to the ordinary schedule rather than written again at once. Logged once a copy a part; counted as `crewlet.code.cache.reports{outcome=again}`. A disk that keeps losing what is written to it, or a git that rejects its own write — look at the disk. |
| `code_cache_hint_confirmed` | `WARN` | git named a packed object of a repository's copy corrupt and the pack holding it (git's sentence that a packed object is corrupt: a report of its own on 2.55, a bare message on 2.56), and that pack, hashed, is not the one the push stored. Whichever git named it — a request's, or the node's own sync's, peeling the tags it sets — the pack is fetched again in place (`code_cache_pack_reinstalled`, `found` `hint`). Counted as `crewlet.code.cache.hints{outcome=confirmed}`. |
| `code_cache_hint_unknown`, `code_cache_hint_unread` | `INFO` | git named a pack corrupt in words, and it was no pack of the copy's rows (`unknown`), or no verdict was reached (`unread`): its rows could not be read, no read slot was free for the hash, the hash ran past its bound (`bound`: a tenth of a second a mebibyte of the pack, five times the slowest cold disk or CPU the cache allows for), or the pack's files were replaced while it was hashed. Nothing is repaired, and an `unread` pack is hashed again by the next request git fails on it. Counted as `crewlet.code.cache.hints{outcome=unknown}` and `{outcome=unread}`, and each hash as `crewlet.code.cache.hint.hashes`. |
| `code_cache_hint_gone` | `DEBUG` | A request git failed with such a report ended before the pack's hash did — its client gave up or a balancer cut it. The hash goes on, on the node's own time, and the next request about that pack takes its verdict. Counted as `crewlet.code.cache.hints{outcome=gone}`. |
| `code_cache_rebuilt` | `WARN` | git found a repository's copy damaged — a request's git or the node's own sync's — and repairing it in place and a scrub found nothing to fetch again, or the node still would not run git in it for its shape after reading it whole in place, and the node rebuilt it beside the damaged one from the rows and the object store; `reason` is what git found (`corrupt`, `missing`, `shape`), or `verify` for a repair asked for. One is a disk's bad moment or damage in the rows (`crewlet code verify` names which); a run of them is a disk to replace — and a `shape` here, something that keeps writing into the cache's directory. |
| `code_cache_rebuild_refused` | `WARN` | A rebuild was asked for and refused: one that did not end the failure ran too recently, or the copy holds exactly what such a rebuild read. The damage is then in the object store or the rows, not in this node's copy. At most once every ten minutes a repository. |
| `code_cache_rebuild_seeded` | `INFO` | A rebuild linked the `packs` found whole into the new copy, of the `whole` found so — by `crewlet code verify` asked to repair, or by the scrub a request's repair ran before it — so it downloads only the rest. A copy serving from a published chain logs a second line: the `layers` of the `installed` chain it linked in, root first, each over packs it linked and held to the row it was installed from ([Published layers](#published-layers)); it fetches the layers above them. |
| `code_cache_rebuild_seed_failed` | `WARN` | A rebuild could not link a pack it had already found whole into the new copy, so it downloads that pack instead — or could not link a published `layer` of the copy's chain, or found its files not its row's, so it fetches that layer and every one above it. The disk refusing a hard link, or a layer damaged in the copy. |
| `code_cache_swapped` | `INFO` | Packs left a repository's copy — a compaction retired them, or a whole read found pack files no row names — or a published chain was placed in a copy a request may be reading ([Published layers](#published-layers)), and the node moved the copy into its next directory ([When packs leave a copy](#the-cache-on-a-git-node)): `from` and `to` the two directories' numbers, `packs_kept` the packs the rows still name and the chain holds, hard-linked, `packs_dropped` those that left, `layers` the installed chain the next directory holds, `graph` whether the commit-graph came along (`kept`, a compaction's), went with a pack no compaction retired or a chain cut whole (`dropped`, written again at once), was never there to keep (`none`), or moves with the chain (`chain`), and `took`. Requests reading the old directory finish there. |
| `code_cache_swap_failed` | `WARN` | Packs left a repository's copy, or a published chain was being placed in it, and the node could not build its next directory (`step`, `error`; `incarnation` the copy's number): the copy keeps serving, every pack the rows name still in it, and the repository's next sync tries again, or the installer a chain's layers. Recurring, the cache volume is refusing links or is full; at `step` `canary`, the read through the placed chain's bitmap could not run, the node's read lane full for ten seconds or the read past its deadline. |
| `code_layer_installed` | `INFO` | A published layer of a repository's chain was placed in this node's copy ([Published layers](#published-layers)): the `layer` (its multi-pack-index's checksum) and the `seq` of its row, its `bytes`, how long its fetch, check and placement `took`, and whether it was `adopted` — linked from the files the repository's maintainer wrote on this node rather than fetched. |
| `code_layer_refused` | `WARN` | This node will not install a published layer while the rows name it ([When a layer is missing](#when-a-layer-is-missing)): `outcome` `missing`, a part the object store holds nothing for while the log's head still names the layer; `corrupt`, bytes or files its row does not describe, or a placement whose read through its bitmap failed — a layer over the wrong base; or `newer_git`, written by a git newer than this node's — with the `detail`. The copy serves from the layers below it, or in its own mode. Once a copy a layer; a restart tries it again. |
| `code_layers_cut` | `INFO` | A repository's installed chain was cut in place (`incarnation` the copy's number), the `kept` layers beneath the `cut` ones staying: `why` is `layers-moved` — the rows name another chain, or a layer's file failed its checksum, its layers then fetched again — or `pack-removed` or `pack-retired`, a pack a cut layer names leaving. |
| `code_layer_install_failed` | `WARN` | A published layer could not be installed (`error`), with the layer's `seq` and `outcome`. The installer logs it when the object store failed to answer, this node's disk refused a write, or a part was `missing` while the log's head, which decides whether the rows still name the layer, could not be read — with `retry_in`, a minute, doubling to half an hour; a full sync logs it for any layer it could not fetch, or placed in a copy nobody held and could not read through its bitmap — no read slot within ten seconds, or the read past its deadline — and leaves the layer to the installer (one `corrupt` is refused at once). The copy serves as it is meanwhile. |
| `code_cache_adopt_failed` | `WARN` | The node that received a push could not link the push's pack into its cache; its next sync downloads the pack instead. Recurring, the disk is refusing writes. |
| `code_cache_maintenance_failed` | `WARN` | A background indexing run failed at its `step` (`multi-pack-index`, `commit-graph` — the copy's own graph, or its tip over a published chain — or `state` when its record could not be written). The repository is served without what the step writes — slower, never wrong — and the next run waits out the interval. |
| `code_cache_compaction_refused` | `WARN` | A compaction of a `repo` needs more memory than this node can give its background work — its `estimate`, against the `budget`, with the `memory` it was taken from and what was `reserved` — so this node does not build it; logged once for the copy however many polls ask again, and counted `crewlet.code.cache.maintenance.refusals{step=compact}`. The next poll asks again, so a node given more memory builds it; at the pack bound the repository's pushes are refused meanwhile, which `code_compaction_behind` raises. |
| `code_cache_maintenance_refused` | `WARN` | One `step` of a repository's background work needs more memory than the node can give it — its `estimate`, the step's figure an object and room for a clone beside it, against the `budget`, with the `memory` it was taken from and what was `reserved` — so it never runs on this node. `bitmap`: the repository is indexed without one, and a clone walks its history; `index`: it is served from its packs' own indexes, a cold cache's index included; `graph`: its walks go by date, and a push of a commit dated before its parent walks every commit; `tip-graph`: a copy serving from a published chain walks the commits pushed since its last publication by date, and the rest by generation. Slower, never wrong. Once for each copy of the repository the cache holds and each step, while [`code_maintenance_refused`](../reference/alarms.md) stands for as long as a bitmap stays refused — never for a tip. Give the node more memory, or — where `git.max_transfers` is set — fewer transfers, each of which keeps 224 MiB of it. |
| `code_cache_provenance_gap` | `WARN` | A sync applied pushes it could not fold into the repository's index of which push introduced each commit (`from`, `through`, `error`: a delta that served no pushes, or one beginning above where the index ends), so it left that range to the backfill, as a full sync does; until it is read, an answer naming a commit in it says the index does not hold it. One now and then is a page the sync could not read; a run of them, `error` says why. |
| `code_cache_provenance_lost` | `WARN` | A full sync found a run the repository's index names missing from the disk or at another size, so the index was made anew at that sync's position and the backfill reads back below it as far as it is wanted — or a move into the copy's next directory ([When packs leave a copy](#the-cache-on-a-git-node)) found one missing, so the next directory starts with no index until its next full sync makes one. Something outside the engine removing files in the cache, or a disk losing them. |
| `code_cache_provenance_unlisted` | `WARN` | A full sync could not list the repository's `crewlet/` directory (`error`), so it kept the index as its record names it; the next full sync lists it again. A disk refusing reads. |
| `code_cache_provenance_seeded` | `INFO` | A rebuild linked the `runs` of the repository's index into the new copy, so the index outlives it. |
| `code_cache_provenance_dropped` | `INFO` | The backfill found the repository gone from the rows at a position that had held it (`why`) — purged — and dropped its index; the repository's next sync takes its copy out of the cache. |
| `code_cache_provenance_unpersisted` | `WARN` | The backfill changed a repository's index but could not write the copy's record of it (`error`): the index serves the change, and the runs it replaced stay on the disk, named by the record there, until the next write of the record — a sync's, the maintainer's or the backfill's own — catches up and takes them off the disk with it; a restart before it reads the index as that record left it. A disk refusing writes, or full. |
| `code_cache_memo_trimmed` | `INFO` | The memo was past its sixteenth of the cache's budget, and the node dropped its oldest `cohort` — the answers used least recently — from every `copies` that held it, `bytes` in all. |
| `code_cache_memo_untrimmed` | `WARN` | A copy's oldest cohort could not be moved out of its memo (`error`): the trim stops, and the next pass of the janitor takes it up. The disk refusing a rename within the cache's own directory. |
| `code_cache_memo_corrupt` | `WARN` | An answer in a repository's memo (`cohort`, `entry`) did not read back whole — cut short, changed, or another answer's — so it was removed and the read computed it again. More than a rare one is the node's disk damaging what it holds. |
| `code_cache_memo_unremoved` | `WARN` | An answer that did not read back whole could not be removed (`error`): every read of it is answered by git until it is. |
| `code_cache_memo_unwritten` | `WARN` | A read could not keep the pages it cut (`error`), so it answered without keeping them, and the next read of them runs git again. Once a read. |
| `code_cache_memo_unlisted` | `WARN` | The node could not list a copy's memo after its boot (`error`), or count a cohort of it no state recorded: it tries again at the janitor's next pass, and meanwhile counts only what the copy's state recorded. |
| `code_cache_memo_foreign` | `WARN` | A copy's memo held something no cohort is — `found` names the first, as a build that kept the memo in one directory left it — so the node moved the whole memo aside unread; its answers are computed again as they are asked. |
| `code_cache_memo_unstated` | `WARN` | The node could not write a copy's state after its memo moved (`error`): it tries again at its next tick, and a restart before then counts the memo as the last state written does. |
| `code_cache_memo_carried` | `INFO` | A rebuild, or a move into the copy's next directory ([When packs leave a copy](#the-cache-on-a-git-node)), moved the repository's memo of answers into the new copy (`from` and `to` are the copies' numbers), whole and in one rename, with the `bytes` it holds. |
| `code_cache_memo_uncarried` | `WARN` | A rebuild, or a move into the copy's next directory, could not move the memo into the new copy (`error`): the new copy starts with none and computes its answers again; the old copy's goes with it. The disk refusing a rename within the cache's own directory. |
| `code_cache_backfill_failed` | `WARN` | A step of the backfill failed for the `repo` (`error`): a page the data nodes could not serve, a run it could not write, or two runs it could not merge — kept as they are, a lookup reading one more run meanwhile. That repository's steps wait `retry_in`, five seconds doubling to five minutes with each of its failures in a row, while every other repository's steps go on; only failures of more than one repository in a row make the backfill as a whole wait, doubling the same way. One now and then is a data node restarting; one repository's, again and again, is rows of it the data nodes cannot serve this node (a rolling upgrade's record deferred everywhere) or a disk refusing its writes; every repository's, while `crewlet.code.cache.provenance.behind` stays up, is the data nodes unreachable from this node, or its disk full. |
| `code_cache_backfill_unpaced` | `WARN` | The backfill could not count the fleet's live git and data nodes to pace its next page (`error`), so it waits five seconds, doubling to five minutes, and counts again rather than reading on a guess. |
| `code_cache_round_index_held` | `INFO` | A cold sync of a repository with many packs answered without writing its index first, because the background work in flight left the node's memory no room for it (`estimate` against the `budget`). Slower, never wrong; the background index follows. |
| `code_cache_round_index_skipped` | `WARN` | A cold sync of a repository with many packs answered without writing its index first: the node's git reads were all busy, or the write failed. Slower, never wrong; the background index follows. |
| `code_cache_trash_failed` | `WARN` | Something the cache had finished with could not be moved aside or removed. It stays on the disk until a later pass or the next boot removes it; recurring, the disk is refusing work. |
| `code_cache_held_unmeasured`, `code_cache_scratch_unmeasured` | `WARN` | At boot, or at a pass of the cache's own housekeeping, a file in the cache could not be measured, so the cache counts less than it holds on the disk. Fix the disk or the file's permissions. |
| `code_cache_use_unrecorded` | `WARN` | A repository's last use could not be written to the disk, so after a restart it may be evicted sooner than its use says. |
| `code_cache_scratch_remove_failed` | `WARN` | The throwaway repository a push was received in could not be moved aside when the push ended. The next boot clears it. |
| `code_cache_stop_unfinished` | `WARN` | The node stopped while its cache still had work running in its directory. The cache keeps its claim on the directory until that work ends, so no other process can open it meanwhile. |

---

## See also

- [Configuration: Code](../getting-started/configuration.md#code) — every field of the `code` block
- [Hosting Code](../guides/code.md) — the steps: turning it on, a token for each person, clone, fetch and push
- [Scaling Out: Git nodes](scaling.md#git-nodes) — what git nodes and data nodes each scale, and what sizes them
- [Object Store](object-store.md) — where a push's bytes live, and the collector and audit that keep them
- [Replication](../guides/replication.md) — the state-log framework the code log rides
- [Retention](../guides/retention.md) — the log's trim, and what a purge does and does not reach
- [Backup](../guides/backup.md) — a repository's rows ride the store copy, its packs ride as required references
- [Metrics](../reference/metrics.md) — the cache's bytes and repositories, its sync durations, and its rebuilds and refusals
