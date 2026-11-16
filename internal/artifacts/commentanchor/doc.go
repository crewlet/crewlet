// Package commentanchor is where a comment on an artifact points, and how that
// place is found again in each version: the authority of ADR-0153.
//
// # An anchor is content, and its place is found at read
//
// A thread stores the [Anchor] it was made with and the version it was made
// on, and nothing else about where it is. Its place on any version is FOUND by
// [Place], which looks for the anchor's content in that version's [Index]: a
// name the page declares, a slide's element by its path and else by its tag
// and text, an artboard and a point inside it. A publish therefore moves every
// thread at once with no write, and a thread whose content is gone is answered
// [StateOutdated], kept and still answerable, never dropped.
//
// # One table says what each kind's comments may point at
//
// [Accepts] is keyed by every artifact kind: `html` takes a page's declared
// name or a point on it, `slides` an element of a slide, `design` a point on
// an artboard, and every other kind the whole version alone, its ADR-0042
// shape kept reserved. A kind added to the artifacts table without a row here
// fails the build's tests, so a new kind states its anchors when it lands.
//
// # The index is a version's, and a version never changes
//
// An [Index] is computed from one version's files ([IndexHTML]), its deck
// ([IndexSlides]) or its canvas ([IndexCanvas]). It holds no artifact id and
// no clock, so a caller may cache it by artifact and version for as long as it
// likes; it is never mutated after it is built and is safe to share.
//
// # The page declares, the host decides
//
// A page names a place with `data-crewlet-anchor` (which the runtime's
// `crewlet.comments.anchor(name, element)` sets) or the literal call itself.
// The index reads those declarations from the files; nothing a page reports at
// run time decides whether a name exists.
package commentanchor
