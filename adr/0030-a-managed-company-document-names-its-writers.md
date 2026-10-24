# ADR-0030: A managed company document names its writers, and every other credential is refused a change

- **Status:** accepted
- **Authority:** `internal/api/configapi`
- **Enforced-by:** `internal/api/configapi.TestAManagedDocumentRefusesEveryOtherCredential`, `internal/api/configapi.TestTheProgrammaticWritesAreJudgedByTheirAuthor`, `internal/api/configapi.TestAReloadOfAManagedDocumentIsOpenToEveryCredential`, `internal/api/configapi.TestAManagedRefusalComesBeforeTheRequestIsRead`, `internal/api/setupapi.TestAManagedDocumentRefusesAConnectBeforeSealing`, `internal/api/setupapi.TestAManagedDocumentRefusesAGitHubAppBeforeGitHubIsAsked`, `cmd/crewlet.TestAManagedDocumentRefusesTheOfflineWrites`
- **Tag-status:** unreleased

## The decision

Tier A's `api.auth.company_writers` lists the token ids that alone may
**change** the company document. Empty, the document is edited by people:
whoever the authority table admits to a write — `admin`, or a unit's
lead inside the subtree they lead — makes it. Set, the document is *managed*:
some other system — a GitOps pipeline, a Kubernetes operator rendering it from
custom resources — is its source, and writes it with its own Tier A token.

The list judges the **credential**, never the author. A Tier A bearer acts
under the login `token:<id>`, and every write it makes records that login as
the credential it came through (`iam.Actor.OperatorID`), so a listed id is
admitted by that and by nothing else. A person's session, a machine token
acting as its owner and a session exchanged from a Tier A token each name
another credential, so every one of them — a lead writing their own subtree
included — is refused while the document is managed. That is the managed
posture rather than an exception to it: a person's edit is exactly the one the
managing system's next reconcile overwrites.

Every write onto the document reaches `configapi`'s prepare, which asks
`Service.Authorize` before it reads anything — and each HTTP route asks it
before it reads the request — so one check covers PUT, PATCH,
the per-entity writes, a revert, every dry run of them, and `/setup` (which
writes through `Apply` and `ApplyEntity`). The two questions compose in a fixed
order: the authority table decides first, so a caller who could never write is
told the grant they lack; then this list, so a caller who could is told the
document is managed; then the org-chart reach a lead's write is admitted on,
and only then is the body read. A credential the list does not name is refused
`403 config_managed`, naming the writers and what to do instead.
`/setup` asks the same function earlier, before its side effects — a sealed
credential, a queued vendor teardown, a GitHub App created with a key issued
once — and the commands that write the store without the API (an offline
`crewlet config import` or `activate`, `crewlet run -import-company`) present
no credential at all and are refused by the Tier A that declares the document
managed; a `-company` bootstrap seed is ignored, loudly. The one reading of the
list is `config.APIAuth.MayWriteCompany`, which the viewer answer also reads as
`config_writer`, so the dashboard offers an edit only where the engine would
take it: `config_writer`, and either `admin` or the lead of the unit
being edited.

What stays open is what changes no byte of the company: a **reload** (and an
offline `activate` of the revision already active), the secret store, a
`/setup` submission that only rotates a value the document already names, and
`config rekey`. Each stays open to whoever the authority table admits to it,
and to nobody else. A rotation is the break-glass a person must keep: a leaked
key needs replacing now, and the managing system cannot know. The engine's own
writes (`iam.ActorSystem` — the reconcile loop, a chart apply, a provisioning
pass recording what it discovered) are not judged by a list of credentials.

## Why the obvious alternative is wrong

The obvious alternative is to leave the API open and let the managing system
win: it re-renders at every reconcile, so a person's edit disappears within
minutes. That is the failure this exists to remove, and it is silent — the
person saw `201`, the dashboard showed their change, and nothing said it was
gone.

The second is a route-level rule in the auth guard: refuse PUT/PATCH/POST
under `/config` for a non-writer. It misses `/setup`, which reaches the
document through `Apply`, and it refuses a reload, the one gesture a rotation
needs. The rule is about the **document**, so it lives where the document is
written.

The third is to manage the secret store too. The managing system usually feeds
credentials through the environment or its own store, and the person who must
rotate a leaked key in the middle of the night is rarely the system that
renders the company; refusing them that is an outage the setting caused.

## What this does not decide

It is not a privilege boundary. It prevents silent drift between the managing
system and the running document; it does not keep a credential from changing
what the document does. Every administrator still writes the secret store and
can reload, so a credential that is not a writer can rewrite any value the
document references through a `${VAR}` — a model's key, a webhook secret, an
endpoint — and publish it, or have the next activation publish it. A
deployment that needs separation must not grant `admin` to anybody it would
not let write the document.

It does not decide who may READ the document — reads are unchanged — nor who
may write the secret store, the work tracker, the knowledge base or anything
else outside the company document; those are the authority table's. It does
not grant anything either: a listed token still needs the grant every write of
the document asks for. It does not stop the engine's own writes, which a
managing system should carry or re-derive. It
does not make Tier A agree across a fleet: the list is per node like the rest
of `api.auth`, and every node should carry the same one.
