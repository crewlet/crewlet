package exec_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/exec"
)

// A TOOL CALL'S REQUEST REACHES ONLY THE ENGINE'S OWN ROUTES: the one URL
// builder the proposal digest, the exec grant's digest and the Dispatcher all
// use keeps the path an absolute path with the query apart, and refuses
// anything that could name another host or smuggle its own query. Mutation:
// drop any one clause of the path check and its row passes where it must not.
func TestARequestURLIsAnEngineRouteAlone(t *testing.T) {
	t.Parallel()
	u, err := exec.Request{Method: "GET", Path: "/tasks/ENG-1",
		Query: url.Values{"b": {"2"}, "a": {"1"}}}.URL()
	if err != nil {
		t.Fatalf("a route's path was refused: %v", err)
	}
	if got := u.String(); got != "/tasks/ENG-1?a=1&b=2" {
		t.Errorf("the URL is %q, want the path with its query encoded in order", got)
	}

	for _, tc := range []struct{ name, method, path string }{
		{"no method", "", "/tasks"},
		{"a method with a space", "GET /x", "/tasks"},
		{"a method with a newline", "GET\n", "/tasks"},
		{"a scheme", "GET", "https://example.com/tasks"},
		{"a host", "GET", "//example.com/tasks"},
		{"user information", "GET", "//user@/tasks"},
		{"its own query", "GET", "/tasks?status=open"},
		{"an empty query", "GET", "/tasks?"},
		{"a fragment", "GET", "/tasks#top"},
		{"a relative path", "GET", "tasks"},
		{"no path", "GET", ""},
		{"a path that does not parse", "GET", "/tasks%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := exec.Request{Method: tc.method, Path: tc.path}.URL()
			if !errors.Is(err, exec.ErrInvalidRequest) {
				t.Errorf("%s %q answered %v, want ErrInvalidRequest", tc.method, tc.path, err)
			}
		})
	}
}
