package iamdomain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LOG'S DECLARED LARGEST RECORD HOLDS THE LARGEST PERSON THE CAPS ALLOW.
//
// A person's document is the one record here that grows — every credential
// change and every edit republishes it whole — and the log's gate reserve is
// sized by the declaration, so the declaration has to hold the widest
// document a writer can form. It is built at every cap the writer holds a
// person to: a name and an address at theirs, the most credentials one person
// holds, every token carrying every grant, the widest reach and a label of
// characters JSON escapes six-fold, and the envelope beside it — the actor,
// the credential they acted through and a reason — at theirs. A cap raised
// without revisiting [iamdomain.IamMaxRecordBytes] fails here.
//
// AND THE CAP IS WHAT THIS RESTS ON, so it is held too: a write that would
// leave one more drops a credential that has stopped verifying, or is refused
// before anything is published when every one still does.
func TestTheLargestRecordHoldsTheLargestPerson(t *testing.T) {
	t.Parallel()
	limit := iamdomain.Domain{}.Stream().MaxRecordBytes
	sealer := newSealer(t)
	person := who
	seal := func(field iamdomain.Field, n int) string {
		sealed, err := sealer.Seal(person, field, strings.Repeat("x", n))
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return sealed
	}
	doc := iamdomain.Person{
		V: iamdomain.DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
		NameSealed:  seal(iamdomain.FieldName, iamdomain.MaxName),
		EmailSealed: seal(iamdomain.FieldEmail, iamdomain.MaxAddress),
		Credentials: widestCredentials(t, sealer, person),
		Grants:      iam.AllGrants, Colleague: iam.ColleagueWrite,
	}
	if len(doc.Credentials) != iamdomain.MaxHeldCredentials {
		t.Fatalf("built %d credentials, want the cap of %d",
			len(doc.Credentials), iamdomain.MaxHeldCredentials)
	}
	mutation, err := iamdomain.EncodePerson(doc)
	if err != nil {
		t.Fatalf("encode the person: %v", err)
	}
	body, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: statelog.NewOpID(time.Now(), "credentials-revoke"),
			Subject: iamdomain.PersonSubject(person), Op: iamdomain.OpUpdate,
			CreatedAt: time.Now().UTC(), Gen: 1 << 20, Writer: strings.Repeat("n", 64),
			Scope: iamdomain.PeopleScope(person),
		},
		Expect:     math.MaxUint64,
		Mutation:   mutation,
		Person:     person,
		Actor:      strings.Repeat("a", iam.MaxLogin),
		ActorKind:  iam.KindPerson,
		OperatorID: "pat:" + strings.Repeat("f", 36),
		Reason:     strings.Repeat("<", iamdomain.MaxReason),
	})
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	t.Logf("%d bytes of a declared largest of %d", len(body), limit)
	if int64(len(body)) > limit {
		t.Errorf("the widest person the caps allow is a %d-byte record, past "+
			"the identity log's declared largest of %d — raise "+
			"iamdomain.IamMaxRecordBytes with the reserve it sizes, or lower "+
			"the cap that grew", len(body), limit)
	}
}

// widestCredentials is the most credentials one person holds, each at its
// widest: a password, an authenticator, a recovery set, and machine tokens for
// the rest — every one revoked and expiring, so every field is written.
func widestCredentials(t *testing.T, sealer *iamdomain.Sealer, person string) []iamdomain.Credential {
	t.Helper()
	at := time.Now().UTC()
	id := func(i int) string { return fmt.Sprintf("018f3a9c-0000-7000-8000-%012d", i) }
	// THE SHAPE ARGON2ID'S PHC STRING TAKES at this build's cost: the
	// parameters, a sixteen-byte salt and a thirty-two-byte digest.
	phc := "$argon2id$v=19$m=65536,t=3,p=1$" + strings.Repeat("s", 22) + "$" +
		strings.Repeat("h", 43)
	seed, err := sealer.SealCredential(person, id(1), iamdomain.FieldTOTP,
		strings.Repeat("A", 32))
	if err != nil {
		t.Fatalf("seal a seed: %v", err)
	}
	verifiers := make([]string, credential.RecoveryCodeCount)
	for i := range verifiers {
		verifiers[i] = credential.HashRecoveryCode(fmt.Sprintf("CODE-%022d", i))
	}
	rawVerifiers, _ := json.Marshal(verifiers)
	out := []iamdomain.Credential{
		{V: iamdomain.DocumentVersion, ID: id(0), Method: iamdomain.MethodPassword,
			Verifier: phc, RevokedAt: at},
		{V: iamdomain.DocumentVersion, ID: id(1), Method: iamdomain.MethodTOTP,
			Verifier: seed, RevokedAt: at,
			Extra: map[string]json.RawMessage{"last_step": json.RawMessage("99999999999")}},
		{V: iamdomain.DocumentVersion, ID: id(2), Method: iamdomain.MethodRecovery,
			RevokedAt: at, Extra: map[string]json.RawMessage{"verifiers": rawVerifiers}},
	}
	for i := len(out); i < iamdomain.MaxHeldCredentials; i++ {
		out = append(out, iamdomain.Credential{
			V: iamdomain.DocumentVersion, ID: id(i), Method: iamdomain.MethodToken,
			Verifier:  credential.TokenVerifier(id(i), strings.Repeat("S", 52)),
			ExpiresAt: at.Add(credential.MaxTokenLifetime), RevokedAt: at,
			Label:  strings.Repeat("<", iamdomain.MaxTokenLabel),
			Grants: iam.AllGrants, Colleague: iam.ColleagueWrite,
			Epoch: math.MaxUint64, Generation: math.MaxUint64,
		})
	}
	return out
}

// A PERSON HOLDS AT MOST MaxHeldCredentials, AND EVERY WRITER OF THE SET HOLDS
// THEM TO IT.
//
// The declared largest record rests on the cap, so the cap is checked where
// the set is formed — the enrolment, a credential change, an edit of the
// person and a mint — and a write that would leave more LIVE credentials than
// it publishes nothing. The mint is the one a person meets, so its refusal is
// the token's own ([iamdomain.ErrInvalidToken]) and counts the live ones.
// Mutation: drop any one of the four checks and its row goes green past the
// cap.
func TestAPersonHoldsNoMoreCredentialsThanTheCap(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	if err := rig.draining(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: owner, OpID: statelog.NewOpID(time.Now(), "fill"),
			Apply: func([]iamdomain.Credential) ([]iamdomain.Credential, error) {
				return liveTokens(iamdomain.MaxHeldCredentials), nil
			},
		})
		return err
	}); err != nil {
		t.Fatalf("a set at the cap was refused: %v", err)
	}
	end, err := rig.end(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{
		PersonID: owner, Label: "one more", Reason: "a PAT"})
	if want := fmt.Sprintf("leave %d live credentials", iamdomain.MaxHeldCredentials+1); !errors.Is(err, iamdomain.ErrInvalidToken) ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("a mint past the cap answered %v, want ErrInvalidToken saying "+
			"it would %s", err, want)
	}
	for name, write := range map[string]func() error{
		"a credential change": func() error {
			_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
				PersonID: owner, OpID: statelog.NewOpID(time.Now(), "grow"),
				Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
					return liveTokens(len(held) + 1), nil
				},
			})
			return err
		},
		"an edit of the person": func() error {
			_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
				PersonID: owner, OpID: statelog.NewOpID(time.Now(), "edit"),
				Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
					p.Credentials = liveTokens(len(p.Credentials) + 1)
					return p, nil
				},
			})
			return err
		},
		"an enrolment": func() error {
			return rig.enrol(iamdomain.Enrolment{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
				Stage: iam.StageActive, Name: "John Doe", Email: "john@example.com",
				Login: "john.doe", Credentials: liveTokens(iamdomain.MaxHeldCredentials + 1),
				OpID: statelog.NewOpID(time.Now(), "enrol"), Reason: "a hire",
			})
		},
	} {
		if err := write(); !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("%s past the cap answered %v, want ErrInvalid", name, err)
		}
	}
	if after, err := rig.end(t.Context()); err != nil || after != end {
		t.Errorf("a refused write moved the log from %d to %d (%v) — the cap is "+
			"decided before anything is published", end, after, err)
	}
}

// A CREDENTIAL THAT HAS STOPPED VERIFYING GIVES UP ITS PLACE, EARLIEST FIRST.
//
// A person at the cap was refused a mint, a second factor or new recovery
// codes and told to revoke a token nothing uses — which frees nothing, since a
// revocation keeps the credential on the document until the retention sweep
// collects it a week later. Now a write past the cap drops the credentials
// that lapsed earliest, as many as it needs and no live one: revoking a token
// makes room at once, an expired token makes room without anybody revoking
// it, and the one kept is the one that lapsed last, whose week of account is
// the longest still to run.
//
// Mutation: count lapsed credentials against the cap again and the mint is
// refused; drop the latest lapse first and the wrong token goes.
func TestALapsedCredentialGivesUpItsPlaceEarliestFirst(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	set := liveTokens(iamdomain.MaxHeldCredentials)
	// TWO LAPSED, the later one first in the set so its order breaks no tie:
	// a revocation an hour ago, and an expiry two hours ago.
	set[5].RevokedAt = brokerAt.Add(-time.Hour)
	set[9].ExpiresAt = brokerAt.Add(-2 * time.Hour)
	if err := rig.draining(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: owner, OpID: statelog.NewOpID(time.Now(), "fill"),
			Apply: func([]iamdomain.Credential) ([]iamdomain.Credential, error) {
				return set, nil
			},
		})
		return err
	}); err != nil {
		t.Fatalf("a set at the cap was refused: %v", err)
	}

	minted := uuid.Must(uuid.NewV7()).String()
	if _, _, err := mintFor(t, rig, asOwner(rig, owner), iamdomain.TokenMint{
		ID: minted, PersonID: owner, Label: "one more", Reason: "a PAT"}); err != nil {
		t.Fatalf("a mint on a person at the cap with two lapsed credentials "+
			"was refused: %v", err)
	}
	documents := rig.column(
		`SELECT CAST(document AS TEXT) FROM iam_people WHERE id = ?`, owner)
	if len(documents) != 1 {
		t.Fatalf("read the document: %v", documents)
	}
	doc, err := iamdomain.DecodePerson([]byte(documents[0]))
	if err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}
	held := map[string]bool{}
	for _, c := range doc.Credentials {
		held[c.ID] = true
	}
	switch {
	case len(doc.Credentials) != iamdomain.MaxHeldCredentials:
		t.Errorf("the person holds %d credentials, want the cap of %d",
			len(doc.Credentials), iamdomain.MaxHeldCredentials)
	case !held[minted]:
		t.Error("the new token is not on the document")
	case held[set[9].ID]:
		t.Errorf("the credential that lapsed earliest (%s, expired two hours "+
			"ago) is still held", set[9].ID)
	case !held[set[5].ID]:
		t.Errorf("the credential that lapsed last (%s, revoked an hour ago) "+
			"was dropped while an earlier one was the one to give way", set[5].ID)
	}
	for _, c := range set {
		if c.ID != set[9].ID && !held[c.ID] {
			t.Errorf("%s was dropped, and only one place was needed", c.ID)
		}
	}
}

// liveTokens is n machine tokens that verify at the writer's clock.
func liveTokens(n int) []iamdomain.Credential {
	out := make([]iamdomain.Credential, n)
	for i := range out {
		out[i] = iamdomain.Credential{V: iamdomain.DocumentVersion,
			ID: fmt.Sprintf("018f3a9c-0000-7000-8000-%012d", i), Method: iamdomain.MethodToken,
			Verifier:  credential.TokenVerifier("id", "secret"),
			ExpiresAt: brokerAt.Add(time.Hour)}
	}
	return out
}
