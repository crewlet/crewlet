package engine_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
)

// duplicateNamesRevision is a stored company that breaks both admission rules
// and none of the runnable ones: two units called "Platform" in different
// departments, each with a seat called "Engineer" on its own explicit handle.
// A build before those rules admitted it, and it runs.
var duplicateNamesRevision = json.RawMessage(`{"name":"Acme",
  "providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["${K}"]}}},
  "units":[
    {"name":"Engineering","children":[{"name":"Platform",
      "roles":[{"name":"Engineer","handle":"platform-engineer","llm":"zulu"}]}]},
    {"name":"Product","children":[{"name":"Platform",
      "roles":[{"name":"Engineer","handle":"product-engineer","llm":"zulu"}]}]}]}`)

// logBuffer is a slog destination a test can read while the reconciler writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every JSON record with this message.
func (b *logBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

// A REVISION AN OLDER PEER ACTIVATED IS APPLIED, AND WARNED ABOUT ONCE.
//
// The rolling-upgrade shape: an older node, which has no admission rules,
// activates a company with duplicate names. The payload reaches this node
// only through the coordination store, never through its own database. When
// applying validated every rule, the newer half of the fleet refused the
// company the older half was running, and every upgrade of a company carrying
// a duplicate was an outage. It runs, and each violation is logged once for
// the epoch rather than once per tick.
func TestARevisionAnOlderPeerActivatedIsAppliedWithAdmissionWarnings(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	p := newPlane(t, func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})

	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "from-an-older-peer", Summary: "written before the rule",
		Payload: duplicateNamesRevision, At: pinnedNow,
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("a revision breaking only admission rules was refused: %v", err)
	}
	if got := p.recon.Applied(); got != published.Epoch {
		t.Fatalf("applied epoch = %d, want %d", got, published.Epoch)
	}
	if row := p.fleetRow(t); row.Status != string(configplane.StatusOK) {
		t.Errorf("fleet status = %q (%s), want ok", row.Status, row.Error)
	}
	seats := p.seats(t)
	if !slices.Contains(seats, "platform-engineer") || !slices.Contains(seats, "product-engineer") {
		t.Errorf("seats = %v, want both engineers running", seats)
	}

	warnings := logs.records(t, "org_admission_warning")
	if len(warnings) != 2 {
		t.Fatalf("%d admission warnings, want one per violation (seat name and unit name): %v",
			len(warnings), warnings)
	}
	var details []string
	for _, w := range warnings {
		if w["revision"] != "from-an-older-peer" {
			t.Errorf("a warning does not name the revision: %v", w)
		}
		detail, _ := w["detail"].(string)
		details = append(details, detail)
	}
	joined := strings.Join(details, "\n")
	for _, want := range []string{"duplicate seat name", "duplicate unit name"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning reports %q: %s", want, joined)
		}
	}

	// ONCE PER APPLIED EPOCH. A tick on an epoch this node already applied
	// applies nothing, so it must not repeat the warning either.
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := len(logs.records(t, "org_admission_warning")); got != 2 {
		t.Errorf("%d admission warnings after a second tick on the same epoch, want still 2", got)
	}
}

// A NODE BOOTS ON A STORED COMPANY THAT BREAKS AN ADMISSION RULE.
//
// Building an epoch is the other half of applying one. A store holding such a
// revision is the ordinary state of a company written before the rule, and a
// node that refused to build it would not start at all after an upgrade.
func TestAnEngineBootsOnAStoredCompanyThatBreaksAnAdmissionRule(t *testing.T) {
	t.Parallel()
	company, err := config.DecodeCompany(duplicateNamesRevision)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if config.Problems(company.ValidateAdmission()) == nil {
		t.Fatal("the fixture breaks no admission rule, so this proves nothing")
	}
	e := newEngine(t, engine.Options{Company: company})
	if got := len(e.Company().Seats()); got != 2 {
		t.Errorf("seats = %d, want both engineers", got)
	}
}
