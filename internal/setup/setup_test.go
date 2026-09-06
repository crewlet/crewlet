package setup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

var pinned = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// --- names ----------------------------------------------------------------- //

// A DERIVED NAME MUST BE ONE A ${VAR} CAN REFERENCE, or the secret is stored
// somewhere the config it points at cannot reach. The grammar is envref's
// own, so a handle with a hyphen in it is the ordinary case rather than an
// exotic one.
func TestADerivedSecretNameIsAlwaysReferenceable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind integration.Kind
		req  setup.Requirement
		want string
	}{
		"the plain case": {
			integration.KindDatadog,
			setup.Requirement{Field: "webhook_token"},
			"DATADOG_WEBHOOK_TOKEN",
		},
		"a per-seat requirement carries its handle": {
			integration.KindSlack,
			setup.Requirement{Field: "bot_token", Seat: "sre-lead"},
			"SLACK_BOT_TOKEN_SRE_LEAD",
		},
		"a handle the grammar refuses becomes one it accepts": {
			integration.KindSlack,
			setup.Requirement{Field: "bot_token", Seat: "nova.1-eu"},
			"SLACK_BOT_TOKEN_NOVA_1_EU",
		},
		"the vendor's own declaration wins": {
			integration.KindDatadog,
			setup.Requirement{Field: "webhook_token", SecretName: "DD_TOKEN"},
			"DD_TOKEN",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := setup.SecretNameFor(tc.kind, tc.req)
			if got != tc.want {
				t.Fatalf("SecretNameFor = %q, want %q", got, tc.want)
			}
			if !setup.ValidSecretName(got) {
				t.Fatalf("%q is not a name a ${VAR} can reference", got)
			}
		})
	}
}

// EVERY VENDOR KIND DERIVES A NAME THE GRAMMAR ACCEPTS, which is what lets
// the slug skip the leading-digit rule: a derived name always begins with the
// kind, and a kind that did not start with a letter would break that
// silently, on one vendor, for the seats whose handles start with a digit.
func TestEveryVendorKindDerivesAReferenceableName(t *testing.T) {
	t.Parallel()
	for _, kind := range integration.Kinds {
		for _, seat := range []string{"", "1st-line", "nova.1"} {
			got := setup.SecretNameFor(kind,
				setup.Requirement{Field: "bot_token", Seat: seat})
			if !setup.ValidSecretName(got) {
				t.Errorf("%s/%q derives %q, which no ${VAR} can reference", kind, seat, got)
			}
		}
	}
}

// And two seats whose handles differ only by a separator stay two names. A
// slug that dropped the separator would seal one seat's credential under the
// other's name.
func TestTwoSeatsNeverCollideOnOneName(t *testing.T) {
	t.Parallel()
	a := setup.SecretNameFor(integration.KindSlack,
		setup.Requirement{Field: "bot_token", Seat: "nova-1"})
	b := setup.SecretNameFor(integration.KindSlack,
		setup.Requirement{Field: "bot_token", Seat: "nova1"})
	if a == b {
		t.Fatalf("nova-1 and nova1 both derive %q", a)
	}
}

// --- pointers -------------------------------------------------------------- //

// THREE OUTCOMES, and they are three different situations. An empty slot gets
// a new pointer; a slot already naming a variable is written THROUGH, which
// is what makes a rotation a change to the store rather than to the company;
// a slot holding a literal is refused, because overwriting it would edit the
// company from a setup form and destroy a credential somebody put there.
func TestAPointerIsMintedReusedOrRefused(t *testing.T) {
	t.Parallel()
	req := setup.Requirement{
		Field: "webhook_token", ConfigPath: "integrations.datadog.webhook_token",
		SecretName: "DATADOG_WEBHOOK_TOKEN", Kind: setup.KindSecret,
	}

	name, writes, err := setup.PointerFor(integration.KindDatadog, req, "")
	if err != nil || name != "DATADOG_WEBHOOK_TOKEN" || !writes {
		t.Fatalf("empty slot = (%q, %v, %v), want a new pointer", name, writes, err)
	}

	name, writes, err = setup.PointerFor(integration.KindDatadog, req, "  ${DD_EXISTING}  ")
	if err != nil || name != "DD_EXISTING" || writes {
		t.Fatalf("existing pointer = (%q, %v, %v), want a write through it", name, writes, err)
	}

	var literal *setup.ErrLiteralInConfig
	if _, _, err = setup.PointerFor(integration.KindDatadog, req, "dd-plaintext"); !errors.As(err, &literal) {
		t.Fatalf("a literal was not refused: %v", err)
	} else if literal.Path != req.ConfigPath {
		t.Errorf("the refusal names %q, not the path", literal.Path)
	}

	// A COMPOSITE is refused too, and it is the case a "does it contain
	// ${" check would wave through: the variable holds a fragment, so
	// writing a credential into it would replace a hostname.
	if _, _, err = setup.PointerFor(integration.KindDatadog, req, "https://${HOST}/x"); !errors.As(err, &literal) {
		t.Fatalf("a composite was not refused: %v", err)
	}
}

// --- resolution ------------------------------------------------------------ //

// PRESENT AND RESOLVED ARE TWO FACTS, and the gap between them is the silent
// outage: the config shows a secret, the vendor shows a healthy hook, and
// every delivery is refused with nothing naming the variable.
func TestResolutionSeparatesWrittenDownFromUsable(t *testing.T) {
	t.Parallel()
	store := map[string]string{"HAVE": "value", "EMPTY": ""}
	resolve := func(name string) (string, bool) { v, ok := store[name]; return v, ok }

	for name, tc := range map[string]struct {
		stored          string
		resolve         func(string) (string, bool)
		wantPresent     bool
		wantResolved    *bool
		wantNilResolved bool
	}{
		"nothing written down":   {"", resolve, false, nil, true},
		"a reference that holds": {"${HAVE}", resolve, true, boolPtr(true), false},
		"a reference that does not": {
			"${MISSING}", resolve, true, boolPtr(false), false,
		},
		"a reference resolving to nothing": {
			"${EMPTY}", resolve, true, boolPtr(false), false,
		},
		// A literal in the document IS the value: present and resolved,
		// whatever the store holds.
		"a literal": {"plain", resolve, true, boolPtr(true), false},
		// AND A PROCESS THAT CANNOT SAY SAYS SO. Reporting false here
		// would tell an operator a working credential is broken.
		"no resolver at all": {"${HAVE}", nil, true, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			present, resolved := setup.Resolution(tc.stored, tc.resolve)
			if present != tc.wantPresent {
				t.Errorf("present = %v, want %v", present, tc.wantPresent)
			}
			switch {
			case tc.wantNilResolved:
				if resolved != nil {
					t.Errorf("resolved = %v, want null", *resolved)
				}
			case resolved == nil:
				t.Errorf("resolved = null, want %v", *tc.wantResolved)
			case *resolved != *tc.wantResolved:
				t.Errorf("resolved = %v, want %v", *resolved, *tc.wantResolved)
			}
		})
	}
}

// A requirement written down but unresolved is NOT satisfied. That is the
// whole point of asking two questions.
func TestSatisfiedNeedsBothHalves(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  setup.Requirement
		want bool
	}{
		{"absent", setup.Requirement{}, false},
		{"present and resolved", setup.Requirement{Present: true, Resolved: boolPtr(true)}, true},
		{"present and unresolved", setup.Requirement{Present: true, Resolved: boolPtr(false)}, false},
		// Where resolution is unknown, present is the most that can be
		// claimed and it is claimed: a standalone API must not show a
		// permanent list of things to fix that are already fine.
		{"present, cannot say", setup.Requirement{Present: true}, true},
	}
	for _, tc := range cases {
		if got := tc.req.Satisfied(); got != tc.want {
			t.Errorf("%s: Satisfied() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

// --- the ordered write ------------------------------------------------------ //

// recorder stands in for the two stores, and records the ORDER, which is what
// this whole file is about.
type recorder struct {
	events  []string
	secrets map[string]string
	failSet error
	failApp error
	active  string
	seat    []byte
}

func (r *recorder) Set(_ context.Context, name, value, by, source string, _ time.Time) error {
	if r.failSet != nil {
		return r.failSet
	}
	if r.secrets == nil {
		r.secrets = map[string]string{}
	}
	r.secrets[name] = value
	r.events = append(r.events, "secret:"+name+" by="+by+" source="+source)
	return nil
}

func (r *recorder) Apply(_ context.Context, patch []byte, summary, _, expect string) (string, int64, error) {
	if r.failApp != nil {
		return "", 0, r.failApp
	}
	r.events = append(r.events, "patch:"+string(patch)+" summary="+summary+" expect="+expect)
	return "rev-1", 7, nil
}

func (r *recorder) Seat(_ context.Context, handle string) ([]byte, error) {
	if r.seat == nil {
		return []byte(`{"name":"` + handle + `","handle":"` + handle + `"}`), nil
	}
	return r.seat, nil
}

func (r *recorder) SetSeat(
	_ context.Context, handle string, body []byte, summary, _, expect string,
) (string, int64, error) {
	if r.failApp != nil {
		return "", 0, r.failApp
	}
	r.events = append(r.events,
		"seat:"+handle+" body="+string(body)+" summary="+summary+" expect="+expect)
	return "rev-3", 9, nil
}

func (r *recorder) Current(context.Context) (string, error) {
	if r.active == "" {
		return "rev-0", nil
	}
	return r.active, nil
}

func (r *recorder) Reload(_ context.Context, summary, _ string) (string, int64, error) {
	if r.failApp != nil {
		return "", 0, r.failApp
	}
	r.events = append(r.events, "reload:"+summary)
	return "rev-2", 8, nil
}

// withStored copies a requirement list with one field's stored value set,
// which is how a test says "the document already holds this here".
func withStored(reqs []setup.Requirement, field, stored string) []setup.Requirement {
	out := make([]setup.Requirement, len(reqs))
	copy(out, reqs)
	for i := range out {
		if out[i].Field == field {
			out[i].Stored = stored
		}
	}
	return out
}

func writer(rec *recorder) setup.Writer {
	return setup.Writer{Secrets: rec, Config: rec, Now: func() time.Time { return pinned }}
}

var datadogReqs = []setup.Requirement{
	{
		Field: "webhook_token", Kind: setup.KindSecret, Required: true,
		ConfigPath: "integrations.datadog.webhook_token", SecretName: "DATADOG_WEBHOOK_TOKEN",
	},
	{
		Field: "route_to", Kind: setup.KindHandle, Required: true,
		ConfigPath: "integrations.datadog.route_to",
	},
}

// THE SECRET IS DURABLE BEFORE THE POINTER LANDS. The other order leaves the
// config naming a secret with nothing behind it, and the route it guards
// answering 503 to every delivery for the length of the window, which looks
// exactly like an attack.
func TestTheSecretIsWrittenBeforeTheConfigPointsAtIt(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	result, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:    integration.KindDatadog,
		Values:  map[string]string{"webhook_token": "s3cr3t-value", "route_to": "sre-lead"},
		Summary: "connect datadog", Operator: "founder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 2 {
		t.Fatalf("events = %v", rec.events)
	}
	if !strings.HasPrefix(rec.events[0], "secret:DATADOG_WEBHOOK_TOKEN") {
		t.Fatalf("the first write was %q, want the secret", rec.events[0])
	}
	if !strings.HasPrefix(rec.events[1], "patch:") {
		t.Fatalf("the second write was %q, want the config", rec.events[1])
	}
	// The pointer, never the value.
	if strings.Contains(rec.events[1], "s3cr3t-value") {
		t.Fatalf("the patch carries the credential: %s", rec.events[1])
	}
	if !strings.Contains(rec.events[1], `"webhook_token":"${DATADOG_WEBHOOK_TOKEN}"`) {
		t.Fatalf("the patch does not point at the secret: %s", rec.events[1])
	}
	// Both fields of one vendor merge into one object rather than the
	// second replacing the first.
	if !strings.Contains(rec.events[1], `"route_to":"sre-lead"`) {
		t.Fatalf("the patch lost the non-secret field: %s", rec.events[1])
	}
	if len(result.Secrets) != 1 || result.Secrets[0] != "DATADOG_WEBHOOK_TOKEN" {
		t.Errorf("wrote_secrets = %v", result.Secrets)
	}
	if rec.secrets["DATADOG_WEBHOOK_TOKEN"] != "s3cr3t-value" {
		t.Errorf("the sealed value is %q", rec.secrets["DATADOG_WEBHOOK_TOKEN"])
	}
}

// A ROTATION PUBLISHES ANYWAY. The pointer is already correct, so the patch
// is empty, and a value written into the store after the last apply is
// invisible to every running seat until something activates.
func TestRotatingAValueStillAdvancesTheEpoch(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	rotating := withStored(datadogReqs, "webhook_token", "${DATADOG_WEBHOOK_TOKEN}")
	result, err := writer(rec).Write(context.Background(), rotating, setup.Submission{
		Kind:    integration.KindDatadog,
		Values:  map[string]string{"webhook_token": "fresh"},
		Summary: "rotate datadog", Operator: "founder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reloaded {
		t.Fatal("a rotation did not report the reload")
	}
	if len(rec.events) != 2 || !strings.HasPrefix(rec.events[1], "reload:") {
		t.Fatalf("events = %v, want the secret then a reload", rec.events)
	}
}

// A field the vendor does not declare is refused HAVING WRITTEN NOTHING.
// Ignoring it while answering 201 is the worst of both: the caller believes
// the value landed and nothing holds it.
func TestAnUnknownFieldIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	_, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:   integration.KindDatadog,
		Values: map[string]string{"webhook_token": "tok", "site": "eu"},
	})
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if !strings.Contains(err.Error(), "site") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refused submission wrote %v", rec.events)
	}
}

// A LITERAL IN THE SLOT IS REFUSED, and nothing is written. Overwriting it
// would edit the company from a setup form and destroy the credential
// somebody put there deliberately.
func TestALiteralInTheConfigIsRefused(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	held := withStored(datadogReqs, "webhook_token", "already-a-plain-token")
	_, err := writer(rec).Write(context.Background(), held, setup.Submission{
		Kind:   integration.KindDatadog,
		Values: map[string]string{"webhook_token": "tok"},
	})
	var literal *setup.ErrLiteralInConfig
	if !errors.As(err, &literal) {
		t.Fatalf("err = %v, want a literal refusal", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refused submission wrote %v", rec.events)
	}
}

// THE SECRET SURVIVES A FAILED PATCH, and the caller is told which. A value
// sealed under a name nothing points at is inert; deleting it here would be a
// second write on the path where one has already failed.
func TestAFailedPatchLeavesTheSealedValueAndNamesIt(t *testing.T) {
	t.Parallel()
	rec := &recorder{failApp: errors.New("the plane refused")}
	result, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:   integration.KindDatadog,
		Values: map[string]string{"webhook_token": "tok"},
	})
	if err == nil {
		t.Fatal("a failed patch was reported as success")
	}
	if len(result.Secrets) != 1 {
		t.Fatalf("the failure does not name the sealed secret: %v", result.Secrets)
	}
	if rec.secrets["DATADOG_WEBHOOK_TOKEN"] != "tok" {
		t.Error("the sealed value was rolled back")
	}
}

// A STALE BASE IS REFUSED BEFORE ANYTHING IS SEALED. Apply would catch it
// too, but only after the credential is already in the store under a name
// nothing points at, which somebody then has to find and remove.
func TestAStaleBaseIsRefusedBeforeAnythingIsSealed(t *testing.T) {
	t.Parallel()
	rec := &recorder{active: "rev-9"}
	_, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:   integration.KindDatadog,
		Values: map[string]string{"webhook_token": "tok"},
		Expect: "rev-1",
	})
	var stale *setup.ErrStaleBase
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want a stale-base refusal", err)
	}
	if stale.Current != "rev-9" || stale.Base != "rev-1" {
		t.Errorf("the refusal carries %q/%q", stale.Base, stale.Current)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refused submission wrote %v", rec.events)
	}
}

// A TOGGLE IS A BOOLEAN in the document. The strict reader refuses "true" for
// a bool field, so a toggle written as a string would be rejected on every
// submission.
func TestAToggleIsWrittenAsABoolean(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	reqs := []setup.Requirement{{
		Field: "enabled", Kind: setup.KindToggle, Required: true,
		ConfigPath: "integrations.datadog.enabled",
	}}
	if _, err := writer(rec).Write(context.Background(), reqs, setup.Submission{
		Kind: integration.KindDatadog, Values: map[string]string{"enabled": "true"},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.events[0], `"enabled":true`) {
		t.Fatalf("the patch carries %s, want a JSON boolean", rec.events[0])
	}
}

// A PER-SEAT SUBMISSION WRITES THROUGH THE SEAT, and the path it sets is
// relative to it. A merge patch replaces an array wholesale, so patching the
// roster to change one seat would delete every other one.
func TestAPerSeatSubmissionWritesThroughTheSeat(t *testing.T) {
	t.Parallel()
	rec := &recorder{seat: []byte(`{"name":"SRE Lead","handle":"sre-lead"}`)}
	reqs := []setup.Requirement{{
		Field: "bot_token", Kind: setup.KindSecret, Required: true,
		ConfigPath: "integrations.slack.bot_token", Seat: "sre-lead",
		SecretName: "SLACK_BOT_TOKEN_SRE_LEAD",
	}}
	result, err := writer(rec).Write(context.Background(), reqs, setup.Submission{
		Kind: integration.KindSlack, Seat: "sre-lead",
		Values: map[string]string{"bot_token": "xoxb-value"}, Operator: "founder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 2 {
		t.Fatalf("events = %v", rec.events)
	}
	// The secret first, then the seat, exactly as the company-wide path.
	if !strings.HasPrefix(rec.events[0], "secret:SLACK_BOT_TOKEN_SRE_LEAD") {
		t.Fatalf("the first write was %q", rec.events[0])
	}
	if !strings.HasPrefix(rec.events[1], "seat:sre-lead") {
		t.Fatalf("the second write was %q", rec.events[1])
	}
	// THE POINTER, NEVER THE VALUE, and spliced into the seat that was
	// read rather than replacing it.
	if strings.Contains(rec.events[1], "xoxb-value") {
		t.Fatalf("the seat carries the credential: %s", rec.events[1])
	}
	if !strings.Contains(rec.events[1], `"bot_token":"${SLACK_BOT_TOKEN_SRE_LEAD}"`) {
		t.Fatalf("the seat does not point at the secret: %s", rec.events[1])
	}
	if !strings.Contains(rec.events[1], `"handle":"sre-lead"`) {
		t.Fatalf("the write dropped what it did not send: %s", rec.events[1])
	}
	if len(result.Secrets) != 1 {
		t.Errorf("wrote_secrets = %v", result.Secrets)
	}
}

// A path with a list position is refused rather than guessed at: a merge
// patch replaces an array wholesale, so a patch built that way would delete
// every other seat.
func TestAListPositionIsRefused(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	reqs := []setup.Requirement{{
		Field: "bot_token", Kind: setup.KindText,
		ConfigPath: "roles[2].integrations.slack.channel",
	}}
	_, err := writer(rec).Write(context.Background(), reqs, setup.Submission{
		Kind: integration.KindSlack, Values: map[string]string{"bot_token": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "list position") {
		t.Fatalf("err = %v, want a refusal naming the list position", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refused submission wrote %v", rec.events)
	}
}
