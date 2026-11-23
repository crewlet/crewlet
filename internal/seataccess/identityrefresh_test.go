package seataccess_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// refreshingVendor renews the kept key on every pass it is asked to: a new
// value sealed over the old, which it revokes only once the new one is sealed.
type refreshingVendor struct {
	*identityVendor
	sink *seataccesstest.Sink
	// next is the value the next refresh mints, "" for nothing due.
	next string
	// retired are the values revoked, and sealedAtRetire what the seal held
	// when each was.
	retired, sealedAtRetire []string
	retireErr               error
	refreshes               int
}

func (v *refreshingVendor) Open(context.Context, seataccess.OpenInput) (seataccess.Session, error) {
	return v, nil
}

func (v *refreshingVendor) RefreshIdentity(_ context.Context, in seataccess.IdentityInput) (seataccess.IdentityRefreshed, error) {
	v.refreshes++
	if v.next == "" {
		return seataccess.IdentityRefreshed{}, nil
	}
	old, fresh := in.Record.ID, v.next
	v.next = ""
	v.roles[fresh] = "Marketing"
	return seataccess.IdentityRefreshed{
		Minted: []seataccess.Minted{{Field: "KEY", Value: fresh}},
		Retire: func(context.Context) error {
			held, _ := v.sink.Held("APP_STORE_CONNECT_KEY_GROWTH")
			v.retired, v.sealedAtRetire = append(v.retired, old), append(v.sealedAtRetire, held)
			return v.retireErr
		},
	}, nil
}

// A KEPT CREDENTIAL IS RENEWED IN PLACE before the pass checks it: the new
// value sealed first, the old one revoked only after, never on a dry run, and
// a revoke that fails is said on the check rather than failing the pass.
func TestAKeptIdentityIsRefreshedBeforeItsCheck(t *testing.T) {
	t.Parallel()
	ledger, sink := seataccesstest.NewLedger(), seataccesstest.NewSink()
	v := &refreshingVendor{identityVendor: &identityVendor{roles: map[string]string{"good": "Marketing"}}, sink: sink}
	c := growthCompany("own", true)
	const sealed = "APP_STORE_CONNECT_KEY_GROWTH"
	keep := &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayHandedIn, Value: "good", FileName: "id-good"}
	if got := identitySync(t, v.identityVendor, ledger, sink, c, keep); !got.Kept() {
		t.Fatalf("not kept: %+v", got)
	}

	v.next = "renewed"
	syncWith(t, v, ledger, nil, c)
	if v.refreshes != 0 {
		t.Fatal("a dry run refreshed the identity")
	}
	got := syncWith(t, v, ledger, sink, c)
	if value, _ := sink.Held(sealed); value != "renewed" {
		t.Fatalf("sealed %q, want the renewed value", value)
	}
	if len(v.retired) != 1 || v.sealedAtRetire[0] != "renewed" {
		t.Fatalf("retired %v with %v sealed: the old value is revoked once the new one is sealed", v.retired,
			v.sealedAtRetire)
	}
	if got.Credential == nil || got.Credential.Source != seataccess.CredentialMinted {
		t.Fatalf("credential %+v, want the renewed value recorded as minted", got.Credential)
	}

	v.next, v.retireErr = "again", errors.New("the vendor is down")
	got = syncWith(t, v, ledger, sink, c)
	if value, _ := sink.Held(sealed); value != "again" || got.Check == nil {
		t.Fatalf("sealed %q, check %+v", value, got.Check)
	}
	if want := "could not revoke the credential it replaced"; !strings.Contains(got.Check.Said, want) {
		t.Fatalf("check said %q, want it to say %q", got.Check.Said, want)
	}
}

// syncWith is one pass of v over c, a dry run where sink is nil, taking
// ask where one is given.
func syncWith(t *testing.T, v seataccess.Vendor, ledger *seataccesstest.Ledger, sink *seataccesstest.Sink,
	c *config.Company, ask ...*seataccess.IdentityAsk) *seataccess.IdentityRecord {
	t.Helper()
	in := seataccess.SyncInput{Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger,
		Pass: "pass-1", By: "test", Now: func() time.Time { return at }}
	if sink != nil {
		in.Sink = sink
	}
	if len(ask) > 0 {
		in.Asks = map[string]seataccess.Ask{"growth": {Lane: identityLane, Identity: ask[0]}}
	}
	if _, err := seataccess.Sync(t.Context(), in); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rec, _ := ledger.Record(integration.KindAppStoreConnect, "growth")
	return rec.Lanes[identityLane].Identity
}

// retiringVendor revokes the identity a keep replaces itself, once the new one
// is sealed, failing where fail is set.
type retiringVendor struct {
	*identityVendor
	sink *seataccesstest.Sink
	// sealedAtRetire is what the seal held when each Retire ran.
	sealedAtRetire []string
	fail           error
}

func (v *retiringVendor) Open(context.Context, seataccess.OpenInput) (seataccess.Session, error) {
	return v, nil
}

func (v *retiringVendor) KeepIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	kept, err := v.identityVendor.KeepIdentity(ctx, in)
	kept.Retire = func(context.Context) error {
		held, _ := v.sink.Held("APP_STORE_CONNECT_KEY_GROWTH")
		v.sealedAtRetire = append(v.sealedAtRetire, held)
		return v.fail
	}
	return kept, err
}

// A REPLACEMENT THE VENDOR'S LANE REVOKES ITSELF asks nobody to revoke it,
// and revokes only once the new one is sealed; a revoke that fails asks the
// person after all.
func TestAReplacementTheLaneRevokesItselfOnceTheNewOneIsSealed(t *testing.T) {
	t.Parallel()
	ledger, sink := seataccesstest.NewLedger(), seataccesstest.NewSink()
	v := &retiringVendor{identityVendor: &identityVendor{roles: map[string]string{"good": "Marketing",
		"next": "Marketing", "last": "Marketing"}}, sink: sink}
	c := growthCompany("own", true)
	keep := func(value string) *seataccess.IdentityRecord {
		return syncWith(t, v, ledger, sink, c, &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayHandedIn,
			Value: value, FileName: "id-" + value})
	}
	if keep("good"); len(v.sealedAtRetire) != 0 {
		t.Fatal("a first keep revoked something; it replaced nothing")
	}
	got := keep("next")
	if len(got.Retiring) != 0 || got.Status() != seataccess.IdentityReady {
		t.Fatalf("a replacement the lane revoked still asks a person: %+v", got)
	}
	if len(v.sealedAtRetire) != 1 || v.sealedAtRetire[0] != "next" {
		t.Fatalf("revoked with %v sealed: the old one is revoked once the new one is", v.sealedAtRetire)
	}
	v.fail = errors.New("the vendor is down")
	if got = keep("last"); len(got.Retiring) != 1 || got.Retiring[0].ID != "id-next" {
		t.Fatalf("a revoke that failed asks nobody: %+v", got)
	}
}
