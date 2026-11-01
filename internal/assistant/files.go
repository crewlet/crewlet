package assistant

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/words"
)

// A FILE'S TYPE IS READ FROM ITS BYTES, never taken from a browser's claim or
// a run's file name, and only a type on the list a caller allows is kept. The
// type decided here is the one a download is served as, beside
// `X-Content-Type-Options: nosniff`, so what a person's browser does with the
// file is decided by its bytes and nothing anybody said about them.

// ErrFileRefused is a file that may not be kept: past [wire.FileMaxBytes], of
// a type the caller does not allow, or named in a way no download can carry.
var ErrFileRefused = errors.New("assistant: the file is refused")

// SniffFile is the media type of content, named name, if it is one of allowed,
// and the name as it is kept: its last path element, with nothing a
// Content-Disposition header or a file system would read differently.
func SniffFile(name string, content []byte, allowed []string) (mediaType, kept string, err error) {
	kept, err = fileName(name)
	if err != nil {
		return "", "", err
	}
	if len(content) == 0 {
		return "", "", fmt.Errorf("%w: %q is empty", ErrFileRefused, kept)
	}
	if len(content) > wire.FileMaxBytes {
		return "", "", fmt.Errorf("%w: %q is %d bytes, over the %d a file may be",
			ErrFileRefused, kept, len(content), wire.FileMaxBytes)
	}
	mediaType = sniff(kept, content)
	if mediaType == "" || !slices.Contains(allowed, mediaType) {
		shown := mediaType
		if shown == "" {
			shown = "an unrecognised type"
		}
		return "", "", fmt.Errorf("%w: %q is %s, and only %s are kept here",
			ErrFileRefused, kept, shown, words.And(allowed))
	}
	return mediaType, kept, nil
}

// fileName is a name as it is kept, or why it cannot be.
func fileName(name string) (string, error) {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	if base == "." || base == "/" || base == ".." || base == "" {
		return "", fmt.Errorf("%w: a file needs a name", ErrFileRefused)
	}
	if !utf8.ValidString(base) || len(base) > assistantdomain.FileNameMaxBytes {
		return "", fmt.Errorf("%w: a file's name must be UTF-8 of at most %d bytes",
			ErrFileRefused, assistantdomain.FileNameMaxBytes)
	}
	for _, r := range base {
		if r < 0x20 || r == 0x7f || r == '"' || r == ';' {
			return "", fmt.Errorf("%w: %q holds a character a download cannot name",
				ErrFileRefused, base)
		}
	}
	return base, nil
}

// sniff is content's type: a binary type by its signature, SVG by its root
// element, and text by its name once it is shown to be text. Empty for
// anything else, HTML included, which no list allows.
func sniff(name string, content []byte) string {
	detected, _, _ := strings.Cut(http.DetectContentType(content), ";")
	switch detected {
	case "image/png", "image/jpeg", "image/webp", "image/gif", "application/pdf":
		return detected
	case "text/html":
		return ""
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return ""
	}
	if svg(content) {
		return "image/svg+xml"
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".csv":
		return "text/csv"
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		if json.Valid(content) {
			return "application/json"
		}
		return ""
	case ".txt", ".text", "":
		return "text/plain"
	}
	return ""
}

// svg reports whether content is an XML document whose root element is svg.
func svg(content []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(content))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if start, ok := tok.(xml.StartElement); ok {
			return strings.EqualFold(start.Name.Local, "svg")
		}
	}
}
