package assistant

import (
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
	mediaType = sniff(content)
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

// sniff is content's type, read from its signature: one of the image types a
// person may attach, and empty for anything else, SVG and HTML included.
func sniff(content []byte) string {
	detected, _, _ := strings.Cut(http.DetectContentType(content), ";")
	if slices.Contains(wire.AttachmentMediaTypes, detected) {
		return detected
	}
	return ""
}
