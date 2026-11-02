package brandformat_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

// version is a kit's files in memory, as the publish pipeline hands them to
// the format.
type version map[string][]byte

var essences = map[string]string{".json": "application/json", ".svg": "image/svg+xml",
	".png": "image/png", ".webp": "image/webp", ".woff2": "font/woff2", ".txt": "text/plain", ".md": "text/markdown"}

func (v version) Paths() []string {
	out := make([]string, 0, len(v))
	for p := range v {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (v version) Essence(p string) (string, bool) {
	if _, ok := v[p]; !ok {
		return "", false
	}
	return essences[path.Ext(p)], true
}

func (v version) Read(p string) ([]byte, error) {
	b, ok := v[p]
	if !ok {
		return nil, errors.New("no such file")
	}
	return b, nil
}

// svgLogo is a 4:1 wordmark.
const svgLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 50"><rect width="200" height="50" fill="#2450e6"/></svg>`

// pngOf is a w by h PNG, with every chunk in extra inserted before its IEND.
func pngOf(t testing.TB, w, h int, extra ...[]byte) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, 0, color.NRGBA{R: 0x24, G: 0x50, B: 0xe6, A: 0xff})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	iend := len(b) - 12
	return slices.Concat(b[:iend], bytes.Join(extra, nil), b[iend:])
}

// chunk is one PNG chunk with its CRC.
func chunk(kind string, data []byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(len(data)))
	out.WriteString(kind)
	out.Write(data)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	return out.Bytes()
}

// riff is a RIFF WEBP container around chunks, each a FourCC and its data.
func riff(chunks ...[2]string) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	for _, c := range chunks {
		body.WriteString(c[0])
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(c[1])))
		body.WriteString(c[1])
		if len(c[1])%2 == 1 {
			body.WriteByte(0)
		}
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// vp8l is a lossless image chunk's data of w by h: the signature and the size
// header, then a few bytes of bitstream the format never decodes.
func vp8l(w, h int) string {
	bits := uint32(w-1) | uint32(h-1)<<14
	var b bytes.Buffer
	b.WriteByte(0x2f)
	_ = binary.Write(&b, binary.LittleEndian, bits)
	b.WriteString("\x00\x00\x00")
	return b.String()
}

// vp8x is an extended header's data for a w by h canvas with flags.
func vp8x(flags byte, w, h int) string {
	b := []byte{flags, 0, 0, 0}
	for _, v := range []int{w - 1, h - 1} {
		b = append(b, byte(v), byte(v>>8), byte(v>>16))
	}
	return string(b)
}

// woff2Of is a WOFF2 header of numTables tables decoding to sfnt bytes, with
// pad bytes of tables after it.
func woff2Of(tables uint16, sfnt uint32, pad int) []byte {
	var b bytes.Buffer
	b.WriteString("wOF2")
	_ = binary.Write(&b, binary.BigEndian, uint32(0x00010000))
	_ = binary.Write(&b, binary.BigEndian, uint32(48+pad))
	_ = binary.Write(&b, binary.BigEndian, tables)
	_ = binary.Write(&b, binary.BigEndian, uint16(0))
	_ = binary.Write(&b, binary.BigEndian, sfnt)
	_ = binary.Write(&b, binary.BigEndian, uint32(pad))
	b.Write(make([]byte, 24))
	b.Write(bytes.Repeat([]byte{0x5a}, pad))
	return b.Bytes()
}

// index is an honest brand.json.
const index = `{
  "v": 1,
  "name": "Acme",
  "tagline": "Tools for people who make things",
  "logos": {"primary": "logos/primary.svg", "on_dark": "logos/on-dark.png", "mark": "logos/mark.webp"},
  "colors": {"primary": "#2450e6", "secondary": "#9584ff", "text": "#121625",
    "light_ground": "#ffffff", "dark_ground": "#0d0f16"},
  "type": {
    "heading": {"kit": "geist"},
    "body": {"family": "Acme Sans", "licensed": true,
      "files": [{"path": "fonts/body-0.woff2", "weight": 400, "style": "normal"},
                {"path": "fonts/body-1.woff2", "weight": 700, "style": "normal"}]}
  },
  "voice": {"summary": "Plain, warm and exact.\nNever breathless.",
    "write_like": ["We fixed it on Tuesday."], "avoid": ["Revolutionary"]}
}`

// kit is an honest kit's version.
func kit(t testing.TB) version {
	t.Helper()
	return version{
		brandformat.EntryPath: []byte(index),
		"logos/primary.svg":   []byte(svgLogo),
		"logos/on-dark.png":   pngOf(t, 300, 100),
		"logos/mark.webp":     riff([2]string{"VP8L", vp8l(64, 64)}),
		"fonts/body-0.woff2":  woff2Of(12, 40000, 64),
		"fonts/body-1.woff2":  woff2Of(12, 41000, 64),
	}
}

// with is the kit with brand.json's text replaced: old by new.
func with(t testing.TB, old, replacement string) version {
	t.Helper()
	v := kit(t)
	if !strings.Contains(index, old) {
		t.Fatalf("the honest index holds no %q", old)
	}
	v[brandformat.EntryPath] = []byte(strings.Replace(index, old, replacement, 1))
	return v
}

// refusal is the findings a read answered, failing a read that succeeded.
func refusal(t testing.TB, err error) []brandformat.Finding {
	t.Helper()
	var refused *brandformat.Refused
	if !errors.As(err, &refused) {
		t.Fatalf("the kit was read (err %v), want it refused", err)
	}
	return refused.Findings
}

// readRefused is the findings ReadKit answered for src, failing a read that
// succeeded.
func readRefused(t testing.TB, src version) []brandformat.Finding {
	t.Helper()
	_, err := brandformat.ReadKit(src)
	return refusal(t, err)
}

// names reports whether a finding of rule says want.
func names(findings []brandformat.Finding, rule brandformat.Rule, want string) bool {
	for _, f := range findings {
		if f.Rule == rule && strings.Contains(f.String(), want) {
			return true
		}
	}
	return false
}

// adoptable is b as an adoptable brand, failing a brand that is not.
func adoptable(t testing.TB, b brandformat.Brand) brandformat.Adoptable {
	t.Helper()
	a, ok := brandformat.AsAdoptable(b)
	if !ok {
		t.Fatalf("the fixture brand leaves out %v, which an adopted brand holds", brandformat.MissingToAdopt(b))
	}
	return a
}
