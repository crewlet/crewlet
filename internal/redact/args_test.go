package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// A CALL'S ARGUMENTS AS A PERSON IS SHOWN THEM HOLD NO SECRET AND STAY ONE
// OBJECT. A credential in a string anywhere in them, a value under a
// password's key, a number under one, an AWS secret its key alone names,
// values under a container a password's key names, and keys that are
// credentials themselves are each redacted as values: the text shown is one
// JSON object with every other value as it was, and two keys a redaction makes
// one keep both values.
//
// Mutation: read a value without its key (no takenByKey), and the AWS secret
// is kept and the password's marker is not redact's; drop the renaming, and
// one of two token keys' values is lost.
func TestACallsArgumentsAsShownHoldNoSecretAndStayOneObject(t *testing.T) {
	t.Parallel()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	aws := strings.TrimPrefix(sample["aws-secret-access-key"], "aws_secret_access_key = ")
	password := strings.TrimPrefix(sample["password"], "password: ")
	key := sample["anthropic-key"]
	args := map[string]any{
		"command":     "curl -H 'x-api-key: " + key + "' https://example.com",
		"password":    password,
		"db_pwd":      json.Number("9876543210987"),
		"credentials": map[string]any{"SecretAccessKey": aws, "AccessKeyId": "AKIDEXAMPLE"},
		"passwords":   map[string]any{"staging": password, "list": []any{password}},
		"tokens":      map[string]any{sample["github-token"]: "one", "ghp_" + strings.Repeat("Zz9", 12): "two"},
		"path":        "internal/a.go",
		"count":       json.Number("12345678901234567890"),
	}
	text, err := plainjson.Marshal(redact.Args(args), "")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(text)
	for _, secret := range []string{aws, password, key, "9876543210987", sample["github-token"], "ghp_Zz9Zz9Zz9"} {
		if strings.Contains(raw, secret[:min(len(secret), 12)]) {
			t.Errorf("the arguments hold %q: %s", secret[:12], raw)
		}
	}
	var back map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&back); err != nil || dec.More() {
		t.Fatalf("the arguments are no longer one object: %s", raw)
	}
	tokens, _ := back["tokens"].(map[string]any)
	if back["path"] != "internal/a.go" || back["count"] != json.Number("12345678901234567890") ||
		len(tokens) != 2 || back["password"] != "[REDACTED:password]" ||
		back["credentials"].(map[string]any)["AccessKeyId"] != "AKIDEXAMPLE" {
		t.Errorf("the arguments kept %s, want every other value as it was and both tokens' values", raw)
	}
	if args["password"] != password {
		t.Error("the call's own arguments were changed: the caller's copy must stay as it came")
	}
}
