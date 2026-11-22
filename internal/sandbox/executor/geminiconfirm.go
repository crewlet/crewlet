package executor

import (
	"bufio"
	"encoding/json"
	"io"
)

// A Gemini CLI take is CONFIRMED BY ITS CONTENT (ruling N2 of #1818): no
// record out of the model's view reaches the session, so no take token can,
// and a take counts as read once the main session holds its exact rendered
// text as a user message, where Gemini CLI puts an AfterAgent block's reason.
// Measured on 0.70.0: written before the next request, kept through a SIGKILL
// of every gemini process, and what a resume sends. Only a user entry's text
// part counts, whole. A compression's
// summary confirms nothing, its restatement of the kept entries neither
// (geminirecord.go): a note was sent plain before any compression could fold
// it, so its plain entry confirms it, even once a later `$patch` drops it.

// geminiConfirmed is which of takes, each the digest of a take's rendered text
// ([ItemDigest]) in the order the takes were made, the main session read from r
// holds: each user text part confirms the first unconfirmed take after the
// last one confirmed whose digest its text has, so two takes of identical text
// need two deliveries, and a take never
// delivered (its hook killed after it answered) leaves the later ones to be
// confirmed. A line that does not decode is passed over, as a torn last line
// is.
func geminiConfirmed(r io.Reader, takes []string, bound int) ([]bool, error) {
	confirmed := make([]bool, len(takes))
	next, restating := 0, false
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), bound, func(line []byte) error {
		var entry struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Patch   json.RawMessage `json:"$patch"`
			Rewind  json.RawMessage `json:"$rewindTo"`
		}
		if next == len(takes) || !decodes(json.Unmarshal, line, &entry) {
			return nil
		}
		if geminiSyncEnds(entry.Patch, entry.Rewind) {
			restating = false
		}
		if restating || entry.Type != "user" {
			return nil
		}
		if _, ok := geminiSummary(entry.Content); ok {
			restating = true
			return nil
		}
		for _, part := range geminiParts(entry.Content) {
			if part.Text == nil {
				continue
			}
			digest := ItemDigest(*part.Text)
			for i := next; i < len(takes); i++ {
				if takes[i] == digest {
					confirmed[i], next = true, i+1
					break
				}
			}
		}
		return nil
	}, func(int64) error { return nil })
	return confirmed, err
}
