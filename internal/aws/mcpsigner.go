package aws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sigv4"
)

// AWS's MCP Server takes no bearer token: each request is signed with
// Signature Version 4 for the service "aws-mcp", in the Region of the endpoint
// it is sent to, and AWS authorizes every AWS call a tool makes as the signer.
// The engine signs with the agent's own session, which never leaves it.

// mcpService is the signing name of AWS's MCP Server.
const mcpService = "aws-mcp"

// mcpHost is an AWS MCP Server endpoint's host; its Region is the signing one.
var mcpHost = regexp.MustCompile(`^aws-mcp\.([a-z0-9-]+)\.api\.aws$`)

// mcpEndpointRegions are where AWS runs its MCP Server, as read live; from any
// other home Region the server at us-east-1 is used.
var mcpEndpointRegions = map[string]bool{"us-east-1": true, "us-west-2": true, "ap-southeast-1": true,
	"ap-southeast-2": true, "ap-northeast-1": true, "eu-central-1": true, "eu-west-1": true, "eu-west-2": true}

// MCPEndpoint is the AWS MCP Server an agent working in home reaches.
func MCPEndpoint(home string) string {
	region := home
	if !mcpEndpointRegions[region] {
		region = "us-east-1"
	}
	return "https://aws-mcp." + region + ".api.aws/mcp"
}

// MCPSigner signs requests to AWS's MCP Server as one agent's session. A
// VALUE, compared with == by the server's spec: a signer holding the same
// session is the same, and a renewed session restarts the server with the
// new one, as every issued tool credential does.
type MCPSigner struct {
	creds sigv4.Credentials
	// home is the Region the agent works in, which each message carries
	// as its metadata, as AWS's own proxy sends it.
	home string
}

// NewMCPSigner is the signer for the session credential_process prints
// ([CredentialProcess]), as the agent's issued variable holds it.
func NewMCPSigner(session, home string) (MCPSigner, error) {
	var c struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
	}
	if err := json.Unmarshal([]byte(session), &c); err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return MCPSigner{}, errors.New("aws: the agent's session is not one the engine issued")
	}
	return MCPSigner{creds: sigv4.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey,
		SessionToken: c.SessionToken}, home: home}, nil
}

// Sign adds the agent's Region to the message's metadata and signs the
// request over every header it carries.
func (s MCPSigner) Sign(_ context.Context, req *http.Request, body []byte) ([]byte, error) {
	m := mcpHost.FindStringSubmatch(req.URL.Hostname())
	if m == nil {
		return nil, errors.New("aws: requests are signed for AWS's MCP Server alone")
	}
	body = withRegion(body, s.home)
	now := time.Now().UTC()
	req.Header.Set("X-Amz-Date", now.Format(sigv4.DateFormat))
	if s.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	signed := http.Header{}
	for k, v := range req.Header {
		switch strings.ToLower(k) {
		case "authorization", "user-agent", "accept-encoding", "content-length":
		default:
			signed[k] = v
		}
	}
	req.Header.Set("Authorization", sigv4.Sign(sigv4.Request{
		Method: req.Method, Host: req.URL.Host, Path: req.URL.Path, Query: req.URL.Query(), Header: signed,
		PayloadHash: sigv4.PayloadHash(body),
	}, s.creds, m[1], mcpService, now))
	return body, nil
}

// withRegion sets params._meta.AWS_REGION on each JSON-RPC request and
// notification in body, a message or a batch, where it is not set. Anything
// else is sent as it came.
func withRegion(body []byte, region string) []byte {
	if region == "" {
		return body
	}
	var one map[string]any
	if json.Unmarshal(body, &one) == nil {
		if !addRegion(one, region) {
			return body
		}
		out, err := json.Marshal(one)
		if err != nil {
			return body
		}
		return out
	}
	var batch []map[string]any
	if json.Unmarshal(body, &batch) != nil {
		return body
	}
	changed := false
	for _, msg := range batch {
		changed = addRegion(msg, region) || changed
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(batch)
	if err != nil {
		return body
	}
	return out
}

// addRegion sets the Region on one message, reporting a change.
func addRegion(msg map[string]any, region string) bool {
	if _, isCall := msg["method"]; !isCall {
		return false
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	if _, set := meta["AWS_REGION"]; set {
		return false
	}
	meta["AWS_REGION"] = region
	params["_meta"] = meta
	msg["params"] = params
	return true
}
