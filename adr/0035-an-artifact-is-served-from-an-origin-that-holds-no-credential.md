# ADR-0035: An artifact is served from an origin that holds no credential

- **Status:** accepted
- **Authority:** `internal/artifactserve`
- **Enforced-by:** `internal/artifactserve.TestTheArtifactListenerServesOnlyTheServingRoutes`, `internal/artifactserve.TestTheArtifactListenerReadsNoCookie`, `internal/artifactserve.TestAnArtifactGetsAnOriginOnlyOnASiteOfItsOwn`, `internal/artifactserve.TestEveryServedDocumentCarriesAFreshNonce`, `internal/artifactserve.TestARevokedShareStopsTheNextDocumentRequest`, `internal/artifactserve.TestAStaleServingNodeAnswersUnavailableRatherThanServe`, `internal/api/artifactsapi.TestTheAPIOriginNeverRendersAnArtifact`
- **Tag-status:** unreleased

## The decision

An artifact is arbitrary HTML, SVG and script, written by a person, an agent,
or an agent somebody prompt-injected. **It never runs where the dashboard's
session runs.** The dashboard acts with an `HttpOnly` cookie that every request
a script on its origin makes carries, so one artifact rendered there is a full
account takeover. Four rules follow, and they span the listener, the API, the
page policy and the dashboard, which is why they are a record:

1. **Artifacts are served by a listener of their own**, `api.artifacts`, which
   mounts exactly the serving routes (one file of a ticket's version, one
   asset of a ticket's artifact, the embedded runtime and the embedded kit)
   and nothing else: no guard, no session arm, no CSRF judge, no API route, no
   probe. It never reads `Cookie` and never writes `Set-Cookie`. A separate
   listener rather than Host routing on `api.port`, so the absence of every API
   route is a property of the handler chain and not of a predicate over a
   header the client controls.
2. **The API origin never renders an artifact's bytes.** The raw-file route on
   `api.port` answers `Content-Disposition: attachment`, serves text as
   `text/plain`, and carries the API's own page policy.
3. **The dashboard embeds the artifact in a sandboxed frame** whose `sandbox`
   and `allow` attributes the engine computes and the dashboard sets verbatim,
   and talks to it only over a `MessagePort` handed over after a nonce-bound
   handshake. Every live call a page makes (its datastore, presence, identity)
   goes through the parent, which calls the engine as the viewer. The artifact
   origin serves bytes and nothing else.
4. **A path-borne view ticket is the only thing that opens a version.** It
   names one principal, one artifact, one immutable version, the credential it
   was minted through and the artifact's access epoch, under the keyring, for
   an hour at most and never past the credential's own deadline.

**A real origin is granted only on a site of its own.** With
`api.artifacts.external_url` naming `{artifact}` at the start of its leftmost
label, each artifact gets its own host, but only when that host's registrable
domain differs from the dashboard's (checked against the Public Suffix List).
On the dashboard's own site, or with a plain origin, every document is served
with an opaque origin through the policy's `sandbox` directive.

**Revocation is the next document request.** A document is re-authorized on
every request against a verdict cache that the artifacts and identity appliers
invalidate the moment a share, a trash, a transfer or a credential moves; a
subresource may be served on a verdict at most thirty seconds old. A serving
node whose own artifacts or identity copy is out of service or stale decides
through a healthy peer, and answers `503` rather than serve from rows that may
still hold a revoked share. Anything that discloses fails closed.

**The script policy is strict and costs nothing at serve time.** Every
document gets a fresh nonce with `'strict-dynamic'`, by a byte substitution of
the placeholder the publish-time linter wrote into every script element; inline
handlers are admitted by hash; `object-src`, `base-uri`, `form-action` and
`frame-src` are `'none'`; `connect-src`, `img-src` and `media-src` reach only
the artifact's own origin. The frame never holds `allow-popups`: an external
link goes through the bridge, and the dashboard names the destination before it
opens it. Every third-party script a page declares (a script element's source,
a stylesheet, a module import, an import map entry) is copied into the version
at publish or refused, so a page fetches nothing from a CDN when somebody views
it unless its own script builds the request at run time.

That last clause is the residual this policy accepts. `'strict-dynamic'`
trusts every script a page's own scripts create, and a page can read its own
nonce, so a page whose author means to can load script from another site and
put in its URL whatever the viewer may read. The policy bounds what a viewer's
row can run, not what the page's author can send; what bounds the author is
that only an `editor` publishes, every version names who published it, and
the linter refuses every remote script a page states. `internal/api/pagepolicy`
says why a nonce-based policy cannot close the channel.

## Why the obvious alternatives are wrong

**Serving artifacts from the dashboard origin under a path** is the shape every
quick prototype takes, and it is the account takeover above: a path is not an
origin, and the cookie travels with every request the page makes.

**Redirecting to a presigned bucket URL** takes the bytes off the engine and
gives up every property this record exists for. The page would load on a
bucket host shared by every company, with none of the engine's headers, and a
signed URL cannot be revoked before it expires. It is refused for artifact
content on every deployment, as is a cache key that carries the ticket (no two
viewers share it) or omits the authorization step (no revocation), and an edge
that verifies tickets with a secret pushed out of the engine.

**A thirty-second authorization cache for every request**, documents included,
looks like the same bound everywhere and is not: a node cut off from the fleet
keeps serving a revoked share for as long as its rows do not move, which is
indefinitely. Documents are therefore never cached, and a stale node fails
closed; only subresources, which are useless without the document that names
them, take the thirty-second verdict.

**`'unsafe-inline'` in the script policy** keeps every model-made page working
and reopens stored script injection through the rows a viewer writes into a
page's datastore. A hash-only policy closes that and breaks most pages that
build markup at runtime. A nonce with `'strict-dynamic'`, the policy browsers'
own guidance recommends, admits the page's scripts and the scripts they load,
and nothing an attacker writes into the page's data.

## What the spikes confirm

Spike S1 records, per browser, that the two origin modes isolate as stated (a
manual procedure in
[`docs/reference/artifact-capacity.md`](../docs/reference/artifact-capacity.md)).
Spike S4 measures the linter and the strict policy over a committed corpus of
model-shaped pages and reports the breakage rate per cause. The capacity page
records what has run and what has not: S1 and the browser half of S4 are
procedures not yet run against this release, and the committed corpus holds 55
hand-written pages rather than 200 captured ones. A measurement that
contradicts a bound changes the bound in `internal/artifacts`, not this
decision.

## What this does not decide

It does not decide **who may open an artifact**, which is
[ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md), decided
again for the ticket's principal on every document request. It does not decide
**an edge cache** for subresources, which a hosted deployment adds later under
the same thirty-second bound, nor **public links**, which get a registrable
domain of their own when they are built. Doc artifacts are rendered by the
dashboard's own element-constructing renderer and never reach the artifact
origin at all.
