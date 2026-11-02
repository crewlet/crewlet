package secretsapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Neither reveal answers a write-only value, the listing marks it, and a write
// through /secrets keeps it unreadable.
func TestAWriteOnlyValueIsNeverRevealed(t *testing.T) {
	t.Parallel()
	cipher := cipherFor(t, "k1")
	h, fleet := surface(t, cipher, "k1")
	const value = "Bearer lin_api_not-a-real-key"
	if err := fleetsecrets.New(fleet, cipher).SetWriteOnly(t.Context(), "MCP_LINEAR_AUTHORIZATION",
		value, secrets.Author{Name: "jane-founder", Kind: "human"}, secrets.SourceMCP, clock); err != nil {
		t.Fatalf("SetWriteOnly: %v", err)
	}
	if code, body := call(t, h, http.MethodPut, "/secrets/GITLAB_TOKEN", "glpat-not-real"); code != http.StatusOK {
		t.Fatalf("seed = %d %s", code, body)
	}

	refused := func(what string, code int, body string, names ...string) {
		t.Helper()
		if code != http.StatusForbidden || !strings.Contains(body, `"error":"`+string(httpjson.CodeWriteOnly)+`"`) {
			t.Fatalf("%s = %d %s, want 403 write_only", what, code, body)
		}
		if strings.Contains(body, "lin_api") || strings.Contains(body, "glpat") {
			t.Fatalf("%s carries a value: %s", what, body)
		}
		var got struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || !slices.Equal(got.Names, names) {
			t.Errorf("%s names %v (%v), want %v", what, got.Names, err, names)
		}
	}
	code, body := call(t, h, http.MethodGet, "/secrets/MCP_LINEAR_AUTHORIZATION?reveal=true", "")
	refused("the single reveal", code, body, "MCP_LINEAR_AUTHORIZATION")
	code, body = call(t, h, http.MethodPost, "/secrets/reveal",
		revealBody(t, "GITLAB_TOKEN", "MCP_LINEAR_AUTHORIZATION"))
	refused("the batched reveal", code, body, "MCP_LINEAR_AUTHORIZATION")

	code, body = call(t, h, http.MethodGet, "/secrets/MCP_LINEAR_AUTHORIZATION", "")
	if code != http.StatusOK || !strings.Contains(body, `"write_only":true`) ||
		!strings.Contains(body, `"source":"mcp"`) {
		t.Errorf("the metadata = %d %s, want the row marked write-only from mcp", code, body)
	}
	code, body = call(t, h, http.MethodGet, "/secrets", "")
	var listed struct {
		Secrets []struct {
			Name      string `json:"name"`
			WriteOnly bool   `json:"write_only"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(body), &listed); code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s (%v)", code, body, err)
	}
	for _, row := range listed.Secrets {
		if row.WriteOnly != (row.Name == "MCP_LINEAR_AUTHORIZATION") {
			t.Errorf("the listing marks %s write-only = %t", row.Name, row.WriteOnly)
		}
	}

	if code, body = call(t, h, http.MethodPut, "/secrets/MCP_LINEAR_AUTHORIZATION",
		"Bearer lin_api_rotated-not-real"); code != http.StatusOK {
		t.Fatalf("rotate = %d %s", code, body)
	}
	code, body = call(t, h, http.MethodGet, "/secrets/MCP_LINEAR_AUTHORIZATION?reveal=true", "")
	refused("a reveal after a rotation through /secrets", code, body, "MCP_LINEAR_AUTHORIZATION")
}
