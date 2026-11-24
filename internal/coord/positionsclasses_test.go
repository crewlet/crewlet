package coord_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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
// board at what a save keeps, beside its two instants. Past the bound, the
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
}
