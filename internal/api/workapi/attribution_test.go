package workapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/tracker"
)

// tokenRow is the one row the guard's machine-token arm reads.
type tokenRow struct{ row credential.TokenRow }

func (d tokenRow) MachineToken(context.Context, string) (credential.TokenRow, error) {
	return d.row, nil
}

// oneSeat is a chart holding one human seat, applied past every binding.
type oneSeat string

func (c oneSeat) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	if ref != string(c) {
		return session.Seat{}, false, nil
	}
	return session.Seat{Handle: string(c), Kind: "human", Unit: "exec"}, true, nil
}

func (oneSeat) Position(context.Context) (uint64, time.Duration, error) {
	return 1 << 40, 0, nil
}

// A WRITE MADE THROUGH SOMEBODY'S MACHINE TOKEN SAYS SO.
//
// Through the REAL guard's machine-token arm, so the credential on the record
// is what the guard composed rather than a principal this case built. The token
// acts as its owner — the CTO, bound to their seat — so the author is the seat,
// kind human; and the operator is `pat:<id>`, on a work item filed through the
// tool and on a page renamed through the writer directly, the surface's two
// paths to a record. Before, both carried the owner's login: whoever held a
// token minted on the CTO's account filed and edited the CTO's work with no row
// saying a token was used. Mutation: drop Via from the arm, or record the login
// in ActorOf, and the operator is `cto.person`.
func TestAWriteThroughAMachineTokenIsRecordedAsTheToken(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	presented := credential.Token{ID: uuid.Must(uuid.NewV7()).String(),
		Position: 3, Secret: secret}
	carried := []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite,
		iam.GrantKnowledgeWrite}
	arm, err := auth.NewTokens(auth.TokensDeps{
		Directory: tokenRow{credential.TokenRow{
			Applied: 10, Found: true, IsToken: true,
			Verifier:  credential.TokenVerifier(presented.ID, secret),
			ExpiresAt: time.Now().Add(time.Hour),
			Grants:    carried, Colleague: iam.ColleagueWrite,
			Owner: credential.TokenOwner{
				Found: true, ID: uuid.Must(uuid.NewV7()).String(),
				Kind: iam.KindPerson, Stage: iam.StageActive,
				Login: "cto.person", Grants: carried,
				Colleague: iam.ColleagueWrite, Seat: "cto", SeatAt: 5,
			},
		}},
		Chart: oneSeat("cto"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var b config.Bootstrap
	b.API.Auth.MaxGrants = iam.AllGrants
	guarded := auth.New(&b).WithTokens(arm).Middleware(r.mux)
	send := func(method, target, body string) int {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+presented.Value())
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send(http.MethodPost, "/work/items",
		`{"title":"rotate the key","project":"ENG"}`); code/100 != 2 {
		t.Fatalf("filing an item through the token answered %d", code)
	}
	if code := send(http.MethodPost, "/pages/p-1/rename",
		`{"title":"Runbook v2"}`); code/100 != 2 {
		t.Fatalf("renaming a page through the token answered %d", code)
	}

	via := iam.MachineTokenName(presented.ID)
	if len(r.writes.actors) == 0 {
		t.Fatal("no work record was written")
	}
	for _, a := range r.writes.actors {
		if a.Handle != "cto" || a.Kind != tracker.AuthorHuman || a.OperatorID != via {
			t.Errorf("a work record is written as %q (%s) through %q, want the "+
				"owner's seat as a human, through %q", a.Handle, a.Kind,
				a.OperatorID, via)
		}
	}
	if len(r.kb.actors) == 0 {
		t.Fatal("no page record was written")
	}
	for _, a := range r.kb.actors {
		if a.Handle != "cto" || a.OperatorID != via {
			t.Errorf("a page record is written as %q through %q, want %q "+
				"through %q", a.Handle, a.OperatorID, "cto", via)
		}
	}
}
