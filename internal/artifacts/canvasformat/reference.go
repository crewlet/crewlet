package canvasformat

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonform"
)

// Reference is the canvas format as an agent reads it before writing a
// design: every key of `canvas.json` with its type ([Form], the strict
// decode's own tree) and the bounds the reader checks against.
func Reference() string {
	side := fmt.Sprintf("a whole number of pixels, %d to %d", MinSide, MaxSide)
	at := fmt.Sprintf("a whole number, at most %d either way", MaxCoordinate)
	var b strings.Builder
	b.WriteString("# A design: " + Path + " and its artboards\n\n")
	b.WriteString("A design is `" + Path + "` at the version's root and one HTML page per " +
		"artboard. Every HTML page of the version is a board and every board names one. A key " +
		"not listed is refused, and so is a value of another type. `{}` is each value of an " +
		"object keyed by a page's path, and `[]` each element of a list.\n\n")
	b.WriteString("## Every key of " + Path + "\n\n")
	b.WriteString(Form.Outline(map[string]string{
		"v":             fmt.Sprintf("required, always %d", Version),
		"boards":        fmt.Sprintf("the artboards by page path, 1 to %d", MaxBoards),
		"boards{}.x":    at,
		"boards{}.y":    at,
		"boards{}.w":    side,
		"boards{}.h":    side,
		"order":         "every board's page path once, back to front",
		"notes":         fmt.Sprintf("at most %d", MaxNotes),
		"notes[].id":    fmt.Sprintf("short and lowercase, at most %d bytes", MaxNoteIDBytes),
		"notes[].kind":  jsonform.OneOf(NoteKinds),
		"notes[].x":     at,
		"notes[].y":     at,
		"notes[].w":     side,
		"notes[].h":     side,
		"notes[].text":  fmt.Sprintf("plain text, at most %d bytes", MaxNoteBytes),
		"notes[].color": "a sticky's colour: " + jsonform.OneOf(NoteColors),
	}))
	return b.String()
}
