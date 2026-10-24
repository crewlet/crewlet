package brandformat

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// ---- PNG ------------------------------------------------------------------ //
//
// A PNG LOGO IS READ CHUNK BY CHUNK, NEVER DECODED INTO PIXELS. Its header is
// held to the size bounds before anything is inflated (a decompression bomb
// is a few kilobytes of IDAT that inflate to gigabytes), and then the whole
// image stream is inflated, row by row through one row's buffer, to exactly
// the bytes its header promises, each row's filter byte checked: a full decode
// of the structure at the memory of one row, rather than of a 64 MiB bitmap.
//
// Kept: the critical chunks, and the ancillary ones that change how the image
// looks (transparency, gamma, chromaticities, the colour space and an ICC
// profile of at most 64 KiB, the pixel density). Dropped: every other
// ancillary chunk, which is where text, EXIF, timestamps and editor state
// live. Refused: an unknown critical chunk (no decoder would draw it), an
// animated PNG (acTL, fcTL, fdAT: a logo is one frame), a bad CRC, anything
// after IEND.

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// pngKept are the ancillary chunks a stored PNG keeps.
var pngKept = map[string]bool{"tRNS": true, "gAMA": true, "cHRM": true, "sRGB": true, "iCCP": true, "pHYs": true}

// pngAnimated are the chunks of an animated PNG.
var pngAnimated = map[string]bool{"acTL": true, "fcTL": true, "fdAT": true}

// maxICCPBytes bounds the colour profile a stored PNG keeps; a larger one is
// dropped, the image then drawn as sRGB.
const maxICCPBytes = 64 << 10

// pngDepths are the bit depths each colour type allows (PNG 11.2.2).
var pngDepths = map[byte][]byte{0: {1, 2, 4, 8, 16}, 2: {8, 16}, 3: {1, 2, 4, 8}, 4: {8, 16}, 6: {8, 16}}

// pngChannels are the samples per pixel of each colour type.
var pngChannels = map[byte]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}

type pngChunk struct {
	kind string
	data []byte
}

// sanitizePNG answers a PNG's stored form and its size in pixels.
func sanitizePNG(data []byte) ([]byte, int, int, error) {
	if len(data) > MaxImageBytes {
		return nil, 0, 0, fmt.Errorf("the PNG is %d bytes and a logo may be at most %d: export it smaller",
			len(data), MaxImageBytes)
	}
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, 0, 0, errors.New("the file is not a PNG: it does not begin with the PNG signature")
	}
	chunks, err := pngChunks(data[len(pngSignature):])
	if err != nil {
		return nil, 0, 0, err
	}
	if len(chunks) == 0 || chunks[0].kind != "IHDR" || len(chunks[0].data) != 13 {
		return nil, 0, 0, errors.New("the PNG does not begin with its IHDR header")
	}
	h := chunks[0].data
	width, height := binary.BigEndian.Uint32(h[0:4]), binary.BigEndian.Uint32(h[4:8])
	depth, color, compression, filter, interlace := h[8], h[9], h[10], h[11], h[12]
	switch {
	case width == 0 || height == 0:
		return nil, 0, 0, errors.New("the PNG is zero pixels wide or tall")
	case width > MaxImageSide || height > MaxImageSide:
		return nil, 0, 0, fmt.Errorf("the PNG is %dx%d and a logo is at most %d pixels a side: export it "+
			"smaller", width, height, MaxImageSide)
	case uint64(width)*uint64(height) > MaxImagePixels:
		return nil, 0, 0, fmt.Errorf("the PNG is %dx%d, more than %d pixels: export it smaller",
			width, height, MaxImagePixels)
	case !bytesContain(pngDepths[color], depth):
		return nil, 0, 0, fmt.Errorf("the PNG's colour type %d and bit depth %d are not a PNG's", color, depth)
	case compression != 0 || filter != 0 || interlace > 1:
		return nil, 0, 0, errors.New("the PNG's header names a compression, filter or interlace method PNG does not define")
	}
	var (
		kept    []pngChunk
		stream  bytes.Buffer
		palette bool
		inIDAT  int // 0 before the image data, 1 inside it, 2 after it
	)
	for i, c := range chunks {
		critical := c.kind[0]&0x20 == 0
		switch {
		case i > 0 && c.kind == "IHDR":
			return nil, 0, 0, errors.New("the PNG holds a second IHDR")
		case pngAnimated[c.kind]:
			return nil, 0, 0, errors.New("the PNG is animated (APNG): a logo is one frame, export a still PNG")
		case c.kind == "IDAT":
			if inIDAT == 2 {
				return nil, 0, 0, errors.New("the PNG's image data is split by another chunk")
			}
			inIDAT = 1
			stream.Write(c.data)
		case inIDAT == 1:
			inIDAT = 2
		}
		switch {
		case c.kind == "PLTE":
			palette = true
		case c.kind == "IEND" && i != len(chunks)-1:
			return nil, 0, 0, errors.New("the PNG holds chunks after its IEND")
		case c.kind == "iCCP" && len(c.data) > maxICCPBytes:
			continue
		case critical && c.kind != "IHDR" && c.kind != "PLTE" && c.kind != "IDAT" && c.kind != "IEND":
			return nil, 0, 0, fmt.Errorf("the PNG holds the critical chunk %q, which no decoder draws", c.kind)
		case !critical && !pngKept[c.kind]:
			continue
		}
		kept = append(kept, c)
	}
	switch {
	case chunks[len(chunks)-1].kind != "IEND":
		return nil, 0, 0, errors.New("the PNG never ends: it has no IEND")
	case inIDAT == 0:
		return nil, 0, 0, errors.New("the PNG holds no image data")
	case color == 3 && !palette:
		return nil, 0, 0, errors.New("the PNG is indexed and holds no palette")
	}
	if err := inflatePNG(stream.Bytes(), int(width), int(height), pngChannels[color]*int(depth), interlace == 1); err != nil {
		return nil, 0, 0, err
	}
	var out bytes.Buffer
	out.Write(pngSignature)
	for _, c := range kept {
		writePNGChunk(&out, c)
	}
	return out.Bytes(), int(width), int(height), nil
}

// pngChunks splits a PNG's chunks, checking each one's length and CRC, and
// refusing any byte after IEND.
func pngChunks(b []byte) ([]pngChunk, error) {
	var out []pngChunk
	for len(b) > 0 {
		if len(b) < 12 {
			return nil, errors.New("the PNG ends inside a chunk")
		}
		n := binary.BigEndian.Uint32(b[0:4])
		if uint64(n) > uint64(len(b)-12) {
			return nil, errors.New("a PNG chunk is longer than the file")
		}
		kind := string(b[4:8])
		for _, c := range []byte(kind) {
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return nil, fmt.Errorf("the PNG holds a chunk named %q, which is not a chunk name", kind)
			}
		}
		body := b[8 : 8+n]
		want := binary.BigEndian.Uint32(b[8+n : 12+n])
		if crc32.ChecksumIEEE(b[4:8+n]) != want {
			return nil, fmt.Errorf("the PNG's %s chunk fails its CRC: the file is damaged", kind)
		}
		out = append(out, pngChunk{kind: kind, data: body})
		b = b[12+n:]
		if kind == "IEND" && len(b) > 0 {
			return nil, errors.New("the PNG holds bytes after its IEND")
		}
	}
	return out, nil
}

// inflatePNG inflates the image stream in full, row by row, and refuses one
// that does not hold exactly the rows the header promises, each with a filter
// byte PNG defines.
func inflatePNG(stream []byte, width, height, bits int, interlaced bool) error {
	z, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		return fmt.Errorf("the PNG's image data cannot be inflated: %v", err)
	}
	defer func() { _ = z.Close() }()
	type pass struct{ w, h int }
	passes := []pass{{width, height}}
	if interlaced {
		// ADAM7: the seven passes' sizes (PNG 8.2).
		passes = nil
		for _, p := range [][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4},
			{1, 0, 2, 2}, {0, 1, 1, 2}} {
			w := (width - p[0] + p[2] - 1) / p[2]
			h := (height - p[1] + p[3] - 1) / p[3]
			if w > 0 && h > 0 {
				passes = append(passes, pass{w, h})
			}
		}
	}
	row := make([]byte, 1+(width*bits+7)/8)
	for _, p := range passes {
		line := row[:1+(p.w*bits+7)/8]
		for range p.h {
			if _, err := io.ReadFull(z, line); err != nil {
				return fmt.Errorf("the PNG's image data holds fewer rows than its header promises: %v", err)
			}
			if line[0] > 4 {
				return fmt.Errorf("the PNG's image data names filter %d, which PNG does not define", line[0])
			}
		}
	}
	// THE STREAM MUST END HERE, and reading to its end checks its Adler-32.
	rest, err := io.ReadAll(io.LimitReader(z, 1))
	switch {
	case err != nil:
		return fmt.Errorf("the PNG's image data is damaged: %v", err)
	case len(rest) > 0:
		return errors.New("the PNG's image data holds more than its header promises")
	}
	return nil
}

func writePNGChunk(out *bytes.Buffer, c pngChunk) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(c.data)))
	out.Write(n[:])
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte(c.kind))
	_, _ = crc.Write(c.data)
	out.WriteString(c.kind)
	out.Write(c.data)
	binary.BigEndian.PutUint32(n[:], crc.Sum32())
	out.Write(n[:])
}

func bytesContain(set []byte, b byte) bool { return bytes.IndexByte(set, b) >= 0 }
