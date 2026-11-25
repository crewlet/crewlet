package coord_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// widestInstant is an instant at its longest as a record encodes it: UTC, to
// the nanosecond.
var widestInstant = time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)

// escapedTo is a JSON value's text of `<`, which JSON writes as six bytes,
// padded with plain bytes so that it encodes to exactly size bytes beside
// frame, the bytes around it.
func escapedTo(size, frame int) string {
	n := (size - frame) / 6
	return strings.Repeat("<", n) + strings.Repeat("a", size-frame-6*n)
}

// EVERY POSITIONS CLASS'S WIDEST RECORD FITS ITS BOUND, as the store encodes
// it: a definition whose schemas and words are each at what [Fits] keeps, in
// a byte JSON writes as six, beside its digest, instant and names; and a Home
// board and a person's instructions at what a save keeps, beside their two
// instants. Past the bound, the
// KV backend refuses the write.
//
// Mutation: drop the 512 from MaxMCPDefinitionBytes, or the 256 from
// MaxHomeBoardRecordBytes, and this is red.
func TestEveryPositionsClassesWidestRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	frame := len(`{"d":""}`)
	definition := coord.MCPToolDefinition{
		Digest: strings.Repeat("f", 64), Title: "a", HintsTitle: "a",
		Description: escapedTo(coord.MaxMCPToolWordsBytes-2, 0),
		Input:       json.RawMessage(`{"d":"` + escapedTo(coord.MaxMCPToolSchemaBytes-2, frame) + `"}`),
		Output:      json.RawMessage(`{}`),
		At:          widestInstant,
	}
	if !definition.Fits() || definition.Size() != coord.MaxMCPToolSchemaBytes+coord.MaxMCPToolWordsBytes {
		t.Fatalf("premise: the widest definition weighs %d (fits %v), want both of its bounds whole",
			definition.Size(), definition.Fits())
	}
	stored, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) > coord.MaxMCPDefinitionBytes {
		t.Errorf("the widest definition is stored as %d bytes, past its class's %d", len(stored),
			coord.MaxMCPDefinitionBytes)
	}

	board := []byte(`{"widgets":[],"pad":"` + strings.Repeat("<", coord.MaxHomeBoardBytes-len(`{"widgets":[],"pad":""}`)) + `"}`)
	if err := coord.CheckHomeBoard(board); err != nil {
		t.Fatalf("premise: the widest board is refused: %v", err)
	}
	// AS THE KV BACKEND ENCODES IT: without HTML escaping, the board in its
	// own bytes.
	var record bytes.Buffer
	enc := json.NewEncoder(&record)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(coord.HomeBoard{Board: board, UpdatedAt: widestInstant, AbsentSince: widestInstant}); err != nil {
		t.Fatal(err)
	}
	if size := record.Len() - len("\n"); size > coord.MaxHomeBoardRecordBytes {
		t.Errorf("the widest Home board is stored as %d bytes, past its class's %d", size,
			coord.MaxHomeBoardRecordBytes)
	}

	doc := []byte(`{"text":"` + strings.Repeat("<", coord.MaxAssistantInstructionsBytes-len(`{"text":""}`)) + `"}`)
	if err := coord.CheckAssistantInstructions(doc); err != nil {
		t.Fatalf("premise: the widest instructions are refused: %v", err)
	}
	record.Reset()
	if err := enc.Encode(coord.SeatInstructions{Instructions: doc, UpdatedAt: widestInstant,
		AbsentSince: widestInstant}); err != nil {
		t.Fatal(err)
	}
	if size := record.Len() - len("\n"); size > coord.MaxAssistantInstructionsRecordBytes {
		t.Errorf("the widest instructions are stored as %d bytes, past their class's %d", size,
			coord.MaxAssistantInstructionsRecordBytes)
	}
}

// EVERY POSITIONS FAMILY'S WIDEST RECORD FITS ITS CLASS, as the store encodes
// it: each field at the bound its Validate holds it to and every byte of free
// text one JSON writes as six. A capacity window records thirty-two
// participants, sixteen of them with the process a request recorded, and
// excludes sixteen more, each named by its excluder, beside eight journal
// records, and stays a small value every boot reads at the term; an OAuth
// grant's three addresses, fifty scopes and hundred accounts fit its 200 KiB.
//
// Mutation: raise MaxMaintenanceParticipants to 32 (which doubles
// MaxMaintenanceRecorded too), or MaxGrantScopeBytes to 512, and this is red.
func TestEveryPositionsFamilysWidestRecordFitsItsClass(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, w := range coordtest.WidestPositionsRecords() {
		seen[w.Class] = true
		if v, ok := w.Record.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				t.Errorf("premise: %s's widest record is refused: %v", w.Class, err)
			}
		}
		if stored := w.Stored(); stored > w.Bound {
			t.Errorf("%s's widest record is stored as %d bytes, past its class's %d", w.Class, stored, w.Bound)
		}
	}
	if len(seen) < 25 {
		t.Fatalf("premise: %d families at their widest, want every one the register declares beyond "+
			"the eight earlier records cover", len(seen))
	}
}

// A SENTENCE IS CUT TO WHAT IT TAKES AS STORED: a sentence whose bytes JSON
// escapes is cut to the bound it takes inside a record, marked, on a rune
// boundary, and one that fits is kept whole.
//
// Mutation: let a sentence of at most its bound in raw bytes through whole,
// and the escaped one is stored past its bound.
func TestASentenceIsCutToWhatItTakesAsStored(t *testing.T) {
	t.Parallel()
	for _, s := range []string{strings.Repeat("<", 100), strings.Repeat("é", 400),
		"a\x01b" + strings.Repeat("x", 300)} {
		cut := coord.FitText(s, 256)
		if coord.TextBytes(cut) > 256 || !strings.HasSuffix(cut, "…") || !utf8.ValidString(cut) {
			t.Errorf("FitText of %d bytes is %q, %d as stored: want at most 256, marked and whole runes",
				len(s), cut, coord.TextBytes(cut))
		}
	}
	if fits := strings.Repeat("a", 256); coord.FitText(fits, 256) != fits {
		t.Error("a sentence that fits its bound was cut")
	}
}

// A FIELD LEFT EMPTY IS REFUSED AS MISSING, and only one present and too long
// as past its bound: a feed mark's stream or group and an acknowledgement's
// log each have a sentence of their own, which a bound checked first would
// pre-empt.
//
// Mutation: check a feed mark's or an acknowledgement's bounds before its
// empty fields, and the empty ones are refused as past their bound.
func TestAnEmptyFieldIsRefusedAsMissingNotAsPastItsBound(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("s", coord.MaxStreamNameBytes+1)
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"a mark with no stream", coord.FeedMark{Group: "g", Floor: 1}.Validate(), "names no stream"},
		{"a mark with no group", coord.FeedMark{Stream: "S", Floor: 1}.Validate(), "names no group"},
		{"a mark on a stream past its bound", coord.FeedMark{Stream: long, Group: "g", Floor: 1}.Validate(),
			"no feed group on a log's stream"},
		{"an acknowledgement of no log", coord.MaintenanceAck{NodeID: "n", Incarnation: "n:p"}.Validate(),
			"acknowledged no log's window"},
		{"an acknowledgement of a log past its bound",
			coord.MaintenanceAck{Stream: long, NodeID: "n", Incarnation: "n:p"}.Validate(), "past its bound"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s is refused with %v, want it to say %q", c.what, c.err, c.want)
		}
	}
}
