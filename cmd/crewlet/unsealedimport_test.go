package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// AN IMPORT THROUGH A RUNNING NODE REPLACES AN UNSEALED REVISION, as the offline
// import does (TestAPlaintextRevisionIsRefusedNamingTheImport).
//
// `crewlet config import` while a node is up is that node's PUT
// /config?import=true, and every refusal of an unsealed revision names it; the
// node refused it in the very words that had sent somebody there, because
// every write on its surface opened the fleet's revision first. The file is
// byte for byte the planted document, so an import that compared against it
// rather than replacing it would write nothing.
//
// Mutation: drop configapi's unsealed arm and the import is refused 409
// unsealed_revision.
func TestAnImportThroughARunningNodeReplacesAnUnsealedRevision(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	plane := coordmemory.NewFleet()
	svc, err := configapi.New(configapi.Options{
		Store: db, Plane: plane, Cipher: fixtureCipher, Node: "core-1",
		Holders: func(context.Context, func(string) bool, time.Time) (
			map[string]configapi.SeatHolder, error) {

			return map[string]configapi.SeatHolder{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	// THE OPERATOR THE GUARD WOULD RESOLVE: a Tier A token, fresh for a step-up.
	operator := iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")), Login: iam.TokenLogin("ops"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), operator)))
	}))
	t.Cleanup(server.Close)

	// THE UNSEALED REVISION, this node's own and the one the fleet runs.
	document, err := json.Marshal(parse(t, companyYAML))
	if err != nil {
		t.Fatal(err)
	}
	plain := storeAndActivate(t, db, plane, store.Revision{
		Source: "file", CreatedBy: testNode, CreatedByKind: iam.ActorSystem,
		Summary: "written without a seal", Payload: document,
		CreatedAt: time.Now().UTC(),
	}).ID

	client := &configClient{base: server.URL, http: httpx.Client(apiTimeout),
		answerCap: maxConfigResponseBytes}
	id, _, err := client.Import(t.Context(), []byte(companyYAML), "restore from my own copy")
	if err != nil {
		t.Fatalf("the import every refusal of an unsealed revision names was refused "+
			"by the running node: %v", err)
	}
	fleets := fleetsRevision(t, db, plane)
	if fleets.ID != id || !secrets.Sealed(fleets.Payload) || fleets.ParentID != plain ||
		fleets.Source != "file" {
		t.Errorf("the fleet's revision is %s (sealed %v, parent %q, source %q), want "+
			"the import %s, sealed, from a file, built on %s", fleets.ID,
			secrets.Sealed(fleets.Payload), fleets.ParentID, fleets.Source, id, plain)
	}
}
