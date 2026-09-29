package chart_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
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
// next edit sealed under a second name and left the first referenced by
// nothing — and the seat created on the freed handle sealed its own
// credential under the name the renamed seat's row still pointed at.
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
	before := r.runtimeOf("sarah-chen")

	r.applySeatRekey("op-rename", "sarah-okoro", "sarah-chen")
	write("op-two", "sarah-okoro", "second")
	after := r.runtimeOf("sarah-okoro")
	if after != before {
		t.Errorf("the renamed seat's credential is sealed under a new name: "+
			"%s, then %s — the name is the seat's identity, which no rename "+
			"moves, so a re-seal overwrites the value its row already names",
			before, after)
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
