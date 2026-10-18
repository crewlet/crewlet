package artifactupload

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE BYTES MUST BE WHAT THEIR TYPE SAYS. The served type is the media table's,
// never sniffed (every response carries nosniff), so a file whose bytes are
// another format is a polyglot passing for a benign one: an HTML document
// uploaded as an image, a script as a font. Every new file is checked as it is
// read back: text must be UTF-8, and a binary file must begin as its format
// does.

// sniffBytes is how much of a file its format is judged from.
const sniffBytes = 64

// signature is how a binary format's bytes begin.
type signature struct {
	offset int
	magic  []byte
}

// signatures are the leading bytes each binary type the media table serves
// begins with: any one of a type's signatures admits it.
var signatures = map[string][]signature{
	"image/png":         {{0, []byte("\x89PNG\r\n\x1a\n")}},
	"image/jpeg":        {{0, []byte("\xff\xd8\xff")}},
	"image/gif":         {{0, []byte("GIF87a")}, {0, []byte("GIF89a")}},
	"image/webp":        {{8, []byte("WEBP")}},
	"image/avif":        {{4, []byte("ftypavif")}, {4, []byte("ftypavis")}, {4, []byte("ftypmif1")}},
	"image/x-icon":      {{0, []byte("\x00\x00\x01\x00")}, {0, []byte("\x00\x00\x02\x00")}},
	"font/woff":         {{0, []byte("wOFF")}},
	"font/woff2":        {{0, []byte("wOF2")}},
	"font/ttf":          {{0, []byte("\x00\x01\x00\x00")}, {0, []byte("true")}},
	"font/otf":          {{0, []byte("OTTO")}},
	"audio/mpeg":        {{0, []byte("ID3")}, {0, []byte("\xff\xfb")}, {0, []byte("\xff\xf3")}, {0, []byte("\xff\xf2")}},
	"audio/wav":         {{8, []byte("WAVE")}},
	"audio/ogg":         {{0, []byte("OggS")}},
	"video/ogg":         {{0, []byte("OggS")}},
	"audio/mp4":         {{4, []byte("ftyp")}},
	"video/mp4":         {{4, []byte("ftyp")}},
	"video/webm":        {{0, []byte("\x1a\x45\xdf\xa3")}},
	"audio/webm":        {{0, []byte("\x1a\x45\xdf\xa3")}},
	"application/wasm":  {{0, []byte("\x00asm")}},
	"model/gltf-binary": {{0, []byte("glTF")}},
	"application/pdf":   {{0, []byte("%PDF-")}},
}

// checkContent refuses bytes that are not what typ says: head is the file's
// first [sniffBytes] (all of it for text, which is judged whole).
func checkContent(path string, typ mediatype.Type, head []byte, size int64) error {
	if typ.Text {
		if !utf8.Valid(head) {
			return &ContentRefused{Path: path, Reason: fmt.Sprintf("a %s file must be UTF-8 text",
				typ.Essence())}
		}
		return nil
	}
	sigs, known := signatures[typ.Essence()]
	if !known {
		return &ContentRefused{Path: path, Reason: fmt.Sprintf("this build cannot check a %s "+
			"file's bytes", typ.Essence())}
	}
	if size == 0 {
		return &ContentRefused{Path: path, Reason: fmt.Sprintf("an empty file is not a %s",
			typ.Essence())}
	}
	for _, sig := range sigs {
		if len(head) >= sig.offset+len(sig.magic) &&
			bytes.Equal(head[sig.offset:sig.offset+len(sig.magic)], sig.magic) {
			return nil
		}
	}
	return &ContentRefused{Path: path, Reason: fmt.Sprintf("the bytes do not begin as a %s "+
		"file's do; a file is served as the type its extension names, so publish it under "+
		"the extension of what it is", typ.Essence())}
}

// checkCredentials refuses, for an agent, text carrying a credential shape,
// naming the file and the line: never silently redacted, which would ship a
// broken page, and never let through, which is how an agent's .env reaches a
// hidden script block.
func checkCredentials(kind iam.ActorKind, what string, text []byte) error {
	if kind != iam.ActorAgent || len(text) == 0 {
		return nil
	}
	s := string(text)
	if !redact.Contains(s) {
		return nil
	}
	// THE LINE IS WHERE THE REDACTED TEXT FIRST DIFFERS: the one grammar
	// that knows a credential's shape is internal/redact's, and it says
	// where by what it would replace.
	redacted := redact.Secrets(s)
	at := 0
	for at < len(s) && at < len(redacted) && s[at] == redacted[at] {
		at++
	}
	line := 1 + strings.Count(s[:at], "\n")
	return fmt.Errorf("%w: %s, line %d, carries a credential; remove it and publish again "+
		"(an artifact is read by everyone it is shared with)", ErrCredential, what, line)
}

// checkStrings refuses, for an agent, a credential in any of a publish's own
// strings: its paths, title, description and labels.
func checkStrings(kind iam.ActorKind, fields map[string]string) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := checkCredentials(kind, name, []byte(fields[name])); err != nil {
			return err
		}
	}
	return nil
}
