// Package svgsafe is the ONE drawing allowlist the engine holds an SVG to,
// wherever the drawing comes from: an inline `<svg>` in a slide (read by
// internal/artifacts/slideformat with the HTML tokenizer) and a standalone
// `.svg` file, a brand kit's logo (read here, with a strict XML front end).
// Two copies of an allowlist drift, and the copy that drifts is the one an
// attacker finds (ADR-0008), so both readers ask this package every question
// about an element, an attribute or a reference.
//
// A DRAWING REACHES NOTHING OUTSIDE ITSELF. Its elements are shapes, paths,
// text, gradients, clips, masks, markers and filter primitives; no script, no
// event attribute, no `style` attribute or `<style>` element (a stylesheet
// would style the page around it), no `foreignObject`, no `<a>`, no
// `<image>`/`feImage`, no animation (SMIL can rewrite an `href` to
// `javascript:` after the check), and every reference (`href`, `url(#…)`) is
// to an id the same drawing declares. Each attribute value is refused when it
// carries `javascript:`, `data:`, `<`, `expression` or `@import`.
//
// A VALUE IS HELD TO A GRAMMAR, NOT A DENYLIST. A presentation attribute is
// parsed as CSS, where an escape spells any word (`u\72l(` is `url(`) and a
// function reaches outside the drawing (`image-set()`, `var()`), so no value
// but an `aria-label` carries a backslash, a parenthesis appears only in
// `url(#id)` on an attribute that refers, in a transform function or in a
// colour function, and every other value is numbers, lengths, percentages,
// #hex colours and keywords (a font family list may quote its names).
//
// THE STANDALONE FRONT END IS STRICTER THAN A BROWSER, ON PURPOSE. A file is
// read with encoding/xml in strict mode with no entity table, so a DOCTYPE or
// ENTITY declaration (the billion laughs, an external entity) is refused before
// anything expands, as is every processing instruction but the XML declaration
// (`xml-stylesheet` loads a stylesheet), `xml:base`, a non-UTF-8 encoding and
// every namespace but SVG's and XLink's. Names are matched by their exact SVG
// spelling, as an XML reader does: `viewbox` is not `viewBox`.
//
// WHAT IS STORED IS THE ENGINE'S RE-SERIALISATION, never the uploaded bytes
// ([ParseFile]): the validated tree written out once, attributes in a fixed
// order, every value and text escaped, no comment, no processing instruction
// and no namespace declaration but the root's SVG one. Whatever Go's XML reader
// and a browser's would disagree about in the uploaded bytes is gone from the
// bytes anybody is served, so a parser differential has nothing to hide in.
// Nothing else is changed: a drawing outside the allowlist is REFUSED with the
// line and the fix, never silently stripped into something its author did not
// make.
//
// A DRAWING THAT EXPANDS IS BOUNDED BY WHAT IT EXPANDS TO, in every reader
// ([Drawing.Open], [Drawing.Close], [Drawing.Resolve]). A `<use>` draws
// another element of the drawing again, so ten groups of ten uses each
// referring to the group before draw ten to the tenth shapes from a hundred
// elements; a marker is drawn at every vertex of the path naming it as
// `marker-mid`; a pattern, a mask, a clip or a filter draws its subtree for
// the element naming it. A drawing is refused when a reference reaches back
// into what refers to it (a cycle), or when it would draw more than
// [MaxExpandedElements] once every reference is expanded.
//
// BUILT AT FIRST USE. Every table and pattern here — the elements and
// attributes, the attribute classes and the value grammar's expressions — is a
// [sync.OnceValue] built the first time a drawing is read, never a package
// variable built at init. The engine is one binary, and that binary is also
// every push's proc-receive hook, which runs every linked package's
// initialisation before its first statement: built at init, the tables were
// 291 KB of allocations on every push, for a process that reads no drawing.
// cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit holds every package to
// that.
package svgsafe
