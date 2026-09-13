package setup_test

import (
	"context"
	"encoding/json"
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
		"the third-party app's own declaration wins": {
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
// silently, on one third-party app, for the seats whose handles start with a
// digit.
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
// outage: the config shows a secret, the third-party app shows a healthy
// hook, and every delivery is refused with nothing naming the variable.
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
	// Both fields of one third-party app merge into one object rather than the
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

// A field the third-party app does not declare is refused HAVING WRITTEN
// NOTHING. Ignoring it while answering 201 is the worst of both: the caller
// believes the value landed and nothing holds it.
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

// A SUBMITTED `${VAR}` NAMES A CREDENTIAL RATHER THAN BEING ONE.
//
// An operator who keeps a token in the sealed store and points the field at
// it is doing what this whole package exists to make possible. Sealing the
// reference as a value would store the literal text "${SHARED_TOKEN}" under
// the field's own name and point the config at THAT: a credential whose
// value is the spelling of another credential, refused by the vendor with
// nothing on any surface saying why.
func TestASubmittedReferenceIsStoredAsAPointer(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	result, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:    integration.KindDatadog,
		Values:  map[string]string{"webhook_token": "${SHARED_TOKEN}", "route_to": "sre-lead"},
		Summary: "point datadog at a stored secret", Operator: "founder",
	})
	if err != nil {
		t.Fatal(err)
	}
	// NOTHING WAS SEALED. Naming an entry is not writing one.
	if len(result.Secrets) != 0 {
		t.Errorf("wrote_secrets = %v, want nothing sealed", result.Secrets)
	}
	if len(rec.secrets) != 0 {
		t.Errorf("the store was written: %v", rec.secrets)
	}
	if len(rec.events) != 1 || !strings.HasPrefix(rec.events[0], "patch:") {
		t.Fatalf("events = %v, want the config write alone", rec.events)
	}
	if !strings.Contains(rec.events[0], `"webhook_token":"${SHARED_TOKEN}"`) {
		t.Fatalf("the patch does not carry the operator's pointer: %s", rec.events[0])
	}
	// NOT THE FIELD'S OWN DERIVED NAME, which is what a pass that sealed
	// first and pointed afterwards would have written.
	if strings.Contains(rec.events[0], "DATADOG_WEBHOOK_TOKEN") {
		t.Errorf("the patch points at the derived name: %s", rec.events[0])
	}
}

// AND A COMPOSITE IS NOT A POINTER. `https://${HOST}/hook` names a variable
// and carries an address beside it, so writing it through as a reference
// would leave the field holding a fragment of a URL where a credential
// belongs. It is a value, and it is sealed like one.
func TestACompositeIsSealedRatherThanPointedAt(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	if _, err := writer(rec).Write(context.Background(), datadogReqs, setup.Submission{
		Kind:    integration.KindDatadog,
		Values:  map[string]string{"webhook_token": "https://${HOST}/hook"},
		Summary: "connect datadog", Operator: "founder",
	}); err != nil {
		t.Fatal(err)
	}
	if rec.secrets["DATADOG_WEBHOOK_TOKEN"] != "https://${HOST}/hook" {
		t.Errorf("the composite was not sealed: %v", rec.secrets)
	}
}

// A CREDENTIAL'S REFERENCE REACHES THE FORM; THE CREDENTIAL NEVER DOES.
//
// The form has to open showing which entry of the sealed store a field
// reads, or an operator cannot tell whether they are editing a pointer or
// about to replace it. A literal in the same position IS the credential, and
// a company that wrote one by hand must not have it read back to a screen.
func TestOnlyAReferenceLeavesOnTheWire(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ stored, want string }{
		"a reference": {stored: "${JIRA_TOKEN}", want: "${JIRA_TOKEN}"},
		"padded":      {stored: "  ${JIRA_TOKEN}  ", want: "${JIRA_TOKEN}"},
		"a literal":   {stored: "ATATT-real-credential", want: ""},
		"a composite": {stored: "https://${HOST}/x", want: ""},
		"nothing yet": {stored: "", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(setup.Requirement{
				Field: "token", Kind: setup.KindSecret,
				ConfigPath: "integrations.jira.token", Stored: tc.stored,
			})
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			if out.Value != tc.want {
				t.Errorf("value = %q, want %q", out.Value, tc.want)
			}
			if tc.want == "" && strings.Contains(string(body), tc.stored) && tc.stored != "" {
				t.Errorf("the payload carries the stored value: %s", body)
			}
		})
	}
}

// A CREDENTIAL'S RESOLVED VALUE HAS NO PATH ONTO THIS WIRE, and the one
// function that resolves anything for the form is where that has to hold.
//
// The links a form draws are built out of these values, so a field naming an
// entry of the store has to say what it currently reads. A credential is the
// exception, and it is not a narrow one: no link in this tree is built out of
// a secret, and the day one is, it must not be this that supplies it.
func TestOnlyANonCredentialSaysWhatItsReferenceReads(t *testing.T) {
	t.Parallel()
	resolve := func(name string) (string, bool) {
		return map[string]string{"ORG": "org-42", "KEY": "the-credential"}[name], true
	}
	reqs := []setup.Requirement{
		{Field: "org_id", Kind: setup.KindID, Stored: "${ORG}"},
		{Field: "api_key", Kind: setup.KindSecret, Stored: "${KEY}"},
		{Field: "url", Kind: setup.KindURL, Stored: "https://acme.example.com"},
		{Field: "cloud_id", Kind: setup.KindID, Stored: "${GONE}"},
	}
	setup.FillEffective(reqs, resolve)

	if reqs[0].Effective != "org-42" {
		t.Errorf("org_id effective = %q, want the resolved organization", reqs[0].Effective)
	}
	if reqs[1].Effective != "" {
		t.Errorf("a credential's resolved value reached the wire: %q", reqs[1].Effective)
	}
	// A LITERAL IS ALREADY THE VALUE, so there is nothing to say about it and
	// saying it anyway would put the same string on the wire twice.
	if reqs[2].Effective != "" {
		t.Errorf("a literal was given an effective value: %q", reqs[2].Effective)
	}
	// A reference naming nothing reads as nothing, which is what stops a
	// link being drawn to an address with a hole in it.
	if reqs[3].Effective != "" {
		t.Errorf("an unresolved reference = %q, want empty", reqs[3].Effective)
	}
	body, err := json.Marshal(reqs[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "the-credential") {
		t.Fatalf("the credential is on the wire: %s", body)
	}
}

// A SUBMITTED VALUE IS TRIMMED, and a space INSIDE a single token is refused.
//
// The two are different mistakes. A trailing space is a paste artifact with
// one obvious intent, and sending an operator back to a form to delete a
// character they cannot see is not a fix. A space in the middle is a value
// nothing answers to, so it is refused naming the field.
func TestASubmissionIsTrimmedAndRefusesASpaceInsideAToken(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{
		{Field: "url", Kind: setup.KindURL, ConfigPath: "integrations.jira.url"},
		{Field: "email", Kind: setup.KindEmail, ConfigPath: "integrations.jira.email"},
		{Field: "role", Kind: setup.KindText, ConfigPath: "integrations.datadog.provisioning.role"},
	}

	rec := &recorder{}
	if _, err := writer(rec).Write(context.Background(), reqs, setup.Submission{
		Kind: integration.KindJira,
		Values: map[string]string{
			"url":   "  https://acme.atlassian.net  ",
			"email": "\tops@acme.example.com\n",
			// FREE TEXT KEEPS ITS SPACES. A role is several words.
			"role": "  Datadog Read Only Role  ",
		},
		Summary: "connect jira", Operator: "founder",
	}); err != nil {
		t.Fatal(err)
	}
	patch := rec.events[0]
	for _, want := range []string{
		`"url":"https://acme.atlassian.net"`,
		`"email":"ops@acme.example.com"`,
		`"role":"Datadog Read Only Role"`,
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("the patch does not carry %s: %s", want, patch)
		}
	}

	_, err := writer(&recorder{}).Write(context.Background(), reqs, setup.Submission{
		Kind:    integration.KindJira,
		Values:  map[string]string{"email": "ops@acme example.com"},
		Summary: "connect jira", Operator: "founder",
	})
	if err == nil {
		t.Fatal("a space inside an email was accepted")
	}
	if !strings.Contains(err.Error(), "integrations.jira.email") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// THE REFUSAL SHOWS THE VALUE THAT HAS THE SPACE IN IT.
//
// It quoted the field NAME, which by construction has no space in it, so the
// message asserted something false about the one identifier it showed and
// withheld the only thing an operator can act on: which of the things they
// pasted is wrong. Every kind that reaches this branch is Kind.Tight — a URL,
// an id, an email — and KindSecret is deliberately not one, so the value
// echoed here is never a credential.
func TestAnInteriorSpaceIsRefusedNamingTheValue(t *testing.T) {
	t.Parallel()
	reqs := []setup.Requirement{{
		Field: "url", Kind: setup.KindURL, Required: true,
		ConfigPath: "integrations.gitlab.url",
	}}
	rec := &recorder{}
	_, err := writer(rec).Write(context.Background(), reqs, setup.Submission{
		Kind:    integration.KindGitLab,
		Values:  map[string]string{"url": "https://gitlab example.com"},
		Summary: "connect gitlab", Operator: "founder",
	})
	if err == nil {
		t.Fatal("a url with a space in the middle was accepted")
	}
	if !strings.Contains(err.Error(), "https://gitlab example.com") {
		t.Errorf("the refusal is %q; it does not show the value that has the "+
			"space in it, which is the only thing an operator can act on",
			err.Error())
	}
	if len(rec.events) != 0 {
		t.Errorf("a refused submission wrote %v", rec.events)
	}
}

// A GATED REQUIREMENT IS NEEDED EXACTLY WHEN ITS ANSWER IS GIVEN.
//
// Neither `Required` alone can say this. GitHub's organization token is the
// pair it exists for: it is the one thing standing between "every repository
// in the organization" and that being true, and a credential the other answer
// never uses. Required, it blocked a connect that needed nothing; optional, it
// let the API store a choice the engine cannot carry out.
//
// THE ASSERTION THAT MATTERS IS THE TRUE ONE. A gate honoured only in the
// direction where it agrees with `Required` is a gate nothing is checking:
// this pins the case where the two disagree — `Required: false`, and needed
// anyway because of the answer beside it.
func TestAGatedRequirementFollowsTheAnswerItIsGatedOn(t *testing.T) {
	t.Parallel()
	const on, off = "true", "false"
	token := setup.Requirement{
		Field:        "token",
		Required:     false,
		RequiredWhen: &setup.Gate{Field: "coverage", Equals: on},
	}
	list := func(answer string) []setup.Requirement {
		return []setup.Requirement{{Field: "coverage", Stored: answer}, token}
	}

	if token.Needed(list(on)) != true {
		t.Error("a field the stored answer demands is not needed, so the API " +
			"stores a choice it cannot carry out")
	}
	if token.Needed(list(off)) != false {
		t.Error("a field no answer demands is needed, so a company taking the " +
			"recommendation cannot connect without a credential it never uses")
	}
	// AN UNGATED REQUIREMENT IS UNCHANGED, whatever else the list holds.
	plain := setup.Requirement{Field: "org", Required: true}
	if !plain.Needed(list(off)) {
		t.Error("an ungated requirement stopped answering to Required")
	}
}

// AND A GATE NAMING A FIELD THAT IS NOT THERE IS SHUT.
//
// The alternative reads a vendor's typo as "always required", which turns one
// package's mistake into every company's blocked connect — and the failure is
// silent, because a field demanded for no reason looks exactly like a field
// that is genuinely needed.
func TestAGateOnAMissingFieldIsShut(t *testing.T) {
	t.Parallel()
	r := setup.Requirement{
		Field:        "token",
		Required:     true,
		RequiredWhen: &setup.Gate{Field: "typo", Equals: "true"},
	}
	if r.Needed([]setup.Requirement{{Field: "coverage", Stored: "true"}}) {
		t.Error("a gate on a field nobody declares reads as open, so a typo " +
			"blocks every connect")
	}
}
