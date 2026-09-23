package authapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE CALLBACK RESOLVES A PROVIDER'S SUBJECT THROUGH THE LINK TABLE.
//
// It used to hand the subject's blind to the ADDRESS lookup. The blinder puts
// the two in different classes inside one MAC by construction, so that lookup
// matched nobody and every provider sign-in was refused as unlinked, while
// each piece passed its own suite. This holds the seam the callback asks
// through: the subject's blind goes to the subject lookup, and an ambiguous
// subject refuses rather than resolving either holder.
func TestTheCallbackResolvesASubjectThroughItsLink(t *testing.T) {
	t.Parallel()
	blinder, err := iamdomain.NewBlinder([]byte(strings.Repeat("k",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	claims := oidc.Claims{Issuer: "https://idp.example.com", Subject: "ada"}
	blind, err := blinder.Subject(claims.Issuer, claims.Subject)
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	ada := iamdomain.Sighting{ID: "018f3a9c-0000-7000-8000-000000000001",
		Login: "ada.linked"}

	service := func(dir *subjectDirectory) *Service {
		return &Service{blinder: blinder, directory: dir, now: time.Now}
	}

	dir := &subjectDirectory{blind: blind, held: ada}
	held, err := service(dir).personForSubject(signInRequest(), claims)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if held.ID != ada.ID {
		t.Errorf("the subject resolved to %q, want %q — the callback did not "+
			"ask the link table, so this person can never sign in through "+
			"their provider", held.ID, ada.ID)
	}

	dir = &subjectDirectory{blind: blind, err: iamdomain.ErrSubjectAmbiguous}
	held, err = service(dir).personForSubject(signInRequest(), claims)
	if !errors.Is(err, iamdomain.ErrSubjectAmbiguous) {
		t.Errorf("an ambiguous subject answered %v, want the refusal", err)
	}
	if held.ID != "" {
		t.Errorf("an ambiguous subject signed in as %q", held.ID)
	}
}

// subjectDirectory answers the subject lookup for one blind. The ADDRESS
// lookup answers a different person for any blind, so a callback that asked
// the wrong question is caught returning the wrong person rather than merely
// nobody.
type subjectDirectory struct {
	Directory // every other verb panics: the callback must not reach it
	blind     string
	held      iamdomain.Sighting
	err       error
}

func (d *subjectDirectory) PersonBySubjectBlind(_ context.Context, blind string,
	_ time.Time) (iamdomain.Sighting, error) {

	if d.err != nil {
		return iamdomain.Sighting{}, d.err
	}
	if blind != d.blind {
		return iamdomain.Sighting{}, nil
	}
	return d.held, nil
}

func (d *subjectDirectory) PersonByEmailBlind(context.Context, string) (
	iamdomain.Sighting, error) {

	return iamdomain.Sighting{ID: "the-wrong-person"}, nil
}
