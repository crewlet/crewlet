package engine

import (
	"encoding/base64"
	"strings"
	"testing"
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
	dial := mirrorDial(true, func(string) (string, bool) { return "", false })
	_, err := dial(t.Context(), "https://127.0.0.1:1/r.git", "${MIRROR_TOKEN}")
	if err == nil || !strings.Contains(err.Error(), "MIRROR_TOKEN") ||
		!strings.Contains(err.Error(), "crewlet secrets set") {
		t.Fatalf("a credential resolving to nothing dialed with %v; want it refused naming MIRROR_TOKEN", err)
	}
}
