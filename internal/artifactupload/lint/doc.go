// Package lint is the publish-time policy linter for HTML artifacts.
//
// It parses each HTML file once at publish, so the serving path never parses
// anything. It REFUSES what the serving contract forbids: a script or
// stylesheet from a host outside the allowlist, a package CDN URL without an
// exact version, a form posting to another origin, an off-origin meta refresh,
// and, for an agent, any credential shape in the content. It REWRITES every
// script element to carry the nonce placeholder the serving origin replaces per
// response, and records the digests of the inline event handlers a page
// publishes, which is what lets the artifact origin run a strict script policy
// (a fresh nonce with strict-dynamic) without breaking model-made pages. It
// WARNS, on success, on what is legal but wrong: browser storage used for shared
// state, no dark-mode tokens, no viewport meta.
package lint
