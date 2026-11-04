package aws

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/crewlet/crewlet/internal/sigv4"
)

// A REQUEST TO AWS'S MCP SERVER IS SIGNED AS THE AGENT, for aws-mcp in the
// endpoint's Region, over the body as sent, which carries the agent's Region in
// each message's metadata; the signature verifies with the agent's secret.
func TestTheMCPSignerSignsAsTheAgent(t *testing.T) {
	t.Parallel()
	session, err := CredentialProcess(awssdk.Credentials{AccessKeyID: "ASIASEAT", SecretAccessKey: "secret",
		SessionToken: "token", CanExpire: true, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewMCPSigner(session, "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://aws-mcp.eu-central-1.api.aws/mcp", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", "s1")
	body, err := s.Sign(context.Background(), req, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_script"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	_ = json.Unmarshal(body, &msg)
	if msg["params"].(map[string]any)["_meta"].(map[string]any)["AWS_REGION"] != "eu-central-1" {
		t.Errorf("the message carries no Region: %s", body)
	}
	at, _ := time.Parse(sigv4.DateFormat, req.Header.Get("X-Amz-Date"))
	hdr := req.Header.Clone()
	hdr.Del("Authorization")
	want := sigv4.Sign(sigv4.Request{Method: http.MethodPost, Host: "aws-mcp.eu-central-1.api.aws", Path: "/mcp",
		Query: req.URL.Query(), Header: hdr, PayloadHash: sigv4.PayloadHash(body)},
		sigv4.Credentials{AccessKeyID: "ASIASEAT", SecretAccessKey: "secret", SessionToken: "token"}, "eu-central-1", "aws-mcp", at)
	if got := req.Header.Get("Authorization"); got != want || !strings.Contains(got, "/eu-central-1/aws-mcp/aws4_request") ||
		!strings.Contains(got, "mcp-session-id") || req.Header.Get("X-Amz-Security-Token") != "token" {
		t.Errorf("Authorization %q, want %q", got, want)
	}
	other, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp", nil)
	if _, err := s.Sign(context.Background(), other, nil); err == nil {
		t.Error("a request to another host was signed")
	}
	again, _ := NewMCPSigner(session, "eu-central-1")
	if s != again {
		t.Error("two signers of one session differ, so the server would restart every pass")
	}
	if MCPEndpoint("ca-central-1") != "https://aws-mcp.us-east-1.api.aws/mcp" ||
		MCPEndpoint("eu-west-1") != "https://aws-mcp.eu-west-1.api.aws/mcp" {
		t.Error("the endpoint does not fall back to us-east-1")
	}
}
