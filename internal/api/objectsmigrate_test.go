package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/engine"
)

// fakeMigrator records what the route asked of the engine.
type fakeMigrator struct {
	asked  []engine.ObjectMigration
	err    error
	report engine.ObjectMigrationReport
}

func (f *fakeMigrator) MigrateObjects(_ context.Context, req engine.ObjectMigration) (engine.ObjectMigrationReport, error) {
	f.asked = append(f.asked, req)
	return f.report, f.err
}

// A MIGRATION MOVES EVERY FILE THE COMPANY HOLDS, so a caller with no
// credential is refused before the engine is asked, and one naming no target
// is told what to name.
func TestAMigrationNeedsACredentialAndATarget(t *testing.T) {
	t.Parallel()
	m := &fakeMigrator{}
	a := newApp(t, api.Options{Bootstrap: guarded(), Objects: m})
	if status, _ := post(t, a, "/objects/migrate?target=/etc/crewlet/s3.yaml", ""); status == http.StatusOK {
		t.Fatal("an unauthenticated caller ran a migration")
	}
	status, body := post(t, a, "/objects/migrate", "t0ken")
	if status != http.StatusBadRequest || body["error"] != "objects_migration_refused" {
		t.Fatalf("a migration naming no target answered %d %v", status, body["error"])
	}
	if len(m.asked) != 0 {
		t.Fatalf("refused requests reached the engine: %+v", m.asked)
	}
}

// THE ROUTE PASSES THE TARGET AND THE FINAL FLAG THROUGH, and answers the
// engine's report.
func TestAMigrationAnswersTheEnginesReport(t *testing.T) {
	t.Parallel()
	m := &fakeMigrator{report: engine.ObjectMigrationReport{From: "nats", To: "s3:/files/acme/", Moved: true}}
	a := newApp(t, api.Options{Bootstrap: guarded(), Objects: m})
	status, body := post(t, a, "/objects/migrate?target=/etc/crewlet/s3.yaml&final=true", "t0ken")
	if status != http.StatusOK || body["moved"] != true || body["to"] != "s3:/files/acme/" {
		t.Fatalf("the final pass answered %d %v", status, body)
	}
	if len(m.asked) != 1 || m.asked[0].Target != "/etc/crewlet/s3.yaml" || !m.asked[0].Final {
		t.Fatalf("the engine was asked %+v", m.asked)
	}
}

// A REFUSAL IS THE CALLER'S TO ACT ON and carries its reason; a store that
// stopped answering is a 503 whose reason stays in the log, beside what was
// copied before it stopped.
func TestAMigrationRefusalAndFailureAreToldApart(t *testing.T) {
	t.Parallel()
	m := &fakeMigrator{err: fmt.Errorf("%w: restart every node in maintenance mode", engine.ErrMigrationRefused)}
	a := newApp(t, api.Options{Bootstrap: guarded(), Objects: m})
	status, body := post(t, a, "/objects/migrate?target=/etc/crewlet/s3.yaml&final=true", "t0ken")
	detail, _ := body["detail"].(string)
	if status != http.StatusConflict || !strings.Contains(detail, "maintenance mode") {
		t.Fatalf("a refusal answered %d %v", status, body)
	}
	m.err = errors.New("ask the target for files/0190: dial tcp 10.0.0.9:443: connection refused")
	status, body = post(t, a, "/objects/migrate?target=/etc/crewlet/s3.yaml", "t0ken")
	if status != http.StatusServiceUnavailable || body["error"] != "objects_migration_failed" {
		t.Fatalf("a failed pass answered %d %v", status, body)
	}
	if body["detail"] != nil || body["report"] == nil {
		t.Fatalf("a failed pass leaked its reason or dropped its report: %v", body)
	}
}
