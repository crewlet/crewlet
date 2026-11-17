package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// The image types a tool's answer may carry, read from an image's bytes.
// The three every vendor's model reads in an image block; GIF and the rest are not.
const (
	ImagePNG  = "image/png"
	ImageJPEG = "image/jpeg"
	ImageWebP = "image/webp"
)

// MaxImages is how many images one answer may carry: a crop and the page around it, never a gallery.
const MaxImages = 2

// MaxImageBytes bounds one image: models read a picture scaled to about a megapixel, which a compressed crop fits well within.
const MaxImageBytes = 256 << 10

// ErrImageRefused is every image a tool's answer may not carry, wrapped with why.
var ErrImageRefused = errors.New("image refused")

// Image is a picture a tool's answer hands the model to see, beside its text.
// Build one with [NewImage]; an answer carrying any is held to [CheckImages].
type Image struct {
	MIMEType string
	Data     []byte
}

// NewImage is data as an image a tool's answer may carry, its type read from
// its first bytes. Bytes that are not PNG, JPEG or WebP, or that weigh more
// than [MaxImageBytes], are refused with why.
func NewImage(data []byte) (Image, error) {
	img := Image{MIMEType: sniffImage(data), Data: data}
	if err := img.check(); err != nil {
		return Image{}, err
	}
	return img, nil
}

// CheckImages reports why images may not ride an answer whose text is output,
// or nil where they may. An image cannot be fenced (ADR-0040), so it rides
// only beside text, which is where its tool says whose picture it is.
func CheckImages(output string, images []Image) error {
	if len(images) == 0 {
		return nil
	}
	if len(images) > MaxImages {
		return fmt.Errorf("%w: the answer carries %d images, and at most %d are sent",
			ErrImageRefused, len(images), MaxImages)
	}
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("%w: the answer carries an image and no text saying whose picture it is",
			ErrImageRefused)
	}
	for i, img := range images {
		if err := img.check(); err != nil {
			return fmt.Errorf("image %d: %w", i+1, err)
		}
	}
	return nil
}

// check reports why img may not be sent: a type its bytes do not bear out,
// bytes of no type sent, or a size past [MaxImageBytes].
func (img Image) check() error {
	if len(img.Data) > MaxImageBytes {
		return fmt.Errorf("%w: it weighs %d bytes, past the %d an image may", ErrImageRefused,
			len(img.Data), MaxImageBytes)
	}
	sniffed := sniffImage(img.Data)
	if sniffed == "" {
		return fmt.Errorf("%w: its bytes are not a PNG, a JPEG or a WebP image", ErrImageRefused)
	}
	if img.MIMEType != sniffed {
		return fmt.Errorf("%w: it says %q and its bytes are %s", ErrImageRefused, img.MIMEType, sniffed)
	}
	return nil
}

// The signatures each type's file opens with, as their formats define them.
var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// sniffImage is the type data's first bytes name, or "" for none sent.
func sniffImage(data []byte) string {
	switch {
	case bytes.HasPrefix(data, pngMagic):
		return ImagePNG
	case bytes.HasPrefix(data, jpegMagic):
		return ImageJPEG
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ImageWebP
	}
	return ""
}

// ImageRecord is what a record of a call keeps of an image its answer
// carried: its type and its size, never its bytes.
type ImageRecord struct {
	MIMEType string `json:"mime_type"`
	Bytes    int    `json:"bytes"`
}

// ImageRecords is images as a record keeps them, nil for none.
func ImageRecords(images []Image) []ImageRecord {
	if len(images) == 0 {
		return nil
	}
	out := make([]ImageRecord, len(images))
	for i, img := range images {
		out[i] = ImageRecord{MIMEType: img.MIMEType, Bytes: len(img.Data)}
	}
	return out
}
