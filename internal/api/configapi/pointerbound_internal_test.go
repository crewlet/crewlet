package configapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// A WRITE WHOSE ORIGIN LEAVES THE POINTER PAST ITS BOUND IS REFUSED, NAMING
// BOTH SIZES, NOT FAILED. Its summary fits, so the writer's name is what does
// not: a 500 would send the caller to this node's log for a write that lands
// through a credential with a shorter name.
//
// Mutation: drop the PointerTooLargeError case from refuseApply, and the
// write answers 500.
func TestAnOriginPastWhatThePointerCarriesIsRefused(t *testing.T) {
	t.Parallel()
	at := time.Now()
	origin := coord.RevisionOrigin{Author: strings.Repeat("a", coord.MaxPointerBytes),
		AuthorKind: iam.ActorOperator, Source: "api", CreatedAt: at}
	err := carried([]byte(`{"v":1}`), []byte(`{"v":1}`), "a short summary", at, origin)
	var pointer *PointerTooLargeError
	if !errors.As(err, &pointer) || !errors.Is(err, coord.ErrValueTooLarge) {
		t.Fatalf("a write under an author of %d bytes = %v, want a PointerTooLargeError",
			len(origin.Author), err)
	}
	res := httptest.NewRecorder()
	(&Service{}).refuseApply(res, httptest.NewRequest(http.MethodPut, "/config", nil), err)
	var body map[string]any
	if decodeErr := json.Unmarshal(res.Body.Bytes(), &body); decodeErr != nil {
		t.Fatalf("decode %s: %v", res.Body, decodeErr)
	}
	if res.Code != http.StatusBadRequest || body["error"] != string(httpjson.CodeOriginTooLong) ||
		body["pointer_bytes"] != float64(pointer.PointerBytes) ||
		body["max_pointer_bytes"] != float64(coord.MaxPointerBytes) {
		t.Errorf("the refusal = %d %s, want 400 origin_too_long naming both sizes", res.Code, res.Body)
	}
}
