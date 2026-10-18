// Package lint is the publish-time policy linter for an artifact's HTML pages
// and stylesheets.
//
// It parses each page once at publish, so the serving path never parses
// anything: the artifact origin answers a stored page by substituting a fresh
// nonce for [artifacts.NoncePlaceholder] in its bytes.
//
// It REFUSES what the serving contract forbids, every finding at once and each
// with its line: a script from another origin that is not an allowlisted CDN
// file the engine can copy in, a stylesheet from a host outside the allowlist,
// a package CDN reference without an exact version, a script importing another
// origin by a literal, a form posting to another origin, an off-origin meta
// refresh, a javascript: URL, and source text that already carries the
// placeholder. Credential shapes in what an agent publishes are refused by the
// publish pipeline, which holds the one credential grammar (internal/redact).
//
// It REWRITES every script element to carry the placeholder as the first nonce
// a browser reads, and every allowlisted reference to the version's own copy of
// the file (internal/artifactupload/vendor). Under the origin's strict policy (a
// fresh nonce with 'strict-dynamic') a nonced script runs from wherever it
// points, so it is this rewrite, not the policy, that keeps a page's scripts its
// own: which is why the rewritten page is tokenized again and every script in it
// verified before it is answered, and a page the linter cannot account for is
// refused rather than stored.
//
// It DERIVES the page's script policy: the digests of the inline event handlers
// it publishes (admitted by hash and nothing wider), whether an in-browser
// transform needs string evaluation, and whether the page compiles
// WebAssembly. A script a page BUILDS at run time is beyond any publish-time
// reading, and the strict policy deliberately lets an author's own scripts load
// further scripts.
//
// It WARNS, on success, on what is legal but almost certainly wrong: browser
// storage used for state (per viewer, and empty on an opaque origin), no
// viewport meta, a resource the origin's policy will not load, a password or
// card field.
//
// [Source] is a stored page as its author wrote it, which is what an edit is
// matched against and a read shows; [Stored] re-derives a carried page's policy
// and refuses one the linter did not write.
package lint
