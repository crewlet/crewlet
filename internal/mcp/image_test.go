package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"testing"
)

// pictures is one small picture in each type an answer may carry, encoded by
// the standard library, with WebP's container written out by hand (Go encodes
// none): its RIFF header and a VP8L chunk.
func pictures(t *testing.T) map[string][]byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var p, j bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	webp := append([]byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f"), make([]byte, 13)...)
	return map[string][]byte{ImagePNG: p.Bytes(), ImageJPEG: j.Bytes(), ImageWebP: webp}
}

// AN IMAGE'S TYPE IS WHAT ITS BYTES SAY. Each type an answer may carry is
// read from its file's own signature, so a tool never names a type the bytes
// do not bear out, and an answer carrying it passes the check.
//
// Mutation: read the type from anything but the bytes, or drop a signature,
// and that type is refused or named wrongly.
func TestAnImagesTypeIsReadFromItsBytes(t *testing.T) {
	t.Parallel()
	for want, data := range pictures(t) {
		img, err := NewImage(data)
		if err != nil {
			t.Fatalf("NewImage(%s): %v", want, err)
		}
		if img.MIMEType != want || !bytes.Equal(img.Data, data) {
			t.Errorf("NewImage(%s) = %q of %d bytes, want %s of %d", want, img.MIMEType,
				len(img.Data), want, len(data))
		}
		if err := CheckImages("Ana's crop of the chart.", []Image{img}); err != nil {
			t.Errorf("an answer carrying one %s was refused: %v", want, err)
		}
	}
}

// ANYTHING BUT A SMALL PNG, JPEG OR WEBP IS REFUSED, NEVER SENT. Bytes of no
// type sent (text, a GIF), an image past its bound, a type the bytes do not
// bear out, more images than an answer carries and an image with no text
// beside it are each refused with why, under [ErrImageRefused].
//
// Mutation: drop any one check, and its case passes the check.
func TestAnImageOutsideTheRulesIsRefused(t *testing.T) {
	t.Parallel()
	pics := pictures(t)
	pngImg := Image{MIMEType: ImagePNG, Data: pics[ImagePNG]}
	oversized := append(append([]byte(nil), pics[ImagePNG]...), make([]byte, MaxImageBytes)...)
	for name, tc := range map[string]struct {
		output string
		images []Image
		says   string
	}{
		"text, not an image": {"a crop", []Image{{MIMEType: ImagePNG, Data: []byte("not a picture at all")}},
			"not a PNG, a JPEG or a WebP"},
		"a GIF": {"a crop", []Image{{MIMEType: "image/gif", Data: []byte("GIF89a\x01\x00\x01\x00")}},
			"not a PNG, a JPEG or a WebP"},
		"past its bound": {"a crop", []Image{{MIMEType: ImagePNG, Data: oversized}},
			"past the 262144"},
		"a type its bytes deny": {"a crop", []Image{{MIMEType: ImageJPEG, Data: pics[ImagePNG]}},
			`says "image/jpeg" and its bytes are image/png`},
		"more than an answer carries": {"three crops", []Image{pngImg, pngImg, pngImg},
			"carries 3 images, and at most 2"},
		"no text beside it": {"  \n", []Image{pngImg}, "no text saying whose picture"},
	} {
		err := CheckImages(tc.output, tc.images)
		if !errors.Is(err, ErrImageRefused) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: CheckImages = %v, want a refusal saying %q", name, err, tc.says)
		}
	}
	if _, err := NewImage(oversized); !errors.Is(err, ErrImageRefused) {
		t.Errorf("NewImage of %d bytes = %v, want a refusal", len(oversized), err)
	}
	if _, err := NewImage([]byte("plain text")); !errors.Is(err, ErrImageRefused) {
		t.Errorf("NewImage of text = %v, want a refusal", err)
	}
}

// A RECORD KEEPS AN IMAGE'S TYPE AND SIZE, NEVER ITS BYTES. What every record
// of a call keeps of the images its answer carried encodes to a few bytes
// whatever the picture weighs.
//
// Mutation: carry the data into the record, and its encoding holds the bytes.
func TestARecordKeepsAnImagesTypeAndSizeAlone(t *testing.T) {
	t.Parallel()
	data := pictures(t)[ImagePNG]
	records := ImageRecords([]Image{{MIMEType: ImagePNG, Data: data}})
	if len(records) != 1 || records[0] != (ImageRecord{MIMEType: ImagePNG, Bytes: len(data)}) {
		t.Fatalf("ImageRecords = %+v, want one PNG of %d bytes", records, len(data))
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"mime_type":"image/png","bytes":` + strconv.Itoa(len(data)) + `}]`
	if string(raw) != want {
		t.Errorf("a record encodes as %s, want %s", raw, want)
	}
	if ImageRecords(nil) != nil {
		t.Error("no images are recorded as an empty list, want none")
	}
}
