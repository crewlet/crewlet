package iamdomain_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SEALED SEED IS THE ONLY FORM OF IT ANY ROW HOLDS.
//
// A second factor's seed is the one credential this estate keeps as a secret,
// and every row it touches — the credential's own, the person's document, the
// trail entry the write leaves — is replicated to every node, snapshotted,
// backed up and donated. Sealed by the writer under the person's key, the
// applier writes it through as bytes it cannot read, so none of the three holds
// the seed; and the credential's row opens back to it as that credential.
//
// The CONTROL is the same write with the seed in the clear, which the same
// scan finds in all three — so a clean scan says the seed was sealed, not that
// the scan read the wrong rows. Mutation: store the seed in the clear in the
// first half and it fails the way the control does.
func TestASealedSeedIsTheOnlyFormOfItAnyRowHolds(t *testing.T) {
	t.Parallel()
	const seed = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	for _, tc := range []struct {
		name   string
		sealed bool
	}{
		{"sealed by the writer", true},
		{"in the clear (the control)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			const id = "018f3a9c-0000-7000-8000-00000000a0b1"
			enrolSarah(t, rig, id)
			sealer, err := iamdomain.NewSealer(rig.keys)
			if err != nil {
				t.Fatal(err)
			}
			stored := seed
			if tc.sealed {
				if stored, err = sealer.SealCredential(t.Context(), id, "app",
					iamdomain.FieldTOTP, seed); err != nil {
					t.Fatalf("seal: %v", err)
				}
			}
			if err := rig.during(func() error {
				_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
					PersonID: id, OpID: "op-enrol-app", Reason: "enrolled a second factor",
					Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
						return append(held, iamdomain.Credential{
							V: iamdomain.DocumentVersion, ID: "app",
							Method: iamdomain.MethodTOTP, Verifier: stored,
						})
					},
				})
				return err
			}); err != nil {
				t.Fatalf("set credentials: %v", err)
			}

			rows := map[string][]string{
				"iam_credentials.verifier": rig.column(
					`SELECT verifier FROM iam_credentials WHERE person_id = ?`, id),
				"iam_people.document": rig.column(
					`SELECT document FROM iam_people WHERE id = ?`, id),
				"iam_history.document": rig.column(
					`SELECT document FROM iam_history WHERE person_id = ?`, id),
			}
			for table, values := range rows {
				if len(values) == 0 {
					t.Fatalf("%s holds no row for the person; the scan reads "+
						"nothing", table)
				}
				held := strings.Contains(strings.Join(values, "\n"), seed)
				switch {
				case tc.sealed && held:
					t.Errorf("%s holds the seed in the clear", table)
				case !tc.sealed && !held:
					t.Errorf("%s does not hold a seed written in the clear, so "+
						"this scan cannot see one", table)
				}
			}
			if !tc.sealed {
				return
			}
			verifier := rig.column(`SELECT verifier FROM iam_credentials
				WHERE person_id = ? AND method = ?`, id, string(iamdomain.MethodTOTP))
			if len(verifier) != 1 {
				t.Fatalf("the person holds %d second factors, want 1", len(verifier))
			}
			opened, err := sealer.OpenCredential(t.Context(), id, "app",
				iamdomain.FieldTOTP, verifier[0])
			if err != nil || opened != seed {
				t.Errorf("the stored seed opens as (%q, %v), want the seed", opened, err)
			}
		})
	}
}
