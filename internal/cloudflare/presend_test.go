package cloudflare_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/setup"
)

// A MALFORMED PASTE IS REFUSED BEFORE ANYTHING IS SENT, by the field's own
// rule: a stray character or one outside the token alphabet.
func TestTheProbeRefusesAMalformedTokenBeforeSendingIt(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{
		"a wrapped paste":          "cfat_abcdef ghijkl",
		"a header and its value":   "Authorization: Bearer cfat_abcdef",
		"a character no token has": "cfat_abc!def",
	} {
		f := newFakeCloudflare()
		_, err := probe(t, f, map[string]string{"account_id": testAccount, "api_token": token})
		if !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("%s: %v, want it refused as unusable input", name, err)
		}
		if strings.Contains(err.Error(), "abcdef") {
			t.Errorf("%s: the refusal %q quotes the token", name, err)
		}
		if len(f.requests) != 0 {
			t.Errorf("%s: sent %v", name, f.requests)
		}
	}
	for _, r := range cloudflare.Requirements(nil, nil) {
		if r.Field == "api_token" && r.Shape != setup.ShapeOpaqueToken {
			t.Errorf("the provisioner token takes shape %q, so the form stores what the Test button refuses", r.Shape)
		}
	}
}
