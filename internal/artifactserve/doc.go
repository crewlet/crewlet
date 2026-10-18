// Package artifactserve is the artifact ORIGIN: the handler of the api.artifacts
// listener, which serves the bytes of artifact versions and nothing else, and
// the frame builder the dashboard's API mints a viewer's frame through.
//
// # An artifact runs where nothing can be stolen
//
// An artifact is arbitrary HTML, SVG and script written by a person, an agent,
// or an agent somebody prompt-injected, so it is served from a listener of its
// own that mounts exactly four routes ([Routes]): one file of a ticket's
// version, one asset of a ticket's artifact, and the embedded runtime and kit.
// No guard, no session arm, no CSRF judge, no API route, no probe, no redirect.
// It never reads the Cookie header and never writes Set-Cookie, so a same-site
// deployment whose browser sends the dashboard's cookie here is answered byte
// for byte as one that sends none. The dashboard embeds a document in a
// sandboxed frame whose sandbox and allow attributes the engine computes
// ([Frames.For]) and talks to it only through a message port; every live call
// a page makes goes through the dashboard, as the viewer.
//
// # A ticket names who may ask, never what they may see
//
// A path-borne view ticket (internal/artifactserve/ticket) names one
// principal, one immutable version, the credential it was minted through and
// the artifact's access epoch, for an hour at most. In per-artifact mode the
// request's host must be the host the ticket's artifact is served on. A
// top-level open of a ticket URL is refused, so a ticket cannot be pasted into
// a tab and presented as a standalone page. A refusal is an empty 404 that
// says nothing about whether the artifact exists; an expired ticket an empty
// 401, which tells the host to mint another.
//
// # Revocation is the next document request
//
// A document is decided on every request: the principal is re-composed from
// the identity rows (internal/api/auth composes it, as for every request the
// API serves), the artifact is read through the estate router under the
// state log's stall grace, and the authority table is asked `get_artifact`
// again. A share withdrawn, a session signed out, a person suspended, a token
// revoked, an artifact trashed or handed to another owner stops the next
// document request on any node whose copy has applied it. A subresource may
// ride its document's verdict for [artifacts.SubresourceVerdictAge], and every
// artifact movement this node's applier reports forgets the artifact's
// verdicts at once.
//
// # Anything that discloses fails closed
//
// A node whose own artifacts copy is out of service or lagging decides
// through a data node whose copy answers (the estate router's own order); a
// copy past the stall grace, an identity copy that cannot vouch for its rows,
// a copy older than the ticket it is asked about, or no data node answering
// at all is a 503 with a Retry-After, never a document served from rows that
// may still hold a revoked share.
//
// # The script policy is strict and costs nothing at serve time
//
// An author never includes the page runtime: every HTML page of a version is
// served with the runtime of the contract the version pins placed before
// everything it runs, past its doctype (a bounded scan of the page's opening,
// never a parse), under the same placeholder. Every document gets a fresh
// nonce by a byte substitution of the placeholder the publish-time linter
// wrote into every script element, so the policy
// (internal/api/pagepolicy's ArtifactDocument) admits the page's own scripts
// and the scripts they create under 'strict-dynamic', its published inline
// handlers by digest, and nothing an attacker writes into the page through a
// row. Connections, images, media, fonts and styles reach only the artifact's
// own origin; scripts the page's own scripts create are admitted from any
// origin, so the policy does not keep a page's author from sending what a
// viewer may read to another site (pagepolicy's package comment says why that
// is the price of D41 and what bounds it instead); object-src, base-uri, form-action and frame-src are 'none'; no
// Cross-Origin-Embedder-Policy is ever sent, so an untrusted page never becomes
// cross-origin isolated; the resource policy follows the origin mode.
//
// # Every byte is checked against the row that names it
//
// A file is read through the object store's checking reader, which hashes and
// counts every byte against the version's manifest entry and holds back the
// byte that completes the file until the whole matches, so a damaged object
// reads short of its length and the response is cut rather than ended as if
// whole. A range is checked for its length, and for the whole digest only
// where it reaches the file's end. Documents and the gzip of small
// compressible files are kept per node, by digest, in a bounded cache;
// everything leaves at the object store's pace, a mebibyte per write deadline.
//
// Markdown and native sheets carry no author code and are rendered by the
// dashboard, so they never reach this origin.
package artifactserve
