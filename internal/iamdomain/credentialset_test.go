package iamdomain_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A CREDENTIAL SET THAT REFUSES IN ITS SNAPSHOT PUBLISHES NOTHING.
//
// A rule about the set a write LANDS on — an enrolment-only session enrolling
// over a second factor its person has come to hold since it opened — can only
// be decided in the snapshot the write is formed in: decided from a read made
// first, it is decided on a set that may have moved. So [CredentialSet.Apply]
// may refuse, and the refusal comes back as the write's error, unwrapped, with
// no record on the log.
//
// The CONTROL is the same write whose Apply forms a set, which lands. Mutation:
// ignore Apply's error and publish what it returned, and the refused row puts a
// record on the log and answers no error.
func TestACredentialSetThatRefusesInItsSnapshotPublishesNothing(t *testing.T) {
	t.Parallel()
	errRefused := errors.New("the set this write would land on is not one it may")
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{"an Apply that refuses", true},
		{"an Apply that forms a set (the control)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			const id = "018f3a9c-0000-7000-8000-00000000a0c2"
			enrolSarah(t, rig, id)
			before, err := rig.log.End(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = rig.during(func() error {
				_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
					PersonID: id, OpID: "op-refusal", Reason: "enrolled a second factor",
					Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
						if tc.refuse {
							return nil, errRefused
						}
						return append(held, iamdomain.Credential{
							V: iamdomain.DocumentVersion, ID: "app",
							Method: iamdomain.MethodTOTP, Verifier: "sealed",
						}), nil
					},
				})
				return err
			})
			after, endErr := rig.log.End(t.Context())
			if endErr != nil {
				t.Fatal(endErr)
			}
			switch {
			case tc.refuse && !errors.Is(err, errRefused):
				t.Errorf("the refused write answered %v, want the refusal", err)
			case tc.refuse && after != before:
				t.Errorf("the refused write moved the log from %d to %d", before, after)
			case !tc.refuse && (err != nil || after == before):
				t.Errorf("the control answered %v and moved the log from %d to %d",
					err, before, after)
			}
		})
	}
}
