package aws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

// THE BOX'S PROFILE READS THE FILE THE POLL REWRITES, from the box's home, in
// the home Region, and never as the default profile.
func TestTheBoxProfileReadsTheRewrittenFile(t *testing.T) {
	t.Parallel()
	got := BoxConfig("eu-west-1")
	for _, want := range []string{"[profile crewlet]\n", "region = eu-west-1\n",
		`credential_process = sh -c 'cat "$HOME/.aws/crewlet-credentials.json"'`} {
		if !strings.Contains(got, want) {
			t.Errorf("the config lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[default]") || strings.Contains(got, "~") {
		t.Errorf("the config writes the default profile or a path credential_process cannot expand:\n%s", got)
	}
}

// A SESSION PRINTS AS CREDENTIAL_PROCESS READS IT, its expiry included so the
// CLI reads it again before it ends; a session with no expiry is refused.
func TestASessionPrintsAsCredentialProcessReadsIt(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 11, 3, 21, 15, 0, 0, time.FixedZone("x", -7*3600))
	c := awssdk.Credentials{AccessKeyID: "ASIA", SecretAccessKey: "s", SessionToken: "t", CanExpire: true, Expires: exp}
	got, err := CredentialProcess(c)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out["Version"] != float64(1) || out["AccessKeyId"] != "ASIA" || out["SessionToken"] != "t" ||
		out["Expiration"] != "2026-11-04T04:15:00Z" {
		t.Errorf("%s", got)
	}
	c.CanExpire = false
	if _, err := CredentialProcess(c); err == nil {
		t.Error("a session with no expiry was printed")
	}
}
