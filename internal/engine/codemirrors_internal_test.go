package engine

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A MIRROR'S CREDENTIAL IS SENT AS HTTP BASIC: a value holding a colon is
// `user:password` as the variable writes it, and any other is a token, sent
// as the password beside the user `x-access-token` — the scheme every
// forge's git endpoint accepts, so one variable serves GitHub, GitLab, Gitea
// and a forge wanting a user of its own alike.
//
// Mutation: send a token as the user rather than the password, and the token
// case decodes to "ghp_abc:"; split a value at its colon and drop the user,
// and the user:password case loses "bot".
func TestAMirrorCredentialIsSentAsBasic(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"ghp_abc":    "x-access-token:ghp_abc",
		"bot:s3cret": "bot:s3cret",
	} {
		header := remoteAuthorization(value)
		encoded, ok := strings.CutPrefix(header, "Authorization: Basic ")
		if !ok {
			t.Errorf("the header for %q is %q, want HTTP Basic", value, header)
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || string(got) != want {
			t.Errorf("the header for %q carries %q (%v), want %q", value, got, err, want)
		}
	}
}

// A MIRROR'S CREDENTIAL THAT RESOLVES TO NOTHING IS REFUSED AT THE DIAL,
// naming the variable and where to set it, rather than fetched anonymously —
// a source that answers an anonymous fetch with an empty repository would
// otherwise be mirrored as one.
//
// Mutation: dial without the header where the lookup finds nothing, and the
// dial answers no error.
func TestAMirrorCredentialThatResolvesToNothingIsRefused(t *testing.T) {
	t.Parallel()
	declared := func() config.CodeRemoteCredentials {
		return config.CodeRemoteCredentials{"MIRROR_TOKEN": {Hosts: []string{"127.0.0.1:1"}}}
	}
	dial := mirrorDial(true, declared, func(string) (string, bool) { return "", false })
	_, err := dial(t.Context(), "https://127.0.0.1:1/r.git", "${MIRROR_TOKEN}")
	if err == nil || !strings.Contains(err.Error(), "MIRROR_TOKEN") ||
		!strings.Contains(err.Error(), "crewlet secrets set") {
		t.Fatalf("a credential resolving to nothing dialed with %v; want it refused naming MIRROR_TOKEN", err)
	}
}

// THE DIAL RESOLVES ONLY A CREDENTIAL code.remote_credentials DECLARES FOR THE
// URL'S HOST, under the table the node holds at the dial: a variable the table
// does not declare, or one declared for another host or another port, is
// refused before anything is looked up — so no value of the node's secret
// store or environment is resolved for a host an administrator did not name —
// and a table that withdraws a declaration stops the dial that it admitted a
// moment before. The one declared for the URL's host is resolved and sent.
//
// Mutation: drop the declared().Refusal check from mirrorDial — the
// undeclared variable, the other host and the withdrawn declaration are each
// resolved, red. Compare hosts without their port in
// config.CodeRemoteCredentials.Refusal — the other port is resolved, red.
func TestAMirrorDialResolvesOnlyACredentialDeclaredForItsHost(t *testing.T) {
	t.Parallel()
	table := config.CodeRemoteCredentials{"FORGE_TOKEN": {Hosts: []string{"127.0.0.1:1"}}}
	var looked []string
	dial := mirrorDial(true, func() config.CodeRemoteCredentials { return table },
		func(name string) (string, bool) {
			looked = append(looked, name)
			return "ghp_value", true
		})
	for what, call := range map[string]struct{ url, credential string }{
		"an undeclared variable":         {"https://127.0.0.1:1/r.git", "${CREWLET_SECRET_KEY_K1}"},
		"a declared one to another port": {"https://127.0.0.1:2/r.git", "${FORGE_TOKEN}"},
		"a declared one to another host": {"https://127.0.0.2:1/r.git", "${FORGE_TOKEN}"},
	} {
		looked = nil
		_, err := dial(t.Context(), call.url, call.credential)
		if err == nil || !strings.Contains(err.Error(), "code.remote_credentials") {
			t.Errorf("%s: dialed with %v, want it refused naming code.remote_credentials", what, err)
		}
		if len(looked) > 0 {
			t.Errorf("%s: looked up %v, want nothing resolved", what, looked)
		}
	}
	looked = nil
	if _, err := dial(t.Context(), "https://127.0.0.1:1/r.git", "${FORGE_TOKEN}"); err != nil ||
		len(looked) != 1 || looked[0] != "FORGE_TOKEN" {
		t.Fatalf("the declared credential: %v, looked up %v, want FORGE_TOKEN resolved", err, looked)
	}
	table = config.CodeRemoteCredentials{}
	looked = nil
	if _, err := dial(t.Context(), "https://127.0.0.1:1/r.git", "${FORGE_TOKEN}"); err == nil || len(looked) > 0 {
		t.Errorf("a withdrawn declaration: dialed with %v, looked up %v, want it refused, nothing resolved", err,
			looked)
	}
}
