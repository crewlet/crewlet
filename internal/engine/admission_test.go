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

// duplicateStepsRevision is a stored SETTINGS company that breaks an
// admission rule and none of the runnable ones: two sandbox setup steps under
// one name. A build before that rule admitted it, and it runs.
//
// A SETTINGS RULE, because a revision carries settings. The fixture used to
// be two units called "Platform", each with a seat called "Engineer" — which
// is the same class of violation on the half a revision no longer holds, and
// is refused as a chart before any rule is reached.
//
// The rule is a real one and its consequence is a write: a step's `env` and
// `files` are credentials restored by the step's NAME, so two steps of one
// name leave every later write carrying that list refused on masks nobody
// edited.
var duplicateStepsRevision = json.RawMessage(`{"name":"Acme",
  "providers":{
    "llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["${K}"]}},
    "sandbox":{"default_run_in":"direct","local":{},"setup":[
      {"name":"git-auth","commands":["true"]},
      {"name":"git-auth","commands":["false"]}]}}}`)

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
		Payload: duplicateStepsRevision, At: pinnedNow,
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
	// THE REVISION IS SERVED, which is the point: an admission violation is
	// a warning on a stored revision, never a refusal. Asserted on the
	// epoch this node published rather than on its seats — the chart is a
	// log of its own now, and a config apply carries no chart to count.
	if p.engine.Company() == nil {
		t.Fatal("the apply reported success and published no epoch")
	}

	// ONE, for the one violation this revision holds.
	warnings := logs.records(t, "org_admission_warning")
	if len(warnings) != 1 {
		t.Fatalf("%d admission warnings, want one per violation: %v",
			len(warnings), warnings)
	}
	// AND IT NAMES THE PLACE, in the path the config package located it at,
	// so a person can change the field rather than search for it.
	placed := map[string][]string{}
	for _, w := range warnings {
		if w["revision"] != "from-an-older-peer" {
			t.Errorf("a warning does not name the revision: %v", w)
		}
		detail, _ := w["detail"].(string)
		raw, _ := w["paths"].([]any)
		var paths []string
		for _, path := range raw {
			text, _ := path.(string)
			paths = append(paths, text)
		}
		if strings.Contains(detail, "duplicate setup step name") {
			placed["duplicate setup step name"] = paths
		}
	}
	if want := []string{"providers.sandbox.setup"}; !slices.Equal(
		placed["duplicate setup step name"], want) {
		t.Errorf("the warning names paths %v, want %v",
			placed["duplicate setup step name"], want)
	}

	// ONCE PER APPLIED EPOCH. A tick on an epoch this node already applied
	// applies nothing, so it must not repeat the warning either.
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := len(logs.records(t, "org_admission_warning")); got != 1 {
		t.Errorf("%d admission warnings after a second tick on the same epoch, want still 1", got)
	}
}

// A NODE BOOTS ON A STORED COMPANY THAT BREAKS AN ADMISSION RULE.
//
// Building an epoch is the other half of applying one. A store holding such a
// revision is the ordinary state of a company written before the rule, and a
// node that refused to build it would not start at all after an upgrade.
func TestAnEngineBootsOnAStoredCompanyThatBreaksAnAdmissionRule(t *testing.T) {
	t.Parallel()
	company, err := config.DecodeCompany(duplicateStepsRevision)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if config.Problems(company.ValidateAdmission()) == nil {
		t.Fatal("the fixture breaks no admission rule, so this proves nothing")
	}
	e := newEngine(t, engine.Options{Company: company})
	if e.Company() == nil {
		t.Error("a node refused to build an epoch from a company that breaks " +
			"an admission rule, so it would not start at all after an upgrade")
	}
}
