package iamapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A RESET LINK IS SHOWN ONCE, IS A CREDENTIAL ON THE PERSON, AND REVOKES THE ONE
// BEFORE IT.
//
// The link is the dashboard's reset screen with `<credential id>.<secret>` in
// the fragment, built from api.external_url; the person's document gains a
// `reset` credential whose verifier is the SHA-256 the secret is checked
// against — never the secret — expiring a day out; an earlier live link is
// revoked by the same record, so a person holds one; and the issue is
// announced without the link. Mutation: store the secret as the verifier and
// the verifier check fails; leave the earlier link live and it is still
// unrevoked.
func TestAResetLinkIsShownOnceAndRevokesTheOneBefore(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.document = &iamdomain.Person{
		V: iamdomain.DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
		Credentials: []iamdomain.Credential{
			{ID: "pw", Method: iamdomain.MethodPassword, Verifier: "argon"},
			{ID: "earlier", Method: iamdomain.MethodReset, Verifier: "old",
				ExpiresAt: at.Add(time.Hour)},
		},
	}
	got := r.as(administrator(), http.MethodPost,
		"/iam/people/"+bob.String()+"/password-reset", nil)
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	link, _ := got.body["url"].(string)
	id, _ := got.body["credential"].(string)
	prefix := "https://crewlet.example.com/dashboard#/reset/" + id + "."
	if id == "" || !strings.HasPrefix(link, prefix) {
		t.Fatalf("the link is %q, want the dashboard's reset screen for "+
			"credential %q", link, id)
	}
	secret := strings.TrimPrefix(link, prefix)
	if expires, _ := got.body["expires_at"].(string); expires !=
		at.Add(credential.ResetLinkLifetime).Format(time.RFC3339) {
		t.Errorf("the link expires at %q, want a day out", expires)
	}

	var issued, earlier iamdomain.Credential
	for _, c := range r.writer.document.Credentials {
		switch c.ID {
		case id:
			issued = c
		case "earlier":
			earlier = c
		}
	}
	switch {
	case issued.Method != iamdomain.MethodReset:
		t.Fatalf("the person's credentials are %+v, with no reset link %s",
			r.writer.document.Credentials, id)
	case issued.Verifier == secret ||
		issued.Verifier != credential.ResetVerifier(id, secret):
		t.Errorf("the stored verifier is %q, want the SHA-256 the link's "+
			"secret is checked against", issued.Verifier)
	case !issued.ExpiresAt.Equal(at.Add(credential.ResetLinkLifetime)):
		t.Errorf("the credential expires %s", issued.ExpiresAt)
	}
	if earlier.RevokedAt.IsZero() {
		t.Error("the person's earlier link is still live: they hold two")
	}
	if !strings.HasPrefix(r.writer.updated.OpID, id) {
		t.Errorf("the issue was published under %q, want a fresh step of the "+
			"credential it issues", r.writer.updated.OpID)
	}
	event := only[types.IAMPasswordResetIssued](t, r.audit)
	if event.Person != bob.String() || event.Credential != id || event.By != "founder" {
		t.Errorf("announced %+v", event)
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), secret) {
		t.Errorf("the announcement carries the link's secret: %s", raw)
	}
}

// A RESET LINK IS REFUSED TO A MACHINE AND TO SOMEBODY A RESET DOES NOT REACH.
//
// A machine has no password; a suspended or retired person is reactivated
// first, and the refusal names the stage. Both are 409 and publish nothing.
// The CONTROL is an invited person, whom a reset reaches. Mutation: drop the
// stage check and the suspended person is issued a link.
func TestAResetLinkIsRefusedWhereNoPasswordShouldBeSet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   iam.Kind
		stage  iam.Stage
		status int
	}{
		{"an invited person (the control)", iam.KindPerson, iam.StageInvited,
			http.StatusCreated},
		{"a machine", iam.KindMachine, iam.StageActive, http.StatusConflict},
		{"a suspended person", iam.KindPerson, iam.StageSuspended, http.StatusConflict},
		{"a retired person", iam.KindPerson, iam.StageRetired, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.document = &iamdomain.Person{V: iamdomain.DocumentVersion,
				Kind: tc.kind, Stage: tc.stage}
			got := r.as(administrator(), http.MethodPost,
				"/iam/people/"+bob.String()+"/password-reset", nil)
			if got.status != tc.status {
				t.Fatalf("answered %d, want %d (body %v)", got.status, tc.status,
					got.body)
			}
			if tc.status != http.StatusConflict {
				return
			}
			if tc.kind == iam.KindPerson && got.body["stage"] != string(tc.stage) {
				t.Errorf("the refusal does not name the stage: %v", got.body)
			}
			if len(r.writer.document.Credentials) != 0 || len(r.audit.all()) != 0 {
				t.Errorf("a refused issue left %v and announced %v",
					r.writer.document.Credentials, r.audit.all())
			}
		})
	}
}
