package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A NODE PUBLISHES ITS ADMIN POSTURE ONLY WHEN IT SERVES THE API.
//
// The posture is applied per node per request, so a fleet mid-rollout may
// disagree on it and only a node's own status row says which serves admin
// (`/query/fleet` reads it). A node serving no API grants nothing, so its
// posture is "not saying", not "refuses"; config accepts `api.auth.admin` on
// such a node, so only [New]'s wiring keeps a worker from advertising `serve`,
// which is why this goes THROUGH [New] rather than an engine literal.
// Mutations: drop the ServesAPI guard in [New] and the first half goes red;
// drop the posture from [Engine.nodeStatus] and the second does.
func TestANodeServingNoAPIPublishesNoAdminPosture(t *testing.T) {
	t.Parallel()

	quiet := testBootstrap(t)
	quiet.API.Auth.Admin = iam.AdminServe
	worker := baseEngine(t, quiet)
	if quiet.ServesAPI() {
		t.Fatal("the worker's bootstrap serves the API, so this half certifies nothing")
	}
	status := statusOf(t.Context(), t, worker)
	if status.AdminPosture != "" {
		t.Errorf("a node serving no API published the admin posture %q, want none",
			status.AdminPosture)
	}
	if posture, said := status.Meta()["admin"]; said {
		t.Errorf("a node serving no API published meta admin=%q, want the key absent",
			posture)
	}

	serving := testBootstrap(t)
	serving.Node.Roles = []string{"data", "seats", "workers", "ingress"}
	serving.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&serving.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	serving.API.Port = 8080
	serving.API.ExternalURL = "https://crewlet.example.com"
	serving.API.Auth.Admin = iam.AdminRefuse
	serving.API.Auth.Tokens = []config.APIToken{{
		ID: "ops", Token: "a-token-value-nobody-reads-here",
		Grants: []iam.Grant{iam.GrantWorklet},
	}}
	if !serving.ServesAPI() {
		t.Fatal("the serving bootstrap serves no API, so this half certifies nothing")
	}
	SeedStore(t, &serving)
	ingress, err := New(t.Context(), Options{Bootstrap: &serving})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { ingress.Stop(context.Background()) })
	status = statusOf(t.Context(), t, ingress)
	if status.AdminPosture != iam.AdminRefuse {
		t.Errorf("a node serving the API published the admin posture %q, want %q",
			status.AdminPosture, iam.AdminRefuse)
	}
	if posture := status.Meta()["admin"]; posture != string(iam.AdminRefuse) {
		t.Errorf("a node serving the API published meta admin=%q, want %q", posture,
			iam.AdminRefuse)
	}
}
