package live_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// encoded is a w by h JPEG as image/jpeg writes one, in colour or grey:
// SOI, quantization tables, a baseline frame header, Huffman tables, a scan,
// EOI, and no JFIF segment.
func encoded(t *testing.T, w, h int, grey bool) []byte {
	t.Helper()
	var img image.Image
	if grey {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for x := range w {
			g.SetGray(x, x%h, color.Gray{Y: 200})
		}
		img = g
	} else {
		c := image.NewRGBA(image.Rect(0, 0, w, h))
		for x := range w {
			c.Set(x, x%h, color.RGBA{R: 200, A: 255})
		}
		img = c
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: browser.FrameQuality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// segmentAt is where the first segment of marker lies in frame, its 0xFF,
// walking the segments before the first scan.
func segmentAt(t *testing.T, frame []byte, marker byte) int {
	t.Helper()
	for i := 2; i+4 <= len(frame); {
		if frame[i] != 0xff {
			t.Fatalf("no marker at %d", i)
		}
		if frame[i+1] == marker {
			return i
		}
		if frame[i+1] == 0xda {
			break
		}
		i += 2 + (int(frame[i+2])<<8 | int(frame[i+3]))
	}
	t.Fatalf("no segment 0x%x before the scan", marker)
	return 0
}

// segment is a marker segment of marker holding body.
func segment(marker byte, body []byte) []byte {
	return append([]byte{0xff, marker, byte((len(body) + 2) >> 8), byte(len(body) + 2)}, body...)
}

// inserted is frame with what put in right after its start of image.
func inserted(frame []byte, what ...[]byte) []byte {
	return slices.Concat(append([][]byte{frame[:2]}, append(what, frame[2:])...)...)
}

// withMarker is frame with its frame header's marker made marker.
func withMarker(t *testing.T, frame []byte, marker byte) []byte {
	t.Helper()
	out := bytes.Clone(frame)
	out[segmentAt(t, out, 0xc0)+1] = marker
	return out
}

// withHeaderByte is frame with the byte at offset into its frame header's
// body made v.
func withHeaderByte(t *testing.T, frame []byte, offset int, v byte) []byte {
	t.Helper()
	out := bytes.Clone(frame)
	out[segmentAt(t, out, 0xc0)+4+offset] = v
	return out
}

// without is frame with its first segment of marker left out.
func without(t *testing.T, frame []byte, marker byte) []byte {
	t.Helper()
	i := segmentAt(t, frame, marker)
	end := i + 2 + (int(frame[i+2])<<8 | int(frame[i+3]))
	return slices.Concat(frame[:i], frame[end:])
}

// jfif is a JFIF APP0 segment, as Chromium's encoder writes one first.
var jfif = segment(0xe0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))

// filler is n bytes of comment segments, each at most a segment's bound.
func filler(n int) [][]byte {
	var out [][]byte
	for n > 0 {
		size := min(n, 60_000)
		out = append(out, segment(0xfe, bytes.Repeat([]byte{'c'}, size-4)))
		n -= size
	}
	return out
}

// THE BOX READS A FRAME'S SIZE AS THE HUB DOES: jpeg.mjs answers a size for
// exactly the frames internal/browser's CheckJPEG takes, at its size, over
// each turn image/jpeg's DecodeConfig takes: JFIF or not, baseline, extended
// or progressive, grey, past fill, restart and stray bytes, a frame header
// just within or just past 64 KiB; lossless, arithmetic, 12-bit, two-part
// and three-sampled headers, two headers, an early end, a short segment, a
// reserved marker, no start or end, a PNG. So the box drops what the hub
// would refuse. On each Node.
//
// Mutations, each red: take a lossless frame header; read past 64 KiB; stop
// at a frame header with no JFIF before it.
func TestTheBoxReadsAFramesSizeAsTheHubDoes(t *testing.T) {
	colour := encoded(t, 320, 200, false)
	grey := encoded(t, 64, 48, true)
	var pngFrame bytes.Buffer
	if err := png.Encode(&pngFrame, image.NewGray(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	jfifFirst := inserted(colour, jfif)
	sof := segmentAt(t, colour, 0xc0)
	sofEnd := sof + 2 + (int(colour[sof+2])<<8 | int(colour[sof+3]))
	// endingAt is the JFIF frame with comments between its JFIF segment and
	// the rest, so its frame header, which DecodeConfig stops at, ends at
	// offset.
	endingAt := func(offset int) []byte {
		return inserted(colour, append([][]byte{jfif}, filler(offset-len(jfif)-sofEnd)...)...)
	}
	frames := map[string][]byte{
		"a baseline frame with no JFIF":         colour,
		"a JFIF frame":                          jfifFirst,
		"an extended frame":                     withMarker(t, colour, 0xc1),
		"a progressive frame":                   withMarker(t, jfifFirst, 0xc2),
		"a grey frame":                          grey,
		"its header just within 64 KiB":         endingAt(browser.JPEGHeaderBytes),
		"its header one byte past 64 KiB":       endingAt(browser.JPEGHeaderBytes + 1),
		"fill bytes before a marker":            slices.Concat(colour[:sof], []byte{0xff, 0xff}, colour[sof:]),
		"stray bytes between segments":          slices.Concat(colour[:sof], []byte{0x00, 0x13}, colour[sof:]),
		"a restart marker between segments":     slices.Concat(colour[:sof], []byte{0xff, 0xd3}, colour[sof:]),
		"a JFIF segment, then another app0":     inserted(colour, jfif, segment(0xe0, []byte("NOTJF"))),
		"a lossless frame":                      withMarker(t, colour, 0xc3),
		"an arithmetic-coded frame":             withMarker(t, colour, 0xc9),
		"a 12-bit frame":                        withHeaderByte(t, colour, 0, 12),
		"two components":                        withHeaderByte(t, colour, 5, 2),
		"three-sampled luma":                    withHeaderByte(t, colour, 7, 0x31),
		"two frame headers":                     slices.Concat(colour[:sofEnd], colour[sof:sofEnd], colour[sofEnd:]),
		"an end of image before any header":     inserted(colour, []byte{0xff, 0xd9}),
		"a short segment":                       inserted(colour, []byte{0xff, 0xfe, 0x00, 0x01}),
		"a reserved marker":                     inserted(colour, segment(0x02, []byte{1, 2})),
		"no frame header":                       without(t, colour, 0xc0),
		"no start of image":                     colour[2:],
		"no end of image":                       colour[:len(colour)-2],
		"a PNG":                                 pngFrame.Bytes(),
		"a JFIF frame whose header is lossless": withMarker(t, jfifFirst, 0xc3),
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		took := 0
		for name, frame := range frames {
			var got struct {
				Size *struct {
					Width  int `json:"width"`
					Height int `json:"height"`
				} `json:"size"`
			}
			probe.Do(map[string]any{"op": "jpeg_size", "jpeg_b64": b64(frame)}, &got)
			meta := browser.FrameMeta{Width: 320, Height: 200}
			if got.Size != nil {
				meta = browser.FrameMeta{Width: got.Size.Width, Height: got.Size.Height}
				took++
			} else if cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame[:min(len(frame), browser.JPEGHeaderBytes)])); err == nil {
				meta = browser.FrameMeta{Width: cfg.Width, Height: cfg.Height}
			}
			err := browser.CheckJPEG(frame, meta)
			var invalid *browser.Invalid
			switch {
			case got.Size != nil && err != nil:
				t.Errorf("%s: the box reads it as %d by %d, and the hub refuses it: %v", name, got.Size.Width,
					got.Size.Height, err)
			case got.Size == nil && (!errors.As(err, &invalid) || invalid.Why != browser.WhyNotJPEG):
				t.Errorf("%s: the box reads no size, and the hub answers %v", name, err)
			}
		}
		if took < 5 || took == len(frames) {
			t.Errorf("the box took %d of %d frames: the corpus tests nothing", took, len(frames))
		}
	})
}
