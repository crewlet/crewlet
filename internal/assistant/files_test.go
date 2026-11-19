package assistant

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A FILE'S TYPE IS ITS BYTES', and only an allowed type is kept: HTML never,
// whatever it is called, and a name that could break a download's header is
// refused rather than repaired.
func TestAFilesTypeIsReadFromItsBytes(t *testing.T) {
	t.Parallel()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	for name, c := range map[string]struct {
		file, content string
		allowed       []string
		want          string
	}{
		"a png named anything":    {"logo.txt", string(png), wire.AttachmentMediaTypes, "image/png"},
		"a gif":                   {"spin.gif", "GIF89a" + string(bytes.Repeat([]byte{0}, 32)), wire.AttachmentMediaTypes, "image/gif"},
		"an svg as an attachment": {"logo.svg", "<svg/>", wire.AttachmentMediaTypes, ""},
		"html called png":         {"logo.png", "<!DOCTYPE html><html><script>x()</script></html>", wire.AttachmentMediaTypes, ""},
		"text":                    {"notes.txt", "hello", wire.AttachmentMediaTypes, ""},
		"a script":                {"run.sh", "#!/bin/sh\nrm -rf /\n", wire.AttachmentMediaTypes, ""},
		"a header-breaking name":  {"a\";x=.png", string(png), wire.AttachmentMediaTypes, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, _, err := SniffFile(c.file, []byte(c.content), c.allowed)
			if c.want == "" {
				if !errors.Is(err, ErrFileRefused) {
					t.Fatalf("kept as %q, err %v; want it refused", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("SniffFile = %q, %v; want %q", got, err, c.want)
			}
		})
	}
	big := append(slices.Clone(png), bytes.Repeat([]byte{0}, wire.FileMaxBytes)...)
	if _, _, err := SniffFile("big.png", big, wire.AttachmentMediaTypes); !errors.Is(err, ErrFileRefused) {
		t.Fatalf("a file past the cap was kept: %v", err)
	}
	if _, kept, _ := SniffFile("../../etc/logo.png", png, wire.AttachmentMediaTypes); kept != "logo.png" {
		t.Fatalf("a path was kept as %q, want its last element", kept)
	}
}
