package iamdomain_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A CREDENTIAL'S CARRIED FIELDS SURVIVE THE PERSON'S RECORD THEY TRAVEL IN.
//
// A credential travels inside its person's document, and what a second factor
// needs beyond its verifier rides in the credential's carried fields: the
// recovery codes' verifiers, and the last TOTP step accepted, which is what
// makes a code single-use. encoding/json marshalled the nested credential by
// its fields alone, so both were dropped on the way to the log: a person who
// had saved recovery codes could never sign in with one, and one code opened
// as many sessions as it was presented in its window.
//
// The write goes through the rig — the writer, the broker and the applier —
// and is read back the way a sign-in reads it. Mutation: delete
// [iamdomain.Credential.MarshalJSON] and both fields come back empty.
func TestACredentialsCarriedFieldsSurviveThePersonsRecordTheyTravelIn(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const id = "018f3a9c-0000-7000-8000-00000000c0a1"
	enrolSarah(t, rig, id)
	if err := rig.draining(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: id, OpID: "op-second-factor", Reason: "enrolled a second factor",
			Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
				return append(held,
					iamdomain.Credential{
						V: iamdomain.DocumentVersion, ID: "app",
						Method: iamdomain.MethodTOTP, Verifier: "sealed",
						Extra: map[string]json.RawMessage{"last_step": json.RawMessage(`58000000`)},
					},
					iamdomain.Credential{
						V: iamdomain.DocumentVersion, ID: "codes",
						Method: iamdomain.MethodRecovery,
						Extra:  map[string]json.RawMessage{"verifiers": json.RawMessage(`["a","b"]`)},
					}), nil
			},
		})
		return err
	}); err != nil {
		t.Fatalf("set the credentials: %v", err)
	}
	rig.drain()

	held, err := rig.reader(t).PersonByLogin(t.Context(), "sarah.chen")
	if err != nil {
		t.Fatalf("read the person: %v", err)
	}
	carried := map[string]string{}
	for _, c := range held.Credentials {
		for name, value := range c.Extra {
			carried[c.ID+"."+name] = string(value)
		}
	}
	for name, want := range map[string]string{
		"app.last_step":   `58000000`,
		"codes.verifiers": `["a","b"]`,
	} {
		if carried[name] != want {
			t.Errorf("%s reads %q after the write, want %q (carried: %v)",
				name, carried[name], want, carried)
		}
	}
}

// A PERSON'S CARRIED FIELDS SURVIVE EVERY RECORD THAT NESTS THEM.
//
// A person's document is the payload of an enrolment and of a password change
// as well as of a content record, and what it carries has to round-trip from
// each — the person's own and every credential's on it. Mutation: delete
// [iamdomain.Person.UnmarshalJSON] and every record loses the person's carried
// field and its credential's.
func TestAPersonsCarriedFieldsSurviveEveryRecordThatNestsThem(t *testing.T) {
	t.Parallel()
	person := iamdomain.Person{
		V: iamdomain.DocumentVersion,
		Credentials: []iamdomain.Credential{{
			V: iamdomain.DocumentVersion, ID: "codes", Method: iamdomain.MethodRecovery,
			Extra: map[string]json.RawMessage{"verifiers": json.RawMessage(`["a"]`)},
		}},
		Extra: map[string]json.RawMessage{"tier": json.RawMessage(`"gold"`)},
	}
	for _, tc := range []struct {
		name string
		trip func() (iamdomain.Person, error)
	}{
		{"the person's own record", func() (iamdomain.Person, error) {
			data, err := iamdomain.EncodePerson(person)
			if err != nil {
				return iamdomain.Person{}, err
			}
			return iamdomain.DecodePerson(data)
		}},
		{"an enrolment", func() (iamdomain.Person, error) {
			data, err := iamdomain.EncodeEnrolled(iamdomain.Enrolled{
				V: iamdomain.DocumentVersion, Person: person,
			})
			if err != nil {
				return iamdomain.Person{}, err
			}
			got, err := iamdomain.DecodeEnrolled(data)
			return got.Person, err
		}},
		{"a password change", func() (iamdomain.Person, error) {
			data, err := iamdomain.EncodePasswordChange(iamdomain.PasswordChange{
				V: iamdomain.DocumentVersion, Person: person,
			})
			if err != nil {
				return iamdomain.Person{}, err
			}
			got, err := iamdomain.DecodePasswordChange(data)
			return got.Person, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.trip()
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if string(got.Extra["tier"]) != `"gold"` {
				t.Errorf("the person carries %v, want tier", got.Extra)
			}
			if len(got.Credentials) != 1 ||
				string(got.Credentials[0].Extra["verifiers"]) != `["a"]` {
				t.Errorf("the credentials read back as %+v, want codes "+
					"carrying its verifiers", got.Credentials)
			}
		})
	}
}
