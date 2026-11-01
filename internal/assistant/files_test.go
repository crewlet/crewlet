package assistant

import (
	"bytes"
	"errors"
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
		"a png named anything":    {"logo.txt", string(png), wire.FileMediaTypes, "image/png"},
		"an svg":                  {"logo.svg", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, wire.FileMediaTypes, "image/svg+xml"},
		"a csv":                   {"palette.csv", "name,hex\nink,#090019\n", wire.FileMediaTypes, "text/csv"},
		"markdown":                {"report.md", "# Teams\n", wire.FileMediaTypes, "text/markdown"},
		"json":                    {"plan.json", `{"teams":[]}`, wire.FileMediaTypes, "application/json"},
		"html called svg":         {"logo.svg", "<!DOCTYPE html><html><script>x()</script></html>", wire.FileMediaTypes, ""},
		"html called text":        {"notes.txt", "<html><body>hi</body></html>", wire.FileMediaTypes, ""},
		"invalid json":            {"plan.json", "{nope", wire.FileMediaTypes, ""},
		"an svg as an attachment": {"logo.svg", "<svg/>", wire.AttachmentMediaTypes, ""},
		"a script":                {"run.sh", "#!/bin/sh\nrm -rf /\n", wire.FileMediaTypes, ""},
		"a header-breaking name":  {"a\";x=.png", string(png), wire.FileMediaTypes, ""},
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
	if _, _, err := SniffFile("big.txt", bytes.Repeat([]byte("a"), wire.FileMaxBytes+1),
		wire.FileMediaTypes); !errors.Is(err, ErrFileRefused) {
		t.Fatalf("a file past the cap was kept: %v", err)
	}
	if _, kept, _ := SniffFile("../../etc/report.md", []byte("# x"), wire.FileMediaTypes); kept != "report.md" {
		t.Fatalf("a path was kept as %q, want its last element", kept)
	}
}
