package workapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// AN UPLOAD THE STORE DID NOT TAKE IS TOLD BY ITS CAUSE: a full store, which
// no wait clears, carries no Retry-After, says who makes room and has a
// message that says waiting won't help; a condition of the node (the upload
// ran out of its pace) carries its own words; a store that did not take the
// bytes is told so in words, with the wait an election takes; and a failure
// the store classed as neither is the node's own fault, `500`, as the
// artifact uploads answer it. Every one was "the file's content could not be
// stored in the object store right now" under the election's four seconds, so
// a full store was uploaded into again every four seconds. Mutations: drop the
// store's classification of a full store or of one that did not answer, or
// answer a fault as a condition, and its row fails; put the internal term back
// and the last assertion does.
func TestAnUploadTheStoreDidNotTakeIsToldByItsCause(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err          error
		status       int
		says         string
		retry        string
		messageWaits bool
	}{
		"a full store": {fmt.Errorf("objstore: store the object: natsobj: put x: %w",
			objstore.ErrFull), http.StatusServiceUnavailable, objstore.FullWords, "", false},
		"an upload past its pace": {fmt.Errorf("objstore: read the object: %w",
			context.DeadlineExceeded), http.StatusServiceUnavailable,
			"This took too long, so it stopped. Try again in a moment.", "2", true},
		"a store that did not answer": {fmt.Errorf("objstore: store the object: %w: %w",
			objstore.ErrUnanswered, errors.New("natsobj: put x: nats: timeout")),
			http.StatusServiceUnavailable, "The company's files couldn't be saved just now. " +
				"Try again in a moment.", "4", true},
		"a fault": {errors.New("objstore: the backend stored x after 3 bytes, before the " +
			"content ended"), http.StatusInternalServerError, "", "", false},
	} {
		rec := httptest.NewRecorder()
		bytesFailed(rec, httptest.NewRequest(http.MethodPut, "/worklet/files/ENG/a.txt", nil),
			c.err, "op-1")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		detail, _ := body["detail"].(string)
		if rec.Code != c.status || detail != c.says {
			t.Errorf("%s: answered %d %v, want %d saying %q", name, rec.Code, body, c.status,
				c.says)
		}
		if got := rec.Header().Get("Retry-After"); got != c.retry {
			t.Errorf("%s: Retry-After %q, want %q", name, got, c.retry)
		}
		message, _ := body["message"].(string)
		if waits := strings.Contains(message, "in a moment"); waits != c.messageWaits {
			t.Errorf("%s: the message %q invites a wait: %v, want %v", name, message, waits,
				c.messageWaits)
		}
		if text := rec.Body.String(); strings.Contains(text, "object store") ||
			strings.Contains(text, "nats") || strings.Contains(text, "3 bytes") {
			t.Errorf("%s: told a person %s", name, text)
		}
	}
}
