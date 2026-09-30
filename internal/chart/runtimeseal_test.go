package chart_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RUNTIME HALF'S CREDENTIALS ARE SEALED LIKE THE ADDRESS IS.
//
// A seat's runtime half is where nearly every credential it holds lives — its
// per-server `mcp_env`, its sandbox's env, its own Slack app's token and
// signing secret, its GitHub App's key — and it is opaque to this domain. A
// writer that sealed only the one field it could read, the address, put every
// one of those on the log in the clear: in the record every node applies, in
// every node's rows, in every snapshot and in every backup of both.

// leaked reports every place a literal reached that it must not: the payload of
// any record on the log, and the rows of both object tables.
func (r *writeRig) leaked(literals ...string) []string {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	var out []string
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		for _, literal := range literals {
			if strings.Contains(string(payload), literal) {
				out = append(out, fmt.Sprintf("record %d carries %s", seq, literal))
			}
		}
	}
	rows := r.column(`SELECT email || CAST(document AS TEXT) FROM chart_seats`)
	rows = append(rows, r.column(`SELECT CAST(document AS TEXT) FROM chart_units`)...)
	for _, row := range rows {
		for _, literal := range literals {
			if strings.Contains(row, literal) {
				out = append(out, "a row carries "+literal)
			}
		}
	}
	return out
}

// seatRuntime is a seat's runtime half holding a literal in every credential
// field the running seat has, beside a whole reference and a composite.
const seatRuntime = `{
	"mcp_env": {"github": {
		"Authorization": "Bearer ${GH_TOKEN} ghp-LITERAL-HEADER",
		"GITHUB_TOKEN": "ghp-LITERAL-ENV",
		"GITHUB_HOST": "${GH_HOST}"}},
	"sandbox": {"enabled": true, "env": {"NPM_TOKEN": "npm-LITERAL"},
		"setup": [{"name": "registry", "files": {"/root/.npmrc": "npmrc-LITERAL"}}]},
	"slack": {"bot_token": "xoxb-LITERAL", "signing_secret": "slack-signing-LITERAL"},
	"mattermost": {"bot_token": "mm-LITERAL", "username": "sarah-bot"},
	"github": {"app_id": 7, "private_key": "pem-LITERAL", "webhook_secret": "gh-hook-LITERAL"}
}`

// seatLiterals are the credentials seatRuntime holds, which is everything that
// must never reach a record or a row.
var seatLiterals = []string{
	"ghp-LITERAL-HEADER", "ghp-LITERAL-ENV", "npm-LITERAL", "npmrc-LITERAL",
	"xoxb-LITERAL", "slack-signing-LITERAL", "mm-LITERAL", "pem-LITERAL",
	"gh-hook-LITERAL",
}

func TestALiteralInASeatsRuntimeHalfIsSealedAndNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))

	if _, err := r.seat("op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
		Runtime: json.RawMessage(seatRuntime),
	}); err != nil {
		t.Fatalf("write a seat whose runtime half holds literals: %v", err)
	}
	if got := r.leaked(seatLiterals...); len(got) > 0 {
		t.Errorf("a literal credential in the runtime half reached the log or "+
			"the rows, which every node applies and every snapshot and backup "+
			"copies: %v", got)
	}
	// EVERY LITERAL IS IN THE STORE, and the row's references resolve back to
	// exactly what was written — a seal that lost the value, or reassembled a
	// composite wrongly, is a seat whose credential is silently different.
	resolved := r.resolvedRuntime("sarah-chen")
	for path, want := range map[string]string{
		"mcp_env.github.Authorization": "Bearer ${GH_TOKEN} ghp-LITERAL-HEADER",
		"mcp_env.github.GITHUB_TOKEN":  "ghp-LITERAL-ENV",
		"mcp_env.github.GITHUB_HOST":   "${GH_HOST}",
		"sandbox.env.NPM_TOKEN":        "npm-LITERAL",
		"slack.bot_token":              "xoxb-LITERAL",
		"slack.signing_secret":         "slack-signing-LITERAL",
		"mattermost.bot_token":         "mm-LITERAL",
		"github.private_key":           "pem-LITERAL",
		"github.webhook_secret":        "gh-hook-LITERAL",
	} {
		if got := resolved[path]; got != want {
			t.Errorf("%s resolves to %q through the store, want %q", path, got, want)
		}
	}
	// AND WHAT IS NOT A CREDENTIAL IS NOT TOUCHED: the half is the seat's
	// configuration, and a writer that sealed a username or an app id would
	// be a seat nobody can read back.
	if got := r.runtimeOf("sarah-chen"); !strings.Contains(got, `"sarah-bot"`) ||
		!strings.Contains(got, `"app_id":7`) {
		t.Errorf("a field that holds no credential was rewritten: %s", got)
	}
}

func TestALiteralInAUnitsRuntimeHalfIsSealedAndNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))

	if _, err := r.writer.WriteUnit(t.Context(), "op-runtime", chart.UnitContent{
		Key: "platform", Name: "Platform",
		Runtime: json.RawMessage(`{"mcp_env": {"tracker": {"TOKEN": "unit-LITERAL"}}}`),
	}); err != nil {
		t.Fatalf("write a unit whose runtime half holds a literal: %v", err)
	}
	r.drain()
	if got := r.leaked("unit-LITERAL"); len(got) > 0 {
		t.Errorf("a literal credential in a unit's runtime half reached the log "+
			"or the rows: %v", got)
	}
}

// A SETUP STEP'S FILE IS SEALED WHOLE, AND READS BACK AS THE BODY WRITTEN.
//
// A file is CONTENT: it is written into a box, and nothing on the way there
// expands it, so a `${…}` inside a script or an .npmrc is the FILE's syntax.
// Cut around it the way a header is, a body was stored as the chart's own
// references strung together with the file's `${NPM_TOKEN}` between them, and
// the box received exactly that. Each literal body is ONE reference to the
// exact body; a body that is itself one `${VAR}` is a pointer and is kept; and
// a SETTING beside it — the step's env — is still cut around its references,
// because the engine expands those.
func TestASetupStepsFileIsSealedWholeAndReadsBackAsWritten(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	const (
		npmrc  = "registry=https://r.example.com\n//r.example.com/:_authToken=${NPM_TOKEN}\n"
		helper = "#!/bin/sh\necho \"${HOME}\" npmrc-HELPER-LITERAL\n"
	)
	runtime, err := json.Marshal(map[string]any{"sandbox": map[string]any{
		"enabled": true, "setup": []any{map[string]any{
			"name": "registry",
			"files": map[string]string{
				"/root/.npmrc":     npmrc,
				"/usr/local/bin/h": helper,
				"/etc/pointer":     "${MY_FILE}",
			},
			"env": map[string]string{"AUTH": "Bearer ${GH_TOKEN} env-LITERAL"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.seat("op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen", Runtime: runtime,
	}); err != nil {
		t.Fatalf("write a seat whose setup step holds files: %v", err)
	}
	if got := r.leaked("npmrc-HELPER-LITERAL", "r.example.com", "env-LITERAL"); len(got) > 0 {
		t.Errorf("a setup step's credential reached the log or the rows: %v", got)
	}
	var row struct {
		Sandbox struct {
			Setup []struct {
				Files map[string]string `json:"files"`
				Env   map[string]string `json:"env"`
			} `json:"setup"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(r.runtimeOf("sarah-chen")), &row); err != nil {
		t.Fatal(err)
	}
	files := row.Sandbox.Setup[0].Files
	lookup := func(name string) (string, bool) { return r.sealer.get(name) }
	for path, want := range map[string]string{"/root/.npmrc": npmrc, "/usr/local/bin/h": helper} {
		name, whole := envref.Whole(files[path])
		if !whole || !chart.OwnsSecret(name) {
			t.Errorf("%s is stored as %q, want ONE sealed reference — cut "+
				"around the file's own ${…}, the box receives the chart's "+
				"references strung together", path, files[path])
			continue
		}
		if got, _ := secrets.ReadContent(files[path], lookup); got != want {
			t.Errorf("%s reads back as %q, want the body written, %q", path, got, want)
		}
	}
	if files["/etc/pointer"] != "${MY_FILE}" {
		t.Errorf("a file that is one ${VAR} is stored as %q, want the pointer "+
			"as written", files["/etc/pointer"])
	}
	if auth := row.Sandbox.Setup[0].Env["AUTH"]; !strings.Contains(auth, "${GH_TOKEN}") {
		t.Errorf("the step's env, a SETTING, was sealed whole to %q — its "+
			"${GH_TOKEN} is the engine's own and has to stay where the "+
			"resolver finds it", auth)
	}
}

// A RUNTIME HALF READ MASKED AND HANDED BACK KEEPS EVERY CREDENTIAL IT HELD.
//
// Every surface that serves the half masks it, so GET-edit-PUT hands the
// marker back in every credential field the half has. Stored, it replaces a
// working token with twelve characters; restored from the row in the decide's
// own snapshot, the edit changes exactly what the person changed — and a list
// member's credential is restored from THAT member, found by its identity,
// never from whichever member now sits in its slot.
func TestARuntimeHalfReadMaskedAndHandedBackKeepsItsCredentials(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	if _, err := r.seat("op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen", Runtime: json.RawMessage(`{
			"mcp_env": {"github": {"GITHUB_TOKEN": "ghp-LITERAL",
				"Authorization": "Bearer ${GH_TOKEN}"}},
			"sandbox": {"enabled": true, "setup": [
				{"name": "npm", "env": {"TOKEN": "npm-LITERAL"}},
				{"name": "pip", "env": {"TOKEN": "pip-LITERAL"}}]},
			"mattermost": {"bot_token": "mm-LITERAL", "username": "sarah-bot"}}`),
	}); err != nil {
		t.Fatalf("seed the seat: %v", err)
	}
	sealed := len(r.sealer.sealed)

	// THE ROUND TRIP, and a real edit inside it: the two setup steps swap
	// places and the bot's username changes.
	// A WHOLE REFERENCE IS SERVED AS ITSELF and a composite as the mask —
	// so the round trip hands back both, and the second is what a restore
	// is for. Every setup step's token is masked by hand as well, which is
	// what a surface serving an older row's literal would hand back.
	masked := string(chart.MaskRuntime(org.RuntimeShape{}, chart.KindSeat,
		json.RawMessage(r.runtimeOf("sarah-chen"))))
	if !strings.Contains(masked, `"Authorization":"`+redacted()+`"`) {
		t.Fatalf("the masked half shows the composite header rather than the "+
			"mask: %s", masked)
	}
	var edited map[string]any
	if err := json.Unmarshal([]byte(masked), &edited); err != nil {
		t.Fatal(err)
	}
	for _, step := range edited["sandbox"].(map[string]any)["setup"].([]any) {
		step.(map[string]any)["env"].(map[string]any)["TOKEN"] = redacted()
	}
	sandbox := edited["sandbox"].(map[string]any)
	steps := sandbox["setup"].([]any)
	sandbox["setup"] = []any{steps[1], steps[0]}
	edited["mattermost"].(map[string]any)["username"] = "sarah-okoro-bot"
	body, _ := json.Marshal(edited)
	if _, err := r.seat("op-round-trip", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen", Runtime: body,
	}); err != nil {
		t.Fatalf("hand the masked half back: %v", err)
	}

	got := r.resolvedRuntime("sarah-chen")
	for path, want := range map[string]string{
		"mcp_env.github.GITHUB_TOKEN":  "ghp-LITERAL",
		"mcp_env.github.Authorization": "Bearer ${GH_TOKEN}",
		"sandbox.setup.0.env.TOKEN":    "pip-LITERAL",
		"sandbox.setup.1.env.TOKEN":    "npm-LITERAL",
		"mattermost.bot_token":         "mm-LITERAL",
		"mattermost.username":          "sarah-okoro-bot",
	} {
		if got[path] != want {
			t.Errorf("%s is %q after the round trip, want %q", path, got[path], want)
		}
	}
	if len(r.sealer.sealed) != sealed {
		t.Errorf("a masked round trip sealed %d new values — a restored mask "+
			"is the stored reference, and sealing it again puts a pointer "+
			"inside the store", len(r.sealer.sealed)-sealed)
	}
}

// AND A MASK WITH NOTHING BEHIND IT IS REFUSED, NAMING THE FIELD.
func TestAMaskInTheRuntimeHalfWithNoStoredValueIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	_, err := r.seat("op-mask", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
		Runtime: json.RawMessage(`{"mcp_env": {"tracker": {"TOKEN": "` +
			redacted() + `"}}}`),
	})
	if !errors.Is(err, chart.ErrRefused) {
		t.Fatalf("a mask over a field nothing is stored in answered %v, want "+
			"a refusal — whatever it stored is either the marker or an empty "+
			"credential the caller never asked for", err)
	}
	if !strings.Contains(err.Error(), "mcp_env.tracker.TOKEN") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// A SEALED VALUE IS NAMED BY THE SEAT'S IDENTITY, NOT BY ITS ADDRESS.
//
// A handle is an address a rename reassigns, and a retired one can be handed
// to a later hire. Named by the handle a seat answers to, a renamed seat's
// values were filed under whichever address it held when each was written,
// and a hire given the freed handle derived names in the renamed seat's space
// — so a write of the same field under a reused operation id sealed over the
// renamed seat's value. Under the IDENTITY the seat was created under, a
// renamed seat's every write derives its names where its first did.
func TestASealedValueIsNamedByTheSeatsIdentity(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	write := func(opID, handle, token string) {
		t.Helper()
		if _, err := r.seat(opID, chart.SeatContent{
			Handle: handle, Name: "Sarah",
			Runtime: json.RawMessage(`{"mcp_env": {"tracker": {"TOKEN": "` + token + `"}}}`),
		}); err != nil {
			t.Fatalf("write %s: %v", handle, err)
		}
	}
	write("op-one", "sarah-chen", "first")

	r.applySeatRekey("op-rename", "sarah-okoro", "sarah-chen")
	write("op-two", "sarah-okoro", "second")
	identity := chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}
	want := chart.SecretRef(identity, "op-two", "mcp_env", "tracker", "TOKEN")
	if after := r.runtimeOf("sarah-okoro"); !strings.Contains(after, want) {
		t.Errorf("the renamed seat's credential is sealed as %s, want under its "+
			"identity, %s — the address it answers to now is one a rename "+
			"reassigns and a later hire may be given", after, want)
	}
	if got := r.resolvedRuntime("sarah-okoro")["mcp_env.tracker.TOKEN"]; got != "second" {
		t.Errorf("the renamed seat resolves %q, want the value it was just given", got)
	}
}

// runtimeOf is a seat's runtime half as its row holds it.
func (r *writeRig) runtimeOf(handle string) string {
	r.t.Helper()
	docs := r.column(`SELECT CAST(document AS TEXT) FROM chart_seats WHERE handle = ?`,
		handle)
	if len(docs) != 1 {
		r.t.Fatalf("no row for seat %q", handle)
	}
	var row struct {
		Runtime json.RawMessage `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(docs[0]), &row); err != nil {
		r.t.Fatalf("decode seat %q: %v", handle, err)
	}
	return string(row.Runtime)
}

// resolvedRuntime is every string in a seat's runtime half, by dotted path,
// with each reference the chart sealed expanded through the store — what the
// running seat reads once a node resolves it. A reference the store does not
// hold is left as written, which is how a pointer at the operator's own
// variable reads.
func (r *writeRig) resolvedRuntime(handle string) map[string]string {
	r.t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(r.runtimeOf(handle)), &doc); err != nil {
		r.t.Fatalf("decode %s's runtime: %v", handle, err)
	}
	out := map[string]string{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(e, strings.TrimPrefix(path+"."+k, "."))
			}
		case []any:
			for i, e := range x {
				walk(e, fmt.Sprintf("%s.%d", path, i))
			}
		case string:
			expanded, _ := envref.Expand(x, func(name string) (string, bool) {
				if value, sealed := r.sealer.get(name); sealed {
					return value, true
				}
				return "${" + name + "}", true
			})
			out[path] = expanded
		}
	}
	walk(doc, "")
	return out
}

// A WRITE THAT DOES NOT LAND CHANGES NO CREDENTIAL ANY ROW NAMES.
//
// A seal happens inside the decide, before the write is arbitrated or
// published — and under a name derived from the object and the field alone it
// wrote over the value the live row already named. So a write refused for an
// over-long name, one whose caller went away after the seal, one lost to a
// concurrent writer, all left the store holding a credential the caller was
// told had been rejected, and every node installed it at its next re-read. A
// refusal the decide can make on its own now comes before any seal and writes
// nothing at all; one it cannot — the record never reaching the log — leaves a
// value only its own write's name holds, which no row names and the orphan
// sweep collects. Either way the credential the row names is the one it named.
func TestAWriteThatDoesNotLandChangesNoCredentialARowNames(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seats", seatOp(chart.SeatAgent, "sarah-chen", ""),
		seatOp(chart.SeatAgent, "bo-lee", ""))
	token := func(value string) json.RawMessage {
		return json.RawMessage(`{"mcp_env": {"github": {"GITHUB_TOKEN": "` + value + `"}}}`)
	}
	for _, handle := range []string{"sarah-chen", "bo-lee"} {
		email := ""
		if handle == "sarah-chen" {
			email = "sarah@example.com"
		}
		if _, err := r.seat("op-one-"+handle, chart.SeatContent{
			Handle: handle, Name: handle, Email: email, Runtime: token("OLD-token"),
		}); err != nil {
			t.Fatalf("seed %s: %v", handle, err)
		}
	}
	unchanged := func(t *testing.T, handle, row string) {
		t.Helper()
		r.drain()
		if got := r.runtimeOf(handle); got != row {
			t.Errorf("the row moved to %s, want %s — the write did not land", got, row)
		}
		if got := r.resolvedRuntime(handle)["mcp_env.github.GITHUB_TOKEN"]; got != "OLD-token" {
			t.Errorf("the credential %s's row names resolves to %q after a write "+
				"that did not land, want the one it named, OLD-token", handle, got)
		}
	}

	t.Run("refused by the decide on its own, before anything is sealed", func(t *testing.T) {
		row := r.runtimeOf("sarah-chen")
		sealed := len(r.sealer.sealed)
		for _, c := range []struct {
			name    string
			content chart.SeatContent
		}{
			{"a name past its cap", chart.SeatContent{Handle: "sarah-chen",
				Name: strings.Repeat("n", chart.MaxName+1), Runtime: token("NEW-token")}},
			{"a masked address with nothing behind it", chart.SeatContent{
				Handle: "bo-lee", Name: "bo-lee", Email: redacted(),
				Runtime: token("NEW-token")}},
		} {
			if _, err := r.writer.WriteSeat(t.Context(), "op-refused-"+c.name,
				c.content); !errors.Is(err, chart.ErrRefused) {
				t.Fatalf("%s: err = %v, want a refusal", c.name, err)
			}
		}
		if got := len(r.sealer.sealed); got != sealed {
			t.Errorf("a write the decide refused on its own sealed %d values on "+
				"the way to the refusal", got-sealed)
		}
		unchanged(t, "sarah-chen", row)
	})

	t.Run("sealed, and its caller gone before the publish", func(t *testing.T) {
		row := r.runtimeOf("sarah-chen")
		ctx, cancel := context.WithCancel(t.Context())
		r.sealer.mu.Lock()
		r.sealer.afterSeal = cancel
		r.sealer.mu.Unlock()
		defer func() {
			r.sealer.mu.Lock()
			r.sealer.afterSeal = nil
			r.sealer.mu.Unlock()
		}()
		if result, err := r.writer.WriteSeat(ctx, "op-abandoned", chart.SeatContent{
			Handle: "sarah-chen", Name: "sarah-chen", Email: redacted(),
			Runtime: token("NEW-token"),
		}); err == nil && result.Outcome == statelog.OutcomeApplied {
			t.Fatalf("a write whose caller went away before its publish applied")
		}
		unchanged(t, "sarah-chen", row)
		// WHAT IT SEALED IS NAMED BY NOTHING, which is what the sweep takes.
		orphan := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
			"op-abandoned", "mcp_env", "github", "GITHUB_TOKEN")
		if got, _ := r.sealer.get(orphan); got != "NEW-token" {
			t.Fatalf("the abandoned write's value is not under its own name: %q", got)
		}
		if r.sealedNames()[orphan] {
			t.Errorf("a row names %s, a value whose write never landed", orphan)
		}
	})

	// A LANDED OPERATION ID IS ANSWERED BY THE LEDGER BEFORE ANY DECIDE RUNS
	// (statelog.Snap.Held), so a retry carrying another value is the landed
	// write, collapsed — never a second decision, and never a seal: the value
	// it carries is not sealed under any name, and the row keeps what the
	// operation wrote. Telling a reused KEY from a retry is the surface's
	// job, which binds a key to its request (chartapi's request digest); the
	// writer's is only that a held id writes nothing.
	t.Run("an operation id reused for another value", func(t *testing.T) {
		row := r.runtimeOf("sarah-chen")
		sealed := len(r.sealer.sealed)
		result, err := r.writer.WriteSeat(t.Context(), "op-one-sarah-chen", chart.SeatContent{
			Handle: "sarah-chen", Name: "sarah-chen", Email: redacted(),
			Runtime: token("NEW-token"),
		})
		if err != nil || !result.Collapsed {
			t.Fatalf("a landed operation id retried with another value = %+v, %v; "+
				"want the landed write, collapsed", result, err)
		}
		if got := len(r.sealer.sealed); got != sealed {
			t.Errorf("a collapsed retry sealed %d values, which nothing names",
				got-sealed)
		}
		unchanged(t, "sarah-chen", row)
	})
}

// A SEALED REFERENCE A WRITE STATES AGAIN IS HELD, AND ONE THE STORE NO LONGER
// HOLDS IS REFUSED.
//
// A `${CHART_…}` reference travels — a read serves it, an export writes it into
// a file, an import writes it back — and the row it lands on may not be the
// row, or the moment, it was sealed for. The store collects a value once no
// row has named it for an hour, so a file taken before a rotation names one
// that may be gone: written back as it was, the reference was accepted and
// resolved to nothing on every node, with the value itself unrecoverable. So a
// reference the replaced row does not already name is HELD — confirmed, and
// its version moved so no sweep that judged it can delete it under the record
// about to name it — and refused, naming the field, where nothing holds it. A
// reference the row already names is asked nothing.
func TestARestatedSealedReferenceIsHeldOrRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seats", seatOp(chart.SeatAgent, "sarah-chen", ""),
		seatOp(chart.SeatAgent, "bo-lee", ""))
	runtime := func(token string) json.RawMessage {
		return json.RawMessage(`{"mcp_env": {"github": {"GITHUB_TOKEN": "` + token + `"}}}`)
	}
	sarah := chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}
	first := chart.SecretName(sarah, "op-one", "mcp_env", "github", "GITHUB_TOKEN")
	second := chart.SecretName(sarah, "op-two", "mcp_env", "github", "GITHUB_TOKEN")
	for _, w := range []struct{ op, token string }{
		{"op-one", "tok-one"}, {"op-two", "tok-two"},
	} {
		if _, err := r.seat(w.op, chart.SeatContent{Handle: "sarah-chen",
			Name: "Sarah", Runtime: runtime(w.token)}); err != nil {
			t.Fatalf("write %s: %v", w.op, err)
		}
	}
	// THE ROTATION LEFT THE FIRST VALUE NAMED BY NOTHING, and the sweep took it.
	r.sealer.forget(first)

	_, err := r.seat("op-stale", chart.SeatContent{Handle: "bo-lee", Name: "Bo",
		Runtime: runtime("${" + first + "}")})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(),
		"mcp_env.github.GITHUB_TOKEN") || !strings.Contains(err.Error(), first) {
		t.Fatalf("a reference to a value the store no longer holds answered %v, "+
			"want a refusal naming the field and the name — accepted, the seat "+
			"resolves an empty credential and nothing says why", err)
	}
	if got := r.runtimeOf("bo-lee"); strings.Contains(got, first) {
		t.Errorf("the refused reference reached the row: %s", got)
	}

	before := len(r.sealer.holds())
	if _, err := r.seat("op-copy", chart.SeatContent{Handle: "bo-lee", Name: "Bo",
		Runtime: runtime("${" + second + "}")}); err != nil {
		t.Fatalf("a reference to a value the store holds was refused: %v", err)
	}
	if got := r.sealer.holds()[before:]; !slices.Equal(got, []string{second}) {
		t.Errorf("stating a sealed reference the row did not name held %v, want "+
			"%s — unheld, a sweep that judged it nobody's deletes it under the "+
			"record about to name it", got, second)
	}
	if got := r.resolvedRuntime("bo-lee")["mcp_env.github.GITHUB_TOKEN"]; got != "tok-two" {
		t.Errorf("the restated reference resolves to %q, want tok-two", got)
	}

	// AND THE ROW'S OWN REFERENCE, restated beside a change, asks nothing.
	before = len(r.sealer.holds())
	if _, err := r.seat("op-extend", chart.SeatContent{Handle: "sarah-chen",
		Name: "Sarah", Runtime: json.RawMessage(`{"mcp_env": {"github": {` +
			`"GITHUB_TOKEN": "${` + second + `}", "GITHUB_HOST": "${GH_HOST}"}}}`),
	}); err != nil {
		t.Fatalf("restate the row's own reference beside a change: %v", err)
	}
	if got := r.sealer.holds()[before:]; len(got) != 0 {
		t.Errorf("restating the row's own reference held %v — every sweep sees "+
			"a value its row names, so there is nothing to hold", got)
	}
}
