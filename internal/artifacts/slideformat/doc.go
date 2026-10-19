// Package slideformat is the slide format a `slides` artifact holds: Claude's
// slide format (D56), checked at publish and handed to the dashboard as one
// canonical tree it draws, presents and prints without parsing any markup.
//
// THE FILES. A deck is a version holding `deck.json`, the index (title, the
// slide order, the outline's sections, the cover, the typefaces), and one
// `slides/<id>.html` per slide holding exactly one `<section id="<id>">` on a
// fixed 1920 by 1080 stage. Images and font files are files of the same
// version, named by their path from the version's root. Claude's own decks
// keep the same files under `project/`; a Crewlet version is already the
// deck's own namespace, so the prefix is dropped.
//
// A CLOSED SUBSET, REFUSED RATHER THAN DROPPED. Every element, attribute and
// inline style property a slide may carry is in this package's tables, each
// style value held to its grammar and range. A slide outside the subset is
// refused at publish with a [Finding] naming the file, the line and the rule,
// so an author fixes what they wrote instead of learning on read that part of
// it never shows. Claude's runtime drops what it does not support; Crewlet
// refuses it, because a deck that publishes is a deck that draws.
//
// NOTHING HERE CAN BECOME SCRIPT OR A REQUEST. No element runs code (no
// script, no event attribute, no embed, no `foreignObject`), no style value
// may name a URL (`url()`, `image-set()`, `@import`, `var()`, `expression`),
// an image or a font is a file of the version, an inline SVG reaches only its
// own `#id`s, and a link is `https:` only. The dashboard draws the canonical
// tree with an element-constructing renderer (`dashboard/src/lib/slides.ts`)
// that holds the same allowlists again, so a tree this package would not have
// produced is still never drawn as anything but text.
//
// PURE. It reads no store and holds no clock: the publish pipeline hands it
// the files' bytes and a lookup of the version's other files, and the read
// routes call it on the same bytes to answer the dashboard.
package slideformat
