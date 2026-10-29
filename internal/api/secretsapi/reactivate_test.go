package secretsapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// company is the config surface a write asks: which paths of the active
// revision read each name, and the re-activation, which it counts.
type company struct {
	mu sync.Mutex
	// reads is each name the active revision reads, and the paths reading it.
	reads map[string][]string
	// refuse is what every reload answers, nil for a reload that lands.
	refuse error
	// reloads are the summaries every reload was asked with, in order.
	reloads []string
}

func (c *company) Reads(_ context.Context, names []string) (map[string][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	for _, name := range names {
		if paths, ok := c.reads[name]; ok {
			out[name] = paths
		}
	}
	return out, nil
}

func (c *company) Reload(_ context.Context, summary string, _ iam.Actor,
	_ string) (configapi.Applied, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads = append(c.reloads, summary)
	if c.refuse != nil {
		return configapi.Applied{}, c.refuse
	}
	return configapi.Applied{RevisionID: "r-" + string(rune('0'+len(c.reloads))),
		Epoch: int64(40 + len(c.reloads))}, nil
}

func (c *company) summaries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reloads)
}

// over serves the surface over a company that reads what the case says.
func over(t *testing.T, c *company) http.Handler {
	t.Helper()
	return mounted(t, secretsapi.Options{
		Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock }, Config: c,
	}, iam.AllGrants...)
}

func decoded(t *testing.T, body string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}
	return out
}

// A WRITE OF A SECRET THE COMPANY READS RE-ACTIVATES THE REVISION THAT READS IT.
//
// A `${VAR}` is resolved when a provider is built, from the snapshot an apply
// takes, so a value sealed and left there signs nothing in: every model kept
// the old credential until something re-activated, and nothing did. The write
// re-activates once, in the same request, and its answer names the paths and
// the epoch it produced (ADR-0070).
func TestAWriteOfAReadSecretReactivates(t *testing.T) {
	t.Parallel()
	c := &company{reads: map[string][]string{
		"CLAUDE_CODE_OAUTH_TOKEN": {"providers.llm.default.auth.token"},
	}}
	h := over(t, c)

	code, body := call(t, h, http.MethodPut, "/secrets/CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-x")
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	got := decoded(t, body)
	if paths, _ := got["read_by"].([]any); len(paths) != 1 ||
		paths[0] != "providers.llm.default.auth.token" {
		t.Errorf("read_by = %v, want the path that reads it", got["read_by"])
	}
	reload, _ := got["reload"].(map[string]any)
	if reload["revision_id"] != "r-1" || reload["epoch"] != float64(41) {
		t.Errorf("reload = %v, want the revision and epoch the re-activation produced", got["reload"])
	}
	if s := c.summaries(); len(s) != 1 || !strings.Contains(s[0], "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("reloads = %q, want one naming the secret", s)
	}

	// A NAME NOTHING READS changes nothing that runs, so nothing re-activates
	// and the answer says so with null rather than leaving the field out.
	code, body = call(t, h, http.MethodPut, "/secrets/UNREAD", "v")
	if code != http.StatusOK {
		t.Fatalf("PUT of an unread name = %d %s", code, body)
	}
	got = decoded(t, body)
	if reload, present := got["reload"]; !present || reload != nil {
		t.Errorf("reload = %v (present %v), want null", reload, present)
	}
	if paths, _ := got["read_by"].([]any); paths == nil || len(paths) != 0 {
		t.Errorf("read_by = %v, want an empty list", got["read_by"])
	}
	if s := c.summaries(); len(s) != 1 {
		t.Errorf("an unread name re-activated: %q", s)
	}

	// A REMOVAL of a read name signs whatever read it out, so it
	// re-activates too; one of a name that was not there changes nothing.
	code, body = call(t, h, http.MethodDelete, "/secrets/CLAUDE_CODE_OAUTH_TOKEN", "")
	if code != http.StatusOK || decoded(t, body)["reload"] == nil {
		t.Errorf("DELETE of a read name = %d %s, want a reload", code, body)
	}
	call(t, h, http.MethodDelete, "/secrets/CLAUDE_CODE_OAUTH_TOKEN", "")
	if s := c.summaries(); len(s) != 2 || !strings.Contains(s[1], "removed") {
		t.Errorf("reloads = %q, want the write's and the one removal's", s)
	}
}

// A BATCH IS ONE ACTIVATION.
//
// Rotating every credential one entry reads, one request each, moved the fleet
// through as many epochs, each running a mix of the old and the new values.
// `PUT /secrets` seals all of them and re-activates once.
func TestABatchIsOneActivation(t *testing.T) {
	t.Parallel()
	c := &company{reads: map[string][]string{
		"AWS_ACCESS_KEY_ID":     {"providers.llm.bedrock.env.AWS_ACCESS_KEY_ID"},
		"AWS_SECRET_ACCESS_KEY": {"providers.llm.bedrock.env.AWS_SECRET_ACCESS_KEY"},
	}}
	h := over(t, c)
	code, body := call(t, h, http.MethodPut, "/secrets", `{"values": {
		"AWS_ACCESS_KEY_ID": "AKIA-not-real", "AWS_SECRET_ACCESS_KEY": "s3cret",
		"UNRELATED": "x"}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /secrets = %d %s", code, body)
	}
	if s := c.summaries(); len(s) != 1 ||
		!strings.Contains(s[0], "AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("reloads = %q, want ONE naming both read secrets", s)
	}
	var got struct {
		Names  []string            `json:"names"`
		Bytes  map[string]int      `json:"bytes"`
		ReadBy map[string][]string `json:"read_by"`
		Reload map[string]any      `json:"reload"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Names, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "UNRELATED"}) {
		t.Errorf("names = %v", got.Names)
	}
	if got.Bytes["AWS_SECRET_ACCESS_KEY"] != len("s3cret") {
		t.Errorf("bytes = %v", got.Bytes)
	}
	if len(got.ReadBy["UNRELATED"]) != 0 || len(got.ReadBy["AWS_ACCESS_KEY_ID"]) != 1 {
		t.Errorf("read_by = %v", got.ReadBy)
	}
	if got.Reload["epoch"] != float64(41) {
		t.Errorf("reload = %v", got.Reload)
	}
	for name, want := range map[string]string{"AWS_ACCESS_KEY_ID": "AKIA-not-real",
		"UNRELATED": "x"} {
		code, body := call(t, h, http.MethodGet, "/secrets/"+name+"?reveal=true", "")
		if code != http.StatusOK || decoded(t, body)["value"] != want {
			t.Errorf("%s = %d %s, want it sealed", name, code, body)
		}
	}
}

// A BATCH WITH ANY BAD NAME OR VALUE STORES NOTHING, and names every one.
//
// A rotation that sealed three of four credentials is a company signing in
// with a mix of the old and the new values, which is the state a batch exists
// to avoid.
func TestABatchIsRefusedWholeNamingEveryFault(t *testing.T) {
	t.Parallel()
	c := &company{}
	h := over(t, c)
	tooLong := strings.Repeat("x", 64<<10+1)
	code, body := call(t, h, http.MethodPut, "/secrets", `{"values": {
		"GOOD": "v", "bad-name": "v", "LONG": "`+tooLong+`"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s, want 400", code, body)
	}
	got := decoded(t, body)
	if got["error"] != string(httpjson.CodeInvalidBody) {
		t.Errorf("error = %v", got["error"])
	}
	invalid, _ := got["invalid"].(map[string]any)
	if _, ok := invalid["bad-name"]; !ok {
		t.Errorf("invalid = %v, want the bad name", invalid)
	}
	if _, ok := invalid["LONG"]; !ok {
		t.Errorf("invalid = %v, want the oversized value", invalid)
	}
	if code, _ := call(t, h, http.MethodGet, "/secrets/GOOD", ""); code != http.StatusNotFound {
		t.Errorf("GOOD = %d after a refused batch, want nothing stored", code)
	}

	for _, tc := range []struct{ name, body string }{
		{"empty", `{"values": {}}`},
		{"unknown key", `{"values": {"A": "b"}, "source": "x"}`},
		{"not an object", `["A"]`},
	} {
		if code, body := call(t, h, http.MethodPut, "/secrets", tc.body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tc.name, code, body)
		}
	}
	var many strings.Builder
	many.WriteString(`{"values": {`)
	for i := range secretsapi.MaxRevealNames + 1 {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`"N` + strings.Repeat("A", i+1) + `": "v"`)
	}
	many.WriteString("}}")
	if code, body := call(t, h, http.MethodPut, "/secrets", many.String()); code != http.StatusBadRequest {
		t.Errorf("65 names: %d %s, want 400", code, body)
	}
	if code, body := call(t, h, http.MethodPut, "/secrets?source=cli", `{"values":{"A":"b"}}`); code != http.StatusBadRequest {
		t.Errorf("a query: %d %s, want 400", code, body)
	}
	if s := c.summaries(); len(s) != 0 {
		t.Errorf("a refused batch re-activated: %q", s)
	}
}

// A RELOAD THAT FAILS STILL ANSWERS 200 WITH THE SEALED VALUE.
//
// The value is sealed before the reload is asked for, and crewlet-operator
// reads exactly 200 from this route: a reload that lost a race is said in
// `reload.error` beside a write that happened, never as a refusal of it.
func TestAReloadFailureStillAnswers200WithTheSealedValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		refuse error
		code   httpjson.Code
	}{
		{"raced", &configapi.RacedError{Base: "r-1", Current: "r-2", Stored: "r-3", Node: "node-1"},
			httpjson.CodeRevisionAdvanced},
		{"unrunnable", &configapi.ValidationError{Err: errors.New("seat x names no model")},
			httpjson.CodeValidationError},
		{"no company", configapi.ErrNoActiveRevision, httpjson.CodeNoActiveRevision},
		{"anything else", errors.New("store down"), httpjson.CodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &company{reads: map[string][]string{"KEY": {"providers.llm.k.auth.api_key"}},
				refuse: tc.refuse}
			h := over(t, c)
			code, body := call(t, h, http.MethodPut, "/secrets/KEY", "new-value")
			if code != http.StatusOK {
				t.Fatalf("PUT = %d %s, want 200 whatever the reload did", code, body)
			}
			reload, _ := decoded(t, body)["reload"].(map[string]any)
			if reload["error"] != string(tc.code) || reload["detail"] == "" {
				t.Errorf("reload = %v, want error %s with a sentence", reload, tc.code)
			}
			if _, ok := reload["revision_id"]; ok {
				t.Errorf("reload = %v names a revision it did not activate", reload)
			}
			code, body = call(t, h, http.MethodGet, "/secrets/KEY?reveal=true", "")
			if code != http.StatusOK || decoded(t, body)["value"] != "new-value" {
				t.Errorf("the value = %d %s, want it sealed", code, body)
			}
		})
	}
}

// A DEFERRED BATCH SEALS AND RE-ACTIVATES NOTHING, so a writer that seals one
// value at a time and has each durable at once (a provisioning run) moves the
// fleet onto one epoch, with its last write.
func TestADeferredBatchSealsAndReactivatesNothing(t *testing.T) {
	t.Parallel()
	c := &company{reads: map[string][]string{"GITLAB_TOKEN_SWE": {"roles[0].integrations.gitlab.token"}}}
	h := over(t, c)
	code, body := call(t, h, http.MethodPut, "/secrets",
		`{"values": {"GITLAB_TOKEN_SWE": "glpat-1"}, "defer_reload": true}`)
	if code != http.StatusOK {
		t.Fatalf("deferred = %d %s", code, body)
	}
	got := decoded(t, body)
	if reload, _ := got["reload"].(map[string]any); reload["deferred"] != true || len(c.summaries()) != 0 {
		t.Errorf("a deferred write = reload %v, reloads %q; want it said deferred and none made",
			got["reload"], c.summaries())
	}
	if readBy, _ := got["read_by"].(map[string]any); readBy["GITLAB_TOKEN_SWE"] == nil {
		t.Errorf("read_by = %v, want what the later re-activation reaches", got["read_by"])
	}
	if code, body := call(t, h, http.MethodGet, "/secrets/GITLAB_TOKEN_SWE?reveal=true", ""); code != http.StatusOK ||
		decoded(t, body)["value"] != "glpat-1" {
		t.Errorf("the deferred value = %d %s, want it sealed", code, body)
	}
}
