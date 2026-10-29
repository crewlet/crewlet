package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// A REFUSED WRITE LOGS ONE LINE, AND NOTHING IT CARRIED.
//
// A write the route refuses logs exactly one `config_write_refused`, with the
// mounted route (never the path, which names the secret), the method, the
// status, the envelope's code, the revision written against and the request
// id the answer carries; the value written, the secret's name and the
// refusal's detail appear nowhere in it. A write that lands, a read and a
// failure of the engine's own (a 5xx) log nothing.
func TestARefusedWriteLogsOneLineAndNothingItCarried(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	inner := http.NewServeMux()
	mux := refusalMux{Mux: inner, log: slog.New(slog.NewJSONHandler(&logged, nil))}
	const secretName, secretValue = "GITLAB_TOKEN_SWE", "glpat-a-secret-value-0000"
	mux.Handle("PUT /secrets/{name}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case string(body) == "lands":
			httpjson.Write(w, http.StatusOK, map[string]any{"name": r.PathValue("name")})
		case string(body) == "breaks":
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		default:
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeRevisionAdvanced, map[string]string{
				"detail": "the value " + string(body) + " of " + r.PathValue("name") + " was not stored"})
		}
	}))
	mux.Handle("GET /secrets/{name}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	}))

	write := func(body, requestID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/secrets/"+secretName, strings.NewReader(body))
		req.Header.Set("If-Match", `"rev-41"`)
		if requestID != "" {
			req.Header.Set(requestIDHeader, requestID)
		}
		res := httptest.NewRecorder()
		inner.ServeHTTP(res, req)
		return res
	}
	res := write(secretValue, "req-7f3a")
	if res.Code != http.StatusConflict || res.Header().Get(requestIDHeader) != "req-7f3a" {
		t.Fatalf("the refused write = %d with request id %q", res.Code, res.Header().Get(requestIDHeader))
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("a refusal logged %d lines, want one:\n%s", len(lines), logged.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"msg": "config_write_refused", "level": "INFO", "route": "PUT /secrets/{name}", "method": "PUT",
		"status": float64(http.StatusConflict), "code": "revision_advanced", "base_revision": "rev-41",
		"request_id": "req-7f3a", "dry_run": false,
	} {
		if line[key] != want {
			t.Errorf("the line's %s = %v, want %v: %s", key, line[key], want, lines[0])
		}
	}
	for _, leak := range []string{secretValue, secretName, "was not stored"} {
		if strings.Contains(lines[0], leak) {
			t.Errorf("the line carries %q: %s", leak, lines[0])
		}
	}

	// A REQUEST ID THE CALLER DID NOT SEND is minted and answered.
	logged.Reset()
	if res := write(secretValue, ""); res.Header().Get(requestIDHeader) == "" ||
		!strings.Contains(logged.String(), res.Header().Get(requestIDHeader)) {
		t.Errorf("a refusal with no request id answered %q and logged %s",
			res.Header().Get(requestIDHeader), logged.String())
	}

	// NOTHING for a write that lands, a 5xx, or a read.
	logged.Reset()
	write("lands", "")
	write("breaks", "")
	get := httptest.NewRecorder()
	inner.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/secrets/"+secretName, nil))
	if logged.Len() != 0 {
		t.Errorf("a landed write, a 5xx or a read logged: %s", logged.String())
	}
}

// A BASE REVISION IS LOGGED ONLY IN A TAG'S SHAPE. If-Match is the caller's
// text; the line carries it only where it is one revision tag of at most 128
// bytes, and marks anything else invalid rather than writing it down.
func TestABaseRevisionIsLoggedOnlyInATagsShape(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 128)
	for _, tc := range []struct{ ifMatch, want string }{
		{"", ""},
		{"*", "*"},
		{`"0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12"`, "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12"},
		{`"llm-providers-3fa9c1d2e4b6a8807f1e2d3c4b5a6978"`, "llm-providers-3fa9c1d2e4b6a8807f1e2d3c4b5a6978"},
		{`W/"rev-41"`, "rev-41"},
		{"rev-41", "rev-41"},
		{`"` + long + `"`, long},
		{`"` + long + `a"`, invalidRevision},
		{`"rev-41", "rev-42"`, invalidRevision},
		{`"glpat a secret"`, invalidRevision},
		{`"rev-41\u0000"`, invalidRevision},
		{`"rév"`, invalidRevision},
		{`""`, invalidRevision},
	} {
		if got := baseRevision(tc.ifMatch); got != tc.want {
			t.Errorf("baseRevision(%q) = %q, want %q", tc.ifMatch, got, tc.want)
		}
	}

	// AND THE LINE USES IT: a header that is not a tag reaches the log as
	// invalid, its text nowhere.
	var logged bytes.Buffer
	inner := http.NewServeMux()
	refusalMux{Mux: inner, log: slog.New(slog.NewJSONHandler(&logged, nil))}.Handle("PUT /config",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			httpjson.Fail(w, http.StatusConflict, httpjson.CodeRevisionAdvanced)
		}))
	req := httptest.NewRequest(http.MethodPut, "/config", nil)
	req.Header.Set("If-Match", `"pasted glpat-a-secret-value-0000 here"`)
	inner.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(logged.String(), `"base_revision":"<invalid>"`) ||
		strings.Contains(logged.String(), "glpat") {
		t.Errorf("an If-Match that is no tag logged %s", logged.String())
	}
}
