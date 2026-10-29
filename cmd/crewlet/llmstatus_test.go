package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// statusEntry writes a company whose one entry, "sub", runs `sh` for its
// status: status_args print json and exit with exit, and status_text_args print
// text, as `claude auth status` and `claude auth status --text` do.
func statusEntry(t *testing.T, json string, exit int, text string) []string {
	t.Helper()
	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  sandbox: {fake: true}
  llm:
    sub:
      agent: custom
      model: m
      state_dir: %q
      overrides:
        binary: sh
        complete_args: ["-p"]
        model_args: ["--model", "{model}"]
        output: text
        credential_paths: [".fake/creds.json"]
        status_args: ["-c", %q]
        status_text_args: ["-c", %q]
        status_fields:
          method: [[authMethod]]
          account: [[email], [orgName]]
roles:
  - name: CEO
    handle: ceo
    kind: human
`, filepath.Join(dir, "state"), fmt.Sprintf("printf '%%s' '%s'; exit %d", json, exit),
		fmt.Sprintf("printf '%%s\\n' '%s'; exit %d", text, exit)))
	return []string{"-company", company, "-config", filepath.Join(dir, "absent.yaml")}
}

// `crewlet llm status` LEADS WITH THE ENGINE'S READING and follows it with the
// CLI's own words: one line a script can read (signed in, how and as whom),
// then the text form a person reads; signed out exits non-zero, as the CLI's
// own status command does.
//
// Mutation: print Status alone (no reading line), or exit 0 when signed out,
// and its case is red.
func TestLLMStatusLeadsWithTheEnginesReading(t *testing.T) {
	t.Parallel()
	t.Run("signed in", func(t *testing.T) {
		t.Parallel()
		flags := statusEntry(t, `{"loggedIn": true, "authMethod": "claude.ai", "email": "ops@example.com"}`, 0,
			"Login method: Claude Max account")
		var out, errOut bytes.Buffer
		if err := runLLM(noEnviron, append([]string{"status", "sub"}, flags...), &out, &errOut); err != nil {
			t.Fatalf("status: %v (%s)", err, errOut.String())
		}
		want := "sub: signed in (claude.ai) as ops@example.com\nLogin method: Claude Max account\n"
		if out.String() != want {
			t.Errorf("status printed %q, want %q", out.String(), want)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		t.Parallel()
		flags := statusEntry(t, `{"loggedIn": false, "authMethod": "none"}`, 1, "Not logged in")
		var out, errOut bytes.Buffer
		err := runLLM(noEnviron, append([]string{"status", "sub"}, flags...), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "crewlet llm login sub") {
			t.Errorf("status of a signed-out entry = %v, want a failure naming the login", err)
		}
		if !strings.HasPrefix(out.String(), "sub: signed out\n") {
			t.Errorf("status printed %q, want the reading first", out.String())
		}
	})
}
