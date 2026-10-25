package slideformat

import "github.com/crewlet/crewlet/internal/artifacts/svgsafe"

// ---- inline SVG ----------------------------------------------------------- //
//
// A chart or a diagram is one `<svg>`: shapes, paths, gradients, clips, masks,
// markers and filter primitives, drawn as one opaque graphic. It reaches
// nothing outside itself, by internal/artifacts/svgsafe's allowlist, the one a
// brand logo is held to as well: no script, no event attribute, no
// `foreignObject`, no `<style>` (an inline stylesheet would style the whole
// dashboard), no `style` attribute, and every reference (`href`, `url(#…)`) is
// to an id of the same drawing. The dashboard gives each drawing's ids a
// prefix of its own when it draws them, so two slides, or one slide drawn
// twice as a thumbnail, never resolve each other's gradients.

// MaxSVGBytes bounds one inline drawing's source, as Claude's format does
// (52 KB): a drawing past it is a picture, which belongs in an image file.
const MaxSVGBytes = 52 << 10

// svgDrawing is one inline drawing as a slide reads it: the source offset of
// its `<svg`, which [MaxSVGBytes] is measured from, and the drawing svgsafe
// checks.
type svgDrawing struct {
	start int
	*svgsafe.Drawing
}
