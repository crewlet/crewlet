package brandformat_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image/png"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

func sanitize(t *testing.T, path, essence string, data []byte) ([]byte, string) {
	t.Helper()
	out, err := brandformat.Sanitize(path, essence, data)
	if err != nil {
		return nil, err.Error()
	}
	return out, ""
}

// A PNG KEEPS ONLY WHAT DRAWS IT: a text chunk and an EXIF chunk are dropped,
// gamma kept, the stored PNG still decodes to the same size, and its stored
// form sanitized again is itself.
//
// Mutation: keep every ancillary chunk and the tEXt survives.
func TestAPNGKeepsOnlyWhatDrawsIt(t *testing.T) {
	t.Parallel()
	in := pngOf(t, 40, 20, chunk("tEXt", []byte("Author\x00Jane Doe")), chunk("eXIf", []byte("MM\x00*GPS")),
		chunk("gAMA", []byte{0, 0, 0xb1, 0x8f}))
	out, msg := sanitize(t, "logo.png", "image/png", in)
	if msg != "" {
		t.Fatalf("an honest PNG was refused: %s", msg)
	}
	for _, gone := range []string{"tEXt", "Jane Doe", "eXIf", "GPS"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("the stored PNG keeps %s", gone)
		}
	}
	if !bytes.Contains(out, []byte("gAMA")) {
		t.Error("the stored PNG lost its gamma")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 40 || cfg.Height != 20 {
		t.Errorf("the stored PNG decodes as %+v (%v)", cfg, err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("the stored PNG does not decode: %v", err)
	}
	if again, _ := sanitize(t, "logo.png", "image/png", out); !bytes.Equal(again, out) {
		t.Error("the stored PNG is not a fixed point")
	}
}

// pngWith is a 4 by 4 grey PNG whose image stream is raw, with the header's
// size and interlace given, so a test can make the stream disagree with it.
func pngWith(width, height uint32, interlace byte, raw []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	_, _ = w.Write(raw)
	_ = w.Close()
	var ihdr bytes.Buffer
	_ = binary.Write(&ihdr, binary.BigEndian, width)
	_ = binary.Write(&ihdr, binary.BigEndian, height)
	ihdr.Write([]byte{8, 0, 0, 0, interlace})
	return bytes.Join([][]byte{[]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr.Bytes()),
		chunk("IDAT", z.Bytes()), chunk("IEND", nil)}, nil)
}

// A PNG OUTSIDE ITS RULES IS REFUSED, NEVER DRAWN: an animation, a damaged
// chunk, a size past the bounds (refused from the header, before anything
// inflates), a stream that does not hold the rows its header promises, a
// filter PNG does not define, a critical chunk no decoder knows, bytes after
// the end.
//
// Mutation: skip the row count and the short stream is stored.
func TestAPNGOutsideItsRulesIsRefused(t *testing.T) {
	t.Parallel()
	honest := pngOf(t, 8, 8)
	damaged := bytes.Clone(honest)
	damaged[len(damaged)-20] ^= 0xff
	row := append([]byte{0}, make([]byte, 4)...)
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"apng":           {pngOf(t, 8, 8, chunk("acTL", make([]byte, 8))), "animated"},
		"bad crc":        {damaged, "CRC"},
		"too wide":       {pngWith(5000, 1, 0, nil), "4096 pixels a side"},
		"too many":       {pngWith(4096, 4097, 0, nil), "4096 pixels a side"},
		"short stream":   {pngWith(4, 4, 0, bytes.Repeat(row, 3)), "fewer rows"},
		"long stream":    {pngWith(4, 4, 0, bytes.Repeat(row, 5)), "more than its header"},
		"bad filter":     {pngWith(4, 4, 0, append(bytes.Repeat(row, 3), 9, 0, 0, 0, 0)), "filter 9"},
		"interlaced":     {pngWith(4, 4, 1, bytes.Repeat(row, 4)), "fewer rows"},
		"unknown chunk":  {pngOf(t, 8, 8, chunk("ZZZZ", []byte("x"))), "critical chunk"},
		"after iend":     {append(bytes.Clone(honest), 0), "after its IEND"},
		"not png":        {[]byte("GIF89a"), "not a PNG"},
		"too many bytes": {make([]byte, brandformat.MaxImageBytes+1), "at most"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, msg := sanitize(t, "logo.png", "image/png", c.data)
			if !strings.Contains(msg, c.want) || !strings.Contains(msg, "logo.png") {
				t.Errorf("the refusal says %q, want it to name logo.png and %q", msg, c.want)
			}
		})
	}
	// AN INTERLACED PNG HOLDING ITS SEVEN PASSES IS A PNG.
	var passes []byte
	// A 4 BY 4 IMAGE'S PASSES: the second and third are empty (PNG 8.2).
	for _, size := range [][2]int{{1, 1}, {1, 1}, {2, 1}, {2, 2}, {4, 2}} {
		for range size[1] {
			passes = append(passes, make([]byte, 1+size[0])...)
		}
	}
	if _, msg := sanitize(t, "logo.png", "image/png", pngWith(4, 4, 1, passes)); msg != "" {
		t.Errorf("an interlaced PNG holding its seven passes was refused: %s", msg)
	}
}

// A WEBP DROPS ITS METADATA: the EXIF and XMP chunks go, the extended
// header's flags and the container's size are written to match, and the
// stored form sanitized again is itself.
//
// Mutation: leave the flags as they were and a reader looks for an EXIF that
// is not there.
func TestAWebPDropsItsMetadata(t *testing.T) {
	t.Parallel()
	in := riff([2]string{"VP8X", vp8x(0x08|0x04|0x10, 64, 32)}, [2]string{"VP8L", vp8l(64, 32)},
		[2]string{"EXIF", "MM\x00*GPS 51.5N"}, [2]string{"XMP ", "<x:xmpmeta>Jane</x:xmpmeta>"})
	out, msg := sanitize(t, "mark.webp", "image/webp", in)
	if msg != "" {
		t.Fatalf("an honest WebP was refused: %s", msg)
	}
	for _, gone := range []string{"EXIF", "GPS", "XMP ", "Jane"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("the stored WebP keeps %s", gone)
		}
	}
	if flags := out[20]; flags&(0x08|0x04) != 0 || flags&0x10 == 0 {
		t.Errorf("the stored WebP's flags are %#x, want EXIF and XMP cleared and alpha kept", flags)
	}
	if size := binary.LittleEndian.Uint32(out[4:8]); int(size)+8 != len(out) {
		t.Errorf("the stored WebP's container says %d bytes and holds %d", size+8, len(out))
	}
	if again, _ := sanitize(t, "mark.webp", "image/webp", out); !bytes.Equal(again, out) {
		t.Error("the stored WebP is not a fixed point")
	}
	lossy := riff([2]string{"VP8 ", "\x10\x02\x00\x9d\x01\x2a\x50\x00\x28\x00"})
	if _, msg := sanitize(t, "mark.webp", "image/webp", lossy); msg != "" {
		t.Errorf("an honest lossy WebP was refused: %s", msg)
	}
}

// A WEBP OUTSIDE ITS RULES IS REFUSED: an animation (a chunk or the flag), a
// chunk a still image does not define, a container that lies about its size,
// a size past the bounds, a canvas that disagrees with its image.
//
// Mutation: skip the ANIM case and the animation is stored.
func TestAWebPOutsideItsRulesIsRefused(t *testing.T) {
	t.Parallel()
	liar := riff([2]string{"VP8L", vp8l(8, 8)})
	binary.LittleEndian.PutUint32(liar[4:8], 999)
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"anim chunk":  {riff([2]string{"VP8X", vp8x(0, 8, 8)}, [2]string{"ANIM", "xxxxxx"}), "animated"},
		"anim flag":   {riff([2]string{"VP8X", vp8x(0x02, 8, 8)}, [2]string{"VP8L", vp8l(8, 8)}), "animated"},
		"unknown":     {riff([2]string{"VP8L", vp8l(8, 8)}, [2]string{"ZZZZ", "x"}), "does not define"},
		"size lie":    {liar, "damaged"},
		"too wide":    {riff([2]string{"VP8L", vp8l(5000, 8)}), "4096 pixels a side"},
		"canvas":      {riff([2]string{"VP8X", vp8x(0, 9, 8)}, [2]string{"VP8L", vp8l(8, 8)}), "canvas"},
		"two images":  {riff([2]string{"VP8L", vp8l(8, 8)}, [2]string{"VP8L", vp8l(8, 8)}), "2 images"},
		"no header":   {riff([2]string{"VP8L", "\x00\x00\x00\x00\x00"}), "VP8L header"},
		"not riff":    {[]byte("RIFX....WEBP"), "not a WebP"},
		"alpha alone": {riff([2]string{"ALPH", "x"}, [2]string{"VP8L", vp8l(8, 8)}), "extended header"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, msg := sanitize(t, "mark.webp", "image/webp", c.data)
			if !strings.Contains(msg, c.want) {
				t.Errorf("the refusal says %q, want it to name %q", msg, c.want)
			}
		})
	}
}

// A FACE IS HELD BY ITS HEADER and stored as it came: a signature, a length
// that is the file's, a table count and a decoded size inside the bounds.
//
// Mutation: drop the decoded-size bound and the 9 MiB face is stored.
func TestAFaceIsHeldByItsHeader(t *testing.T) {
	t.Parallel()
	honest := woff2Of(12, 40000, 64)
	out, msg := sanitize(t, "body.woff2", "font/woff2", honest)
	if msg != "" || !bytes.Equal(out, honest) {
		t.Fatalf("an honest face was refused or changed: %s", msg)
	}
	lies := bytes.Clone(honest)
	binary.BigEndian.PutUint32(lies[8:12], 4)
	reserved := bytes.Clone(honest)
	reserved[15] = 1
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"not woff2":  {[]byte("wOFF" + strings.Repeat("\x00", 60)), "not a WOFF2"},
		"length":     {lies, "damaged"},
		"no tables":  {woff2Of(0, 40000, 64), "1 to 64"},
		"too many":   {woff2Of(65, 40000, 64), "1 to 64"},
		"too large":  {woff2Of(12, brandformat.MaxFontSFNTBytes+1, 64), "subset it"},
		"reserved":   {reserved, "reserved"},
		"file bound": {make([]byte, brandformat.MaxFontFileBytes+1), "subset it"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, msg := sanitize(t, "body.woff2", "font/woff2", c.data); !strings.Contains(msg, c.want) {
				t.Errorf("the refusal says %q, want it to name %q", msg, c.want)
			}
		})
	}
}

// AN SVG IS STORED AS svgsafe WRITES IT: the comment gone, the stored form a
// fixed point, a script refused naming the file and its line.
//
// Mutation: answer the uploaded bytes for an SVG and the comment survives.
func TestAnSVGIsStoredAsSvgsafeWritesIt(t *testing.T) {
	t.Parallel()
	out, msg := sanitize(t, "logo.svg", "image/svg+xml", []byte("<!-- made by hand -->"+svgLogo))
	if msg != "" {
		t.Fatalf("an honest SVG was refused: %s", msg)
	}
	if bytes.Contains(out, []byte("made by hand")) {
		t.Error("the stored SVG keeps its comment")
	}
	if again, _ := sanitize(t, "logo.svg", "image/svg+xml", out); !bytes.Equal(again, out) {
		t.Error("the stored SVG is not a fixed point")
	}
	_, err := brandformat.Sanitize("logo.svg", "image/svg+xml",
		[]byte("<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 9 9\">\n<script/>\n</svg>"))
	findings := refusal(t, err)
	if len(findings) != 1 || findings[0].Path != "logo.svg" || findings[0].Line != 2 ||
		findings[0].Rule != brandformat.RuleAsset {
		t.Errorf("the refusal is %+v, want one asset finding at logo.svg line 2", findings)
	}
	if _, msg := sanitize(t, "x.txt", "text/plain", []byte("x")); !strings.Contains(msg, "not a logo") {
		t.Errorf("a text file was sanitized: %q", msg)
	}
}

// THE BRAND SERVES FIVE EXTENSIONS, each with the media table's own type, and
// no other: the public route's allowlist is this list.
//
// Mutation: add "html" to ServedExts and ContentType answers a page type.
func TestTheBrandServesFiveExtensions(t *testing.T) {
	t.Parallel()
	want := map[string]string{"css": "text/css; charset=utf-8", "svg": "image/svg+xml", "png": "image/png",
		"webp": "image/webp", "woff2": "font/woff2"}
	if len(brandformat.ServedExts) != len(want) {
		t.Errorf("the brand serves %v", brandformat.ServedExts)
	}
	for ext, typ := range want {
		got, ok := brandformat.ContentType(ext)
		if !ok || got != typ {
			t.Errorf("ContentType(%s) = %q, %v; want %q", ext, got, ok, typ)
		}
		essence, _, _ := strings.Cut(typ, ";")
		if back, ok := brandformat.Ext(essence); !ok || back != ext {
			t.Errorf("Ext(%s) = %q, %v; want %s", essence, back, ok, ext)
		}
	}
	for _, other := range []string{"html", "js", "json", "SVG", ""} {
		if _, ok := brandformat.ContentType(other); ok {
			t.Errorf("the brand serves %q", other)
		}
	}
}
