package secretsapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// countedFleet counts the store's reads: each whole pass, and each read or
// removal of one key.
type countedFleet struct {
	coord.Fleet
	passes, keys atomic.Int64
}

func (f *countedFleet) SecretValues(ctx context.Context) ([]coord.SecretRecord, error) {
	f.passes.Add(1)
	return f.Fleet.SecretValues(ctx)
}

func (f *countedFleet) Secret(ctx context.Context, name string) (coord.SecretRecord, bool, error) {
	f.keys.Add(1)
	return f.Fleet.Secret(ctx, name)
}

func (f *countedFleet) DeleteSecret(ctx context.Context, name string) (bool, error) {
	f.keys.Add(1)
	return f.Fleet.DeleteSecret(ctx, name)
}

// countedSurface is the routes over a counted memory fleet.
func countedSurface(t *testing.T) (http.Handler, *countedFleet) {
	t.Helper()
	fleet := &countedFleet{Fleet: coordmem.NewFleet()}
	return mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...), fleet
}

// A NAME PAST ITS BOUND IS REFUSED AS AN INVALID NAME, never as a fault: a
// credential's record is stored under its name and keeps it, so a name has a
// widest form (secrets.MaxNameBytes). Each write is answered 400 saying the
// bound, nothing sealed; a read or a delete of one answers absent without
// asking the store, since no row can be stored under it. A name at the bound
// is written.
//
// Mutation: drop the length from secrets.CheckName, and each write is sealed;
// drop fleetsecrets' namesNoRow, and the read and the delete ask the store.
func TestANamePastItsBoundIsRefusedAsAnInvalidName(t *testing.T) {
	t.Parallel()
	h, fleet := countedSurface(t)
	long := strings.Repeat("A", secrets.MaxNameBytes+1)
	bound := strconv.Itoa(secrets.MaxNameBytes)
	for _, c := range []struct {
		what, method, path, body, code string
	}{
		{"a put", http.MethodPut, "/secrets/" + long, "value", "invalid_name"},
		{"a batch", http.MethodPut, "/secrets", `{"values": {"` + long + `": "value"}}`, "invalid_body"},
		{"a reveal", http.MethodPost, "/secrets/reveal", revealBody(t, long), "invalid_name"},
	} {
		code, body := call(t, h, c.method, c.path, c.body)
		if code != http.StatusBadRequest || !strings.Contains(body, c.code) || !strings.Contains(body, bound) {
			t.Errorf("%s naming a %d-byte name = %d %s, want 400 %s saying the bound, %s", c.what,
				len(long), code, body, c.code, bound)
		}
	}
	if rows, err := fleet.Fleet.SecretValues(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("the refused writes left %d rows (%v), want none", len(rows), err)
	}
	fleet.keys.Store(0)
	if code, body := call(t, h, http.MethodGet, "/secrets/"+long, ""); code != http.StatusNotFound {
		t.Errorf("GET of a long name = %d %s, want 404", code, body)
	}
	if code, body := call(t, h, http.MethodDelete, "/secrets/"+long, ""); code != http.StatusOK ||
		!strings.Contains(body, `"removed":false`) {
		t.Errorf("DELETE of a long name = %d %s, want 200 with removed:false", code, body)
	}
	if n := fleet.keys.Load(); n != 0 {
		t.Errorf("reading a name no row can have asked the store %d times, want none", n)
	}
	if code, body := call(t, h, http.MethodPut, "/secrets/"+long[1:], "value"); code != http.StatusOK {
		t.Errorf("a put of a name at its bound = %d %s, want it written", code, body)
	}
}
