package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// largeHierarchy is a derived hierarchy the size a company of a few hundred
// seats answers with: the part of a config write's answer that grows with the
// company rather than staying an id and an epoch.
func largeHierarchy(seats int) map[string]any {
	list := make([]map[string]any, 0, seats)
	for i := range seats {
		handle := fmt.Sprintf("platform-engineer-%d", i)
		list = append(list, map[string]any{
			"path": fmt.Sprintf("units[3].children[1].roles[%d]", i), "handle": handle,
			"name": "Platform Engineer", "kind": "agent", "unit_path": "units[3].children[1]",
			"placed_by_ref": false, "manager": "cto", "managers": []string{"cto"},
			"reports": nil, "auto_reports": nil, "onboarding_chain": []string{"Engineering", "Platform"},
		})
	}
	return map[string]any{"seats": list, "units": []any{}}
}

// answering is a client whose node answers every request with status and raw.
func answering(t *testing.T, status int, raw []byte) *configClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	return &configClient{base: server.URL, http: httpx.Client(apiTimeout), answerCap: maxConfigResponseBytes}
}

// answeringLarge is answering with a JSON body larger than the 64 KiB the
// client used to read, which is what a large company's answer is.
func answeringLarge(t *testing.T, status int, body map[string]any) *configClient {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 64<<10 {
		t.Fatalf("the answer is %d bytes, which the old bound held: this proves nothing", len(raw))
	}
	return answering(t, status, raw)
}

// A WRITE THAT LANDED IS REPORTED AS LANDED, however large the company.
//
// The answer carries the derived hierarchy of the whole document, and the
// client used to read only the first 64 KiB of it: a company of a few hundred
// seats had its successful import reported as an answer this build could not
// read, which sends an operator to write it again.
func TestAnImportOfALargeCompanyReadsItsWholeAnswer(t *testing.T) {
	t.Parallel()
	client := answeringLarge(t, http.StatusCreated, map[string]any{
		"revision_id": "rev-large", "epoch": 42,
		"warnings": []any{}, "derived": largeHierarchy(400),
	})
	id, epoch, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if id != "rev-large" || epoch != 42 {
		t.Errorf("Import = %q, %d, want rev-large on epoch 42", id, epoch)
	}
}

// AND A REFUSAL OF ONE KEEPS ITS WORDS. A refusal carries a located problem
// per failure beside the same hierarchy; cut short, it decoded to nothing
// and the operator was shown a fragment of JSON instead of the detail.
func TestARefusalOfALargeCompanyKeepsItsDetail(t *testing.T) {
	t.Parallel()
	client := answeringLarge(t, http.StatusBadRequest, map[string]any{
		"error": "validation_error", "detail": "roles[1].llm: value not in the allowed set",
		"hint":     "the whole document a write produces is validated",
		"problems": []any{}, "derived": largeHierarchy(400),
	})
	_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil {
		t.Fatal("a refusal was reported as a write")
	}
	for _, want := range []string{"validation_error", "roles[1].llm", "whole document"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}

// AN IMPORT THAT LOST A RACE SAYS WHAT PUTS IT LIVE FROM WHERE THE OPERATOR
// IS, which is talking to a running node.
//
// This route is taken because the engine holds its store, and `crewlet config
// diff` and `crewlet config activate` open that store directly: the commands
// the refusal used to suggest refused on the very node that had answered. A
// write refused before it stored anything names no revision, and the old
// message sent the operator to activate an empty id.
func TestAnImportThatLostARaceSaysWhatWorksAgainstARunningNode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body       string
		want, deny []string
	}{
		"stored": {
			body: `{"error": "revision_advanced", "current_revision_id": "rev-won", "stored_revision_id": "rev-kept"}`,
			want: []string{"rev-kept", "rev-won", "/config/revisions/rev-kept/diff", "/config/revisions/rev-kept/revert"},
			deny: []string{"crewlet config activate", "crewlet config diff"},
		},
		"nothing stored": {
			body: `{"error": "revision_advanced", "current_revision_id": "rev-won"}`,
			want: []string{"nothing was stored", "rev-won", "import again"},
			deny: []string{"crewlet config activate", "/revert", "revision  "},
		},
	} {
		client := answering(t, http.StatusConflict, []byte(tc.body))
		_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
		if err == nil {
			t.Fatalf("%s: a lost race was reported as a write", name)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q does not say %q", name, err, want)
			}
		}
		for _, deny := range tc.deny {
			if strings.Contains(err.Error(), deny) {
				t.Errorf("%s: %q says %q", name, err, deny)
			}
		}
	}
}

// AN ANSWER THAT IS NOT THE ENGINE'S JSON IS READ FOR WHAT IT SAID. The client
// reads a company-sized answer now, and a proxy's page of that size pasted into
// an error is a terminal full of markup around the one sentence it held: its
// title. A page with nothing readable says how large it was instead.
func TestARefusalQuotesWhatAnAnswerThatIsNotJSONSaid(t *testing.T) {
	t.Parallel()
	page := "<html><head><title>502 Bad Gateway</title></head>" +
		strings.Repeat("<div>a gateway error page</div>", 10_000) + "</html>"
	client := answering(t, http.StatusBadGateway, []byte(page))
	_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil {
		t.Fatal("a gateway error was reported as a write")
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") || strings.Contains(err.Error(), "<div>") {
		t.Errorf("the refusal %.300q does not say what the page said", err)
	}

	untitled := "<html>" + strings.Repeat("<div>layout</div>", 1000) + "</html>"
	client = answering(t, http.StatusBadGateway, []byte(untitled))
	_, _, err = client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil || !strings.Contains(err.Error(), "bytes of") || strings.Contains(err.Error(), "<div>") {
		t.Errorf("an untitled page = %v, want its size and none of its markup", err)
	}
}

// AN ANSWER PAST THE CAP IS REFUSED, NOT CLIPPED. A clipped one reached the
// decoder and read as something this build cannot parse, on a write whose
// status said it had landed.
//
// At a cap of a KiB rather than the client's own 64 MiB: the +1 that makes an
// overrun visible and the sentence saying the write landed are the same at any
// size, and building and reading 64 MiB under -race was most of six seconds.
func TestAnAnswerPastTheCapIsRefusedNamingWhatLanded(t *testing.T) {
	t.Parallel()
	const small = 1 << 10
	client := answering(t, http.StatusOK, []byte(strings.Repeat(" ", small+1)))
	client.answerCap = small
	_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil || !strings.Contains(err.Error(), "exceeded") ||
		!strings.Contains(err.Error(), "says the revision was stored") {
		t.Errorf("an over-long answer = %v, want it refused, saying the write landed", err)
	}
	// And the cap is inclusive: an answer of exactly it is read, so the
	// refusal above is the +1 and not an off-by-one.
	exact := answering(t, http.StatusOK, []byte(`{"revision_id":"r-1","epoch":3}`+strings.Repeat(" ", small-31)))
	exact.answerCap = small
	if id, _, err := exact.Import(t.Context(), []byte("name: Acme\n"), "import"); err != nil || id != "r-1" {
		t.Errorf("an answer of exactly the cap = (%q, %v), want it read", id, err)
	}
}

// A CLIENT READS AN ANSWER OF UP TO 64 MiB, the figure maxConfigResponseBytes
// argues for: sixteen times the largest document the route accepts, because
// the derived hierarchy an answer carries restates every seat with its
// relations spelled out. The case above runs at a cap of its own, so this is
// what ties the client a command builds to the real one.
func TestAConfigClientReadsSixteenDocumentsOfAnswer(t *testing.T) {
	t.Parallel()
	if maxConfigResponseBytes != 16*configapi.MaxBodyBytes {
		t.Errorf("maxConfigResponseBytes = %d, want 16 × configapi.MaxBodyBytes (%d)",
			maxConfigResponseBytes, 16*configapi.MaxBodyBytes)
	}
	boot := &config.Bootstrap{}
	boot.API.Auth.Disabled = true
	client, err := newConfigClient(boot, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("newConfigClient: %v", err)
	}
	if client.answerCap != maxConfigResponseBytes {
		t.Errorf("a command's client reads %d bytes of answer, want maxConfigResponseBytes (%d)",
			client.answerCap, maxConfigResponseBytes)
	}
}

// A CLIENT WITH NO CAP IS REFUSED rather than sent: a cap of zero would refuse
// every answer, so a write that landed would be reported as one that did not.
func TestAConfigClientWithNoCapSendsNothing(t *testing.T) {
	t.Parallel()
	var sent atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Store(true)
		_, _ = w.Write([]byte(`{"revision_id":"r-1","epoch":3}`))
	}))
	t.Cleanup(server.Close)
	client := &configClient{base: server.URL, http: httpx.Client(apiTimeout)}
	if _, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import"); err == nil ||
		!strings.Contains(err.Error(), "no answer cap") {
		t.Errorf("a client with no cap = %v, want it refused", err)
	}
	if sent.Load() {
		t.Error("a client with no cap sent the write anyway")
	}
}

// A 404 AT PUT /config IS A MISSING SURFACE, and which one it is decides where
// the operator goes next.
//
// The route answers no 404 of its own. So the engine router's `no_route` is a
// Crewlet node without the ingress role — one that binds api.port for its probes
// and tool bridge alone — and a 404 carrying no engine code is something that is not a
// node's API at all. Each is named for what it is: a node missing a role is
// never called "not a Crewlet node", and a proxy is never sent looking for a
// role.
func TestA404AtConfigNamesWhatAnswered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    string
		mustNot string
	}{
		{"a node without ingress", `{"error":"no_route","detail":"this node serves nothing at PUT /config"}`,
			"without the ingress role", "not a Crewlet node"},
		{"something else at the address", "404 page not found",
			"not a Crewlet node's API", "without the ingress role"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := answering(t, http.StatusNotFound, []byte(tt.body))
			_, _, err := c.Import(t.Context(), []byte("name: Nimbus\n"), "test")
			if err == nil {
				t.Fatalf("a 404 at PUT /config answered no error")
			}
			if !strings.Contains(err.Error(), "no /config surface") ||
				!strings.Contains(err.Error(), tt.want) {
				t.Errorf("the 404 was reported as %q, want the missing surface and %q",
					err, tt.want)
			}
			if strings.Contains(err.Error(), tt.mustNot) {
				t.Errorf("the 404 was reported as %q, which says %q", err, tt.mustNot)
			}
		})
	}
}
