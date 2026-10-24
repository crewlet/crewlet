package brandformat

import (
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/svgsafe"
)

// ---- the assets ----------------------------------------------------------- //
//
// A LOGO OR A FACE IS STORED IN THE FORM THE ENGINE WRITES, never as uploaded:
// an SVG re-serialised by internal/artifacts/svgsafe, a PNG without the
// ancillary chunks that carry metadata, a WebP without its EXIF and XMP, a
// WOFF2 as it came once its header is inside the bounds. [Sanitize] is that
// form; the publish pipeline stores it, and an adoption sanitizes every file it
// copies again, so the bytes the public brand route serves are always this
// build's form whatever build published the kit. Anything outside a rule is
// refused, naming the file and the fix: only metadata is ever dropped.

// asset is one checked logo or face: its stored form and, for a logo, its size.
type asset struct {
	out           []byte
	width, height float64
}

// ratio is a logo's width over its height, zero for a face.
func (a asset) ratio() float64 {
	if a.height <= 0 {
		return 0
	}
	return a.width / a.height
}

// Sanitize is the form a logo or a face's file is stored in, read from data,
// the file at path served as essence: one of the logo essences (SVG, PNG,
// WebP) or WOFF2. A file outside its rule is a *[Refused] naming the path and
// the fix. Sanitize of its own answer is that answer.
func Sanitize(path, essence string, data []byte) ([]byte, error) {
	a, findings := inspect(path, essence, data)
	if len(findings) > 0 {
		return nil, &Refused{Findings: findings}
	}
	return a.out, nil
}

// inspect checks one file against the rule of its essence.
func inspect(path, essence string, data []byte) (asset, []Finding) {
	refuse := func(format string, args ...any) (asset, []Finding) {
		return asset{}, []Finding{{Path: path, Rule: RuleAsset, Message: fmt.Sprintf(format, args...)}}
	}
	switch essence {
	case "image/svg+xml":
		c, err := svgsafe.ParseFile(data, MaxSVGBytes)
		var refused *svgsafe.Refused
		if errors.As(err, &refused) {
			out := make([]Finding, len(refused.Findings))
			for i, f := range refused.Findings {
				out[i] = Finding{Path: path, Line: f.Line, Rule: RuleAsset, Message: f.Message}
			}
			return asset{}, out
		}
		if err != nil {
			return refuse("the drawing cannot be read: %v", err)
		}
		return asset{out: c.Bytes, width: c.Width, height: c.Height}, nil
	case "image/png":
		out, w, h, err := sanitizePNG(data)
		if err != nil {
			return refuse("%v", err)
		}
		if msg := rasterShape("PNG", w, h); msg != "" {
			return refuse("%s", msg)
		}
		return asset{out: out, width: float64(w), height: float64(h)}, nil
	case "image/webp":
		out, w, h, err := sanitizeWebP(data)
		if err != nil {
			return refuse("%v", err)
		}
		if msg := rasterShape("WebP", w, h); msg != "" {
			return refuse("%s", msg)
		}
		return asset{out: out, width: float64(w), height: float64(h)}, nil
	case fontEssence:
		if err := checkWOFF2(data); err != nil {
			return refuse("%v", err)
		}
		return asset{out: data}, nil
	}
	return refuse("%s is not a logo (SVG, PNG or WebP) or a face (WOFF2)", essence)
}

// MaxBytes is the most bytes a file of essence may be, false for an essence a
// kit holds no file of: what a reader that knows a file's size holds it to
// before reading any of it ([Sizer]).
func MaxBytes(essence string) (int, bool) {
	switch essence {
	case "image/svg+xml":
		return MaxSVGBytes, true
	case "image/png", "image/webp":
		return MaxImageBytes, true
	case fontEssence:
		return MaxFontFileBytes, true
	}
	return 0, false
}

// TooLarge is why a file of essence and size bytes is past its bound, in the
// words its own check says it in, or "" when it is inside it.
func TooLarge(essence string, size int64) string {
	limit, ok := MaxBytes(essence)
	if !ok || size <= int64(limit) {
		return ""
	}
	switch essence {
	case "image/svg+xml":
		return fmt.Sprintf("the file is %d bytes and a drawing may be at most %d: simplify its paths "+
			"or export it at a lower precision", size, limit)
	case fontEssence:
		return fmt.Sprintf("the font is %d bytes and a face's file may be at most %d: subset it", size, limit)
	case "image/png":
		return fmt.Sprintf("the PNG is %d bytes and a logo may be at most %d: export it smaller", size, limit)
	}
	return fmt.Sprintf("the WebP is %d bytes and a logo may be at most %d: export it smaller", size, limit)
}

// rasterShape is why a raster logo of w by h pixels is not a logo's shape, or
// "" when it is: between 1:8 and 8:1, the bound a drawing's viewBox is held to
// ([svgsafe.MinAspect], [svgsafe.MaxAspect]), so a 4096 by 1 strip is no more
// a logo as a PNG than as an SVG.
func rasterShape(format string, w, h int) string {
	if aspect := float64(w) / float64(h); aspect < svgsafe.MinAspect || aspect > svgsafe.MaxAspect {
		return fmt.Sprintf("the %s is %dx%d, %.2f times as wide as it is tall, and a logo is between "+
			"1:8 and 8:1: crop it to the logo", format, w, h, aspect)
	}
	return ""
}

// ---- what the brand serves ------------------------------------------------ //

// ServedExts are the extensions the public brand route serves a file under,
// `/_crewlet/brand/<sha256>.<ext>`: the generated stylesheet, the three logo
// formats and WOFF2. Nothing else is ever served there.
var ServedExts = []string{"css", "svg", "png", "webp", "woff2"}

// served are the essences of [ServedExts], in the same order.
var served = []string{"text/css", "image/svg+xml", "image/png", "image/webp", fontEssence}

// Ext is the extension a file of essence is served under, false for an
// essence the brand never serves.
func Ext(essence string) (string, bool) {
	i := slices.Index(served, essence)
	if i < 0 {
		return "", false
	}
	return ServedExts[i], true
}

// ContentType is the Content-Type a file served under ext carries: the media
// table's own (internal/artifacts/mediatype), never a stored value; false for
// an extension the brand never serves.
func ContentType(ext string) (string, bool) {
	if !slices.Contains(ServedExts, ext) {
		return "", false
	}
	t, ok := mediatype.For("brand." + ext)
	if !ok {
		return "", false
	}
	return t.Served, true
}
