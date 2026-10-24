package brandformat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ---- WebP ----------------------------------------------------------------- //
//
// A WEBP LOGO IS ONE STILL IMAGE IN ITS RIFF CONTAINER, read chunk by chunk:
// the simple formats (one VP8 or VP8L chunk) and the extended one (VP8X, then
// an optional colour profile and alpha, then the image). Its EXIF and XMP
// chunks are dropped, with the extended header's flags and the container's
// size written to match, because that is where a camera or an editor leaves
// a location, a name or a device. An animation (ANIM, ANMF, or the header's
// animation flag) is refused, as is any chunk WebP does not define for a still
// image: a logo is one frame. The image's size comes from its own bitstream
// header, and must match the extended header's canvas where there is one.

// webpChunks are the chunks a stored WebP may hold; EXIF and XMP are read and
// dropped.
var webpChunks = map[string]bool{"VP8X": true, "ICCP": true, "ALPH": true, "VP8 ": true, "VP8L": true,
	"EXIF": true, "XMP ": true}

// The extended header's flags (WebP container spec, "Extended file format").
const (
	webpFlagAnimation = 0x02
	webpFlagXMP       = 0x04
	webpFlagEXIF      = 0x08
)

type webpChunk struct {
	kind string
	data []byte
}

// sanitizeWebP answers a WebP's stored form and its size in pixels.
func sanitizeWebP(data []byte) ([]byte, int, int, error) {
	if len(data) > MaxImageBytes {
		return nil, 0, 0, fmt.Errorf("the WebP is %d bytes and a logo may be at most %d: export it smaller",
			len(data), MaxImageBytes)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, 0, 0, errors.New("the file is not a WebP: it is not a RIFF WEBP container")
	}
	if size := binary.LittleEndian.Uint32(data[4:8]); uint64(size)+8 != uint64(len(data)) {
		return nil, 0, 0, fmt.Errorf("the WebP's container says %d bytes and the file holds %d: the file is "+
			"damaged", uint64(size)+8, len(data))
	}
	var chunks []webpChunk
	for b := data[12:]; len(b) > 0; {
		if len(b) < 8 {
			return nil, 0, 0, errors.New("the WebP ends inside a chunk header")
		}
		kind, n := string(b[0:4]), binary.LittleEndian.Uint32(b[4:8])
		padded := uint64(n) + uint64(n&1)
		if padded > uint64(len(b)-8) {
			return nil, 0, 0, fmt.Errorf("the WebP's %q chunk is longer than the file", kind)
		}
		switch {
		case kind == "ANIM" || kind == "ANMF":
			return nil, 0, 0, errors.New("the WebP is animated: a logo is one frame, export a still image")
		case !webpChunks[kind]:
			return nil, 0, 0, fmt.Errorf("the WebP holds a %q chunk, which a still WebP does not define", kind)
		}
		chunks = append(chunks, webpChunk{kind: kind, data: b[8 : 8+n]})
		b = b[8+padded:]
	}
	if len(chunks) == 0 {
		return nil, 0, 0, errors.New("the WebP holds no image")
	}
	var (
		kept          []webpChunk
		images        int
		width, height int
		canvasW       int
		canvasH       int
	)
	extended := chunks[0].kind == "VP8X"
	for i, c := range chunks {
		switch c.kind {
		case "VP8X":
			if i != 0 || len(c.data) != 10 {
				return nil, 0, 0, errors.New("the WebP's extended header is misplaced or malformed")
			}
			if c.data[0]&webpFlagAnimation != 0 {
				return nil, 0, 0, errors.New("the WebP is animated: a logo is one frame, export a still image")
			}
			canvasW = 1 + int(uint32(c.data[4])|uint32(c.data[5])<<8|uint32(c.data[6])<<16)
			canvasH = 1 + int(uint32(c.data[7])|uint32(c.data[8])<<8|uint32(c.data[9])<<16)
			header := bytes.Clone(c.data)
			header[0] &^= webpFlagEXIF | webpFlagXMP
			kept = append(kept, webpChunk{kind: c.kind, data: header})
			continue
		case "EXIF", "XMP ":
			continue
		case "ICCP", "ALPH":
			if !extended {
				return nil, 0, 0, fmt.Errorf("the WebP holds a %q chunk without an extended header", c.kind)
			}
		case "VP8 ", "VP8L":
			images++
			w, h, err := webpSize(c)
			if err != nil {
				return nil, 0, 0, err
			}
			width, height = w, h
		}
		kept = append(kept, c)
	}
	switch {
	case images != 1:
		return nil, 0, 0, fmt.Errorf("the WebP holds %d images and a logo is one", images)
	case extended && (width != canvasW || height != canvasH):
		return nil, 0, 0, fmt.Errorf("the WebP's image is %dx%d and its canvas %dx%d: the file is damaged",
			width, height, canvasW, canvasH)
	case width > MaxImageSide || height > MaxImageSide:
		return nil, 0, 0, fmt.Errorf("the WebP is %dx%d and a logo is at most %d pixels a side: export it smaller",
			width, height, MaxImageSide)
	case width*height > MaxImagePixels:
		return nil, 0, 0, fmt.Errorf("the WebP is %dx%d, more than %d pixels: export it smaller",
			width, height, MaxImagePixels)
	}
	var body bytes.Buffer
	body.WriteString("WEBP")
	for _, c := range kept {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(c.data)))
		body.WriteString(c.kind)
		body.Write(n[:])
		body.Write(c.data)
		if len(c.data)%2 == 1 {
			body.WriteByte(0)
		}
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(body.Len()))
	out.Write(n[:])
	out.Write(body.Bytes())
	return out.Bytes(), width, height, nil
}

// webpSize reads an image chunk's size from its bitstream header: a lossy
// keyframe's (RFC 6386 9.1) or a lossless image's (the VP8L header).
func webpSize(c webpChunk) (int, int, error) {
	d := c.data
	if c.kind == "VP8L" {
		if len(d) < 5 || d[0] != 0x2f {
			return 0, 0, errors.New("the WebP's lossless image has no VP8L header")
		}
		bits := binary.LittleEndian.Uint32(d[1:5])
		if bits>>29 != 0 {
			return 0, 0, errors.New("the WebP's lossless image names a version WebP does not define")
		}
		return 1 + int(bits&0x3fff), 1 + int(bits>>14&0x3fff), nil
	}
	if len(d) < 10 || d[0]&1 != 0 || d[3] != 0x9d || d[4] != 0x01 || d[5] != 0x2a {
		return 0, 0, errors.New("the WebP's lossy image is not a keyframe with a VP8 start code")
	}
	w := int(binary.LittleEndian.Uint16(d[6:8]) & 0x3fff)
	h := int(binary.LittleEndian.Uint16(d[8:10]) & 0x3fff)
	if w == 0 || h == 0 {
		return 0, 0, errors.New("the WebP's image is zero pixels wide or tall")
	}
	// THE FIRST PARTITION LIES INSIDE THE CHUNK (RFC 6386 9.1, the frame
	// tag's 19-bit size): the bound the frame header itself states. Bytes
	// past a lossy or lossless bitstream's end cannot be found without
	// decoding the image, which the engine does not do; they draw nothing and
	// are served under nosniff, inert.
	if part := int(uint32(d[0])|uint32(d[1])<<8|uint32(d[2])<<16) >> 5; part > len(d)-10 {
		return 0, 0, fmt.Errorf("the WebP's lossy image says its first partition is %d bytes and the "+
			"chunk holds %d after its header: the file is damaged", part, len(d)-10)
	}
	return w, h, nil
}
