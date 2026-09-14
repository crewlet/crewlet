package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	return &configClient{base: server.URL, http: httpx.Client(apiTimeout)}
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

// AN ANSWER THAT IS NOT THE ENGINE'S JSON IS QUOTED, AND ONLY SO MUCH OF IT.
// The client reads a company-sized answer now, and a proxy's page of that size
// pasted whole into an error is a terminal full of markup.
func TestARefusalQuotesAnAnswerThatIsNotJSONBoundedly(t *testing.T) {
	t.Parallel()
	page := "<html>" + strings.Repeat("a gateway error page ", 10_000) + "</html>"
	client := answering(t, http.StatusBadGateway, []byte(page))
	_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil {
		t.Fatal("a gateway error was reported as a write")
	}
	if !strings.Contains(err.Error(), "<html>a gateway error page") {
		t.Errorf("the refusal %q does not quote the answer", err)
	}
	if len(err.Error()) > maxRefusalTextBytes+512 {
		t.Errorf("the refusal is %d bytes long, want the answer quoted to %d", len(err.Error()), maxRefusalTextBytes)
	}
}
