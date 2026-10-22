package artifactsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// A VERSION THAT NO LONGER READS IN ITS FORMAT IS TOLD IN A PERSON'S WORDS,
// with the format's findings in the log: the detail was "version 3 does not
// read in this build's design format: " followed by the format's Go error,
// package name and all. Mutation: put the error back into the detail and
// this fails.
func TestAVersionThatNoLongerReadsIsToldInWords(t *testing.T) {
	t.Parallel()
	refused := fmt.Errorf("%w: canvas.json: board \"index.html\" is 0 wide", canvasformat.ErrInvalid)
	cache := newFormatCache[int](nil, "design", canvasformat.ErrInvalid,
		func(artifactsdomain.Version) string { return canvasformat.Path },
		func(manifestSource, artifactsdomain.Version) (int, int64, error) {
			return 0, 0, refused
		})
	rec := httptest.NewRecorder()
	_, ok := cache.read(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a/canvas", nil),
		Admission{}, artifactsdomain.Version{Number: 3})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	want := "Version 3 can't be shown: this version of Crewlet can't read it as a design. " +
		"Publishing it again fixes that."
	if ok || rec.Code != http.StatusConflict || body["error"] != "content_refused" ||
		body["detail"] != want {
		t.Errorf("answered %v %d %v, want 409 content_refused saying %q", ok, rec.Code, body, want)
	}
	for _, theirs := range []string{"canvasformat", "canvas.json", "0 wide", "invalid canvas"} {
		if strings.Contains(rec.Body.String(), theirs) {
			t.Errorf("the answer carries %q: %s", theirs, rec.Body.String())
		}
	}
}
