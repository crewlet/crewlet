package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// syncingNow is what an access answer reads for kind's Syncing.
func syncingNow(a *SeatAccessAPI, kind integration.Kind) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running[kind].n > 0
}

// EVERY ACCESS SYNC ENDS IN ITS FOLLOW-UP, AND READS AS SYNCING UNTIL IT DOES.
//
// The follow-up is the check route's pass over the whole integration. Whether
// a person's action (answered once the seat's sync is done) or a save's sync
// (run after the apply), the tab reads as syncing while it runs, so it draws
// what the follow-up records. With no secret store here the sync fails with
// its own record, which still counts as a sync that ran.
//
// Mutation: drop the follow-up from either path, or release the mark before it
// runs, and a case here fails.
func TestAnAccessSyncEndsInItsFollowUpWhileItStillReadsAsSyncing(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
seat_access_migrated: [datadog]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  datadog:
    enabled: true
    route_to: sre
    webhook_token: "${DATADOG_WEBHOOK_TOKEN}"
    provisioning: {site: datadoghq.com, api_key: "${DATADOG_API_KEY}", app_key: "${DATADOG_APP_KEY}"}
roles:
  - name: SRE
    handle: sre
    llm: zulu
    integrations:
      datadog: {enabled: true, role: role-std}
`)
	for name, afterSave := range map[string]bool{"an action": false, "a save": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{}
			e.epoch.current.Store(company)
			fleet := func(context.Context) (*config.Company, string, error) {
				return company.Config, "rev-1", nil
			}
			api := e.SeatAccessAPI(fleet, nil, func() bool { return false })
			kind := integration.KindDatadog
			started := make(chan bool, 1)
			release, done := make(chan struct{}), make(chan struct{})
			err := api.Sync(t.Context(), setupapi.AccessSync{
				Kind: kind, Revision: "rev-1", Handles: []string{"sre"}, AfterSave: afterSave,
				Then: func(context.Context) {
					started <- syncingNow(api, kind)
					<-release
					close(done)
				},
			})
			if err != nil {
				t.Fatalf("Sync = %v", err)
			}
			select {
			case syncing := <-started:
				if !syncing {
					t.Fatal("the follow-up ran while the integration read as idle")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the sync never ran its follow-up")
			}
			if !syncingNow(api, kind) {
				t.Error("the integration stopped reading as syncing before its follow-up finished")
			}
			close(release)
			<-done
			deadline := time.Now().Add(5 * time.Second)
			for syncingNow(api, kind) {
				if time.Now().After(deadline) {
					t.Fatal("the integration still reads as syncing after its follow-up finished")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
