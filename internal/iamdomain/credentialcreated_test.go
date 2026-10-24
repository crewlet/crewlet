package iamdomain_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A CREDENTIAL KEEPS THE INSTANT IT WAS CREATED THROUGH LATER CHANGES.
//
// Every record about a person restates all of their credentials, and the
// applier replaced their rows stamping each with the record's own instant, so
// revoking a token read as having created it — after its own revocation — and
// a password set a year ago read as set a moment ago beside it. The CONTROL is
// the credential the later record adds, which is created then. Mutation:
// stamp every row with the record's instant again and the kept token moves.
func TestACredentialKeepsTheInstantItWasCreatedThroughLaterChanges(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000c0a2"
	enrolSarah(t, rig, id)
	add := func(credential string, revoke string) {
		t.Helper()
		if err := rig.draining(func() error {
			_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
				PersonID: id, OpID: "op-" + credential, Reason: "a change",
				Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
					for i := range held {
						if held[i].ID == revoke {
							held[i].RevokedAt = time.Now().UTC()
						}
					}
					return append(held, iamdomain.Credential{
						V: iamdomain.DocumentVersion, ID: credential,
						Method: iamdomain.MethodTOTP, Verifier: "sealed",
					}), nil
				},
			})
			return err
		}); err != nil {
			t.Fatalf("write %s: %v", credential, err)
		}
		rig.drain()
	}
	created := func() map[string]time.Time {
		t.Helper()
		rows, err := rig.reader(t).Credentials(t.Context(), id)
		if err != nil {
			t.Fatalf("list the credentials: %v", err)
		}
		out := map[string]time.Time{}
		for _, row := range rows {
			out[row.ID] = row.CreatedAt
		}
		return out
	}

	add("first", "")
	before := created()["first"]
	// The broker stamps a record with its own clock, to the millisecond, so
	// the second record is held a few past the first's.
	time.Sleep(5 * time.Millisecond)
	add("second", "first")
	after := created()

	if !after["first"].Equal(before) {
		t.Errorf("the first credential was created at %v and reads %v after a "+
			"later record revoked it", before, after["first"])
	}
	if !after["second"].After(before) {
		t.Errorf("the second credential reads created at %v, want after the "+
			"first's %v", after["second"], before)
	}
}
