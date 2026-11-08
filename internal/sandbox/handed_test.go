package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// EVERY SECRET A LAUNCH HANDS ITS BOX IS KEPT FOR ITS SESSION'S SCRUB, AND
// NOTHING ELSE IS, class by class, from the very request the job is started
// with: what the engine says sits in a credential position
// ([LaunchRequest.Secrets]), its MCP servers' env and headers and what their
// url's references resolved to, the run's own export token, and every string of
// the login the box was seeded with, read from the file it was seeded from; and
// where that login lives, so a capture reads it again in the box and on the
// host. Not the seat's handle or email, a setting, the telemetry's protocol, the
// unresolved text of a reference or a setup step's file: none is a secret, and
// each, scrubbed by value, was lost from every kept transcript that named it.
// Nothing blank and nothing twice, and the whole of it round-trips through what
// the row's seal covers.
//
// Mutation: leave a class out of handedSecrets, add the run's environment or
// its setup steps back whole, read the login only at the capture, or keep a
// blank value, and a check below goes red.
func TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse(t *testing.T) {
	t.Parallel()
	login := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"login-access-value",`+
		`"refreshToken":"login-refresh-value","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := LaunchRequest{
		Spec:    Spec{CredentialFiles: map[string]string{".claude/.credentials.json": login}},
		Secrets: []string{"resolved-env-token", "credential-named-token", "  ", "resolved-env-token"},
		Setup: []SetupStep{{
			Env: map[string]string{"NPM_TOKEN": "${NPM_TOKEN}"},
			Files: map[string]string{
				"/home/user/.npmrc":     "//registry.example.com/:_authToken=${NPM_TOKEN}",
				"/home/user/git-env.sh": "git config user.name agent-swe-the-seat",
			},
		}},
		MCPServers: map[string]MCPServer{
			"zapier": {Name: "zapier", Transport: TransportHTTP,
				URL:     "https://mcp.example.com/s/url-path-secret-value/mcp",
				Headers: map[string]string{"Authorization": "Bearer mcp-header-value"},
				Secrets: []string{"url-path-secret-value"}},
			"files": {Name: "files", Transport: TransportStdio, Command: "mcp-files",
				Env: map[string]string{"API_TOKEN": "mcp-env-value"}},
		},
	}
	env := map[string]string{
		"CREWLET_AGENT_HANDLE": "agent-swe-the-seat", "CREWLET_AGENT_EMAIL": "agent-swe@example.com",
		"NODE_ENV": "production-settings", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		otelBoxEndpointVar: "http://host.docker.internal:8080" + otelTokenPath + "the-runs-export-token",
		"GITHUB_TOKEN":     "resolved-env-token",
		GitTokenVar:        "the-runs-gitlet-token",
	}
	handed := handedSecrets(req, env)
	for class, want := range map[string]string{
		"a value the engine resolved from a reference": "resolved-env-token",
		"a variable named for a credential":            "credential-named-token",
		"an MCP server's header":                       "Bearer mcp-header-value",
		"an MCP server's env":                          "mcp-env-value",
		"what an MCP server's url's reference held":    "url-path-secret-value",
		"the run's own export token":                   "the-runs-export-token",
		"the run's Gitlet credential":                  "the-runs-gitlet-token",
		"the seeded login's access token":              "login-access-value",
		"the seeded login's refresh token":             "login-refresh-value",
	} {
		if !slices.Contains(handed.Values, want) {
			t.Errorf("%s (%q) is not kept for the scrub: %q", class, want, handed.Values)
		}
	}
	for class, not := range map[string]string{
		"the seat's handle":                  "agent-swe-the-seat",
		"the seat's email":                   "agent-swe@example.com",
		"a setting":                          "production-settings",
		"the telemetry's protocol":           "http/protobuf",
		"the unresolved text of a reference": "${NPM_TOKEN}",
		"a setup step's file":                "//registry.example.com/:_authToken=${NPM_TOKEN}",
		"a setup step's script":              "git config user.name agent-swe-the-seat",
	} {
		if slices.Contains(handed.Values, not) {
			t.Errorf("%s (%q) is kept for the scrub, and is no secret: %q", class, not, handed.Values)
		}
	}
	seen := map[string]bool{}
	for _, v := range handed.Values {
		if strings.TrimSpace(v) == "" || seen[v] {
			t.Errorf("kept %q blank or twice: %q", v, handed.Values)
		}
		seen[v] = true
	}
	if handed.Logins[".claude/.credentials.json"] != login {
		t.Errorf("the login's place is kept as %v, want it read again from %s", handed.Logins, login)
	}
	raw, err := handed.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeHanded(raw)
	if err != nil || !reflect.DeepEqual(back, handed) {
		t.Fatalf("the secrets read back as %+v, %v; want %+v", back, err, handed)
	}
}

// SECRETS PAST WHAT A ROW KEEPS ARE NEVER KEPT IN PART: encoded past
// [MaxHandedBytes] they are refused whole, so the job's captures are refused
// saying so rather than scrubbed of only some of them.
//
// Mutation: cut the values to fit, and a capture is scrubbed of less than the
// box was handed.
func TestSecretsPastWhatARowKeepsAreRefusedWhole(t *testing.T) {
	t.Parallel()
	handed := HandedSecrets{Values: []string{strings.Repeat("k", MaxHandedBytes)}}
	if _, err := handed.encode(); err == nil {
		t.Fatal("secrets past the row's bound were encoded")
	}
}

// awsSessionDocument is an AWS session as a seat's box holds it: the document
// its credential_process prints (internal/aws's CredentialProcess), whose
// strings are its key's id, its secret, its token and its expiry.
func awsSessionDocument(key, secret, token, expiry string) string {
	return `{"Version":1,"AccessKeyId":"` + key + `","SecretAccessKey":"` + secret +
		`","SessionToken":"` + token + `","Expiration":"` + expiry + `"}`
}

// openedSet is turnID's job's set beside its row, opened and read as a capture
// reads it.
func openedSet(t *testing.T, rig *coordRig, keep *sessionKeep, turnID string) HandedSecrets {
	t.Helper()
	raw, err := keep.OpenHanded(turnID, rig.handedOf(t, turnID))
	if err != nil {
		t.Fatalf("opening the job's set: %v", err)
	}
	set, err := decodeHanded(raw)
	if err != nil {
		t.Fatalf("reading the job's set: %v", err)
	}
	return set
}

// AN ISSUED DOCUMENT IS SCRUBBED BY EVERY STRING IT HOLDS, where its launch
// handed it and where the poll renewed it. A seat's AWS session reaches its box
// as the document its credential_process prints, and a transcript prints the
// session's parts bare — `aws configure export-credentials`, a helper's trace —
// which the whole document never matches and no shape names. So the set holds
// the document whole and every string of it, and a capture is handed them all,
// beside what the launch handed.
//
// Mutation: keep a launch's value whole only, or a renewal's, or record a
// renewal in a set the capture does not read, and a check below goes red.
func TestAnIssuedDocumentIsScrubbedByEveryStringItHolds(t *testing.T) {
	t.Parallel()
	launched := awsSessionDocument("ASIALAUNCHEDKEY00001", "the-launched-secret-key",
		"the-launched-session-token", "2026-08-23T12:15:00Z")
	handed := handedSecrets(LaunchRequest{Secrets: []string{launched}}, nil)
	for _, want := range []string{launched, "ASIALAUNCHEDKEY00001", "the-launched-secret-key",
		"the-launched-session-token", "2026-08-23T12:15:00Z"} {
		if !slices.Contains(handed.Values, want) {
			t.Errorf("the session the launch handed is kept without %q: %q", want, handed.Values)
		}
	}

	rig, _, _ := keepingRig(t)
	run := rig.launchKept(t, "t1")
	renewed := awsSessionDocument("ASIARENEWEDKEY000001", "the-renewed-secret-key",
		"the-renewed-session-token", "2026-08-23T12:25:00Z")
	if err := rig.coordinator.RecordHanded(t.Context(), run, renewed, rig.now.Add(25*time.Minute)); err != nil {
		t.Fatalf("recording the renewed session: %v", err)
	}
	scrub, err := rig.coordinator.scrubValues(t.Context(), rig.get("t1"), rig.provider.Box(run.SandboxID))
	if err != nil {
		t.Fatalf("the capture's values: %v", err)
	}
	for _, want := range []string{handedToken, renewed, "ASIARENEWEDKEY000001", "the-renewed-secret-key",
		"the-renewed-session-token"} {
		if !slices.Contains(scrub, want) {
			t.Errorf("a capture after the renewal is not scrubbed of %q: %q", want, scrub)
		}
	}
}

// A RENEWED VALUE IS DROPPED AN HOUR PAST ITS EXPIRY, and nothing a launch
// handed ever is. A lapsed token opens nothing, and dropping it is what keeps a
// long run's renewals — an AWS session's 144 a day — from taking the set past
// its bound; an hour is far past any clock's error, so nothing still good goes.
// A value recorded again lapses at the later of its expiries, and one the
// launch handed keeps none, whatever a renewal says of it. The poll's record
// drops them from the set beside the row as it adds the next.
//
// Mutation: drop at the expiry itself, never drop, drop a value the launch
// handed, keep the earlier of two expiries, or drop only at a launch, and a
// check below goes red.
func TestARenewedValueIsDroppedAnHourPastItsExpiry(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	set := HandedSecrets{Values: []string{"the-launchs-own-token"}}
	set.record("the-first-renewed-token", at)
	set.record("the-second-renewed-token", at)
	set.record("the-second-renewed-token", at.Add(30*time.Minute))
	set.record("the-launchs-own-token", at)
	holds := func(when string, want ...string) {
		t.Helper()
		if !slices.Equal(set.Values, want) {
			t.Errorf("%s the set holds %q, want %q", when, set.Values, want)
		}
	}
	set.drop(at.Add(handedLapse))
	holds("an hour past the first expiry,", "the-launchs-own-token", "the-first-renewed-token",
		"the-second-renewed-token")
	set.drop(at.Add(handedLapse + time.Second))
	holds("a second later,", "the-launchs-own-token", "the-second-renewed-token")
	set.drop(at.Add(30*time.Minute + handedLapse + time.Second))
	holds("an hour past the later expiry,", "the-launchs-own-token")
	if len(set.Expires) != 0 {
		t.Errorf("the set keeps %d expiries of values it no longer holds", len(set.Expires))
	}

	// AND BESIDE THE ROW, as the poll records the next renewal.
	rig, _, keep := keepingRig(t)
	run := rig.launchKept(t, "t1")
	if err := rig.coordinator.RecordHanded(t.Context(), run, "the-first-renewed-token",
		rig.now.Add(15*time.Minute)); err != nil {
		t.Fatalf("the first renewal: %v", err)
	}
	rig.now = rig.now.Add(15*time.Minute + handedLapse + time.Second)
	if err := rig.coordinator.RecordHanded(t.Context(), run, "the-next-renewed-token",
		rig.now.Add(15*time.Minute)); err != nil {
		t.Fatalf("the next renewal: %v", err)
	}
	stored := openedSet(t, rig, keep, "t1")
	if slices.Contains(stored.Values, "the-first-renewed-token") ||
		!slices.Contains(stored.Values, "the-next-renewed-token") || !slices.Contains(stored.Values, handedToken) {
		t.Errorf("the set beside the row holds %q, want the launch's token and the next renewal, "+
			"the first renewal an hour past its expiry dropped", stored.Values)
	}
}

// AN OLDER BUILD'S SCRUB TAKES THE RENEWED VALUES, which live in the one list
// it reads. A node of the build before this one decodes the set as its values
// and its logins alone, and scrubs a capture by the values: a renewal kept
// anywhere else would be in no capture it takes mid-upgrade. When each one
// lapses rides beside them, which it reads past. And a set marked as holding
// less than its box reads to it as a job that keeps none, whose every capture
// it refuses, as this build does: what it reads is the empty seal, not a word
// it does not know.
//
// Mutation: keep a renewal in a field of its own, or mark a set by a flag
// beside the seal it still holds, and a check below goes red.
func TestAnOlderBuildsScrubTakesTheRenewedValues(t *testing.T) {
	t.Parallel()
	// THE OLDER BUILD'S SHAPES, as it decodes the set and the part it sits in.
	type olderSet struct {
		Values []string          `json:"values"`
		Logins map[string]string `json:"logins,omitempty"`
	}
	type olderPart struct {
		Launch    string `json:"launch"`
		LaunchSeq int64  `json:"launch_seq"`
		Sealed    string `json:"sealed"`
	}
	rig, _, keep := keepingRig(t)
	run := rig.launchKept(t, "t1")
	partOf := func() olderPart {
		t.Helper()
		record, found, err := rig.pending.runs.SandboxRunPart(t.Context(), "t1", coord.RunHanded)
		if err != nil || !found {
			t.Fatalf("the part beside the row reads (%v, %v)", found, err)
		}
		var part olderPart
		if err := json.Unmarshal(record.Value, &part); err != nil {
			t.Fatalf("the older build cannot decode the part: %v", err)
		}
		return part
	}
	renewed := awsSessionDocument("ASIARENEWEDKEY000001", "the-renewed-secret-key",
		"the-renewed-session-token", "2026-08-23T12:15:00Z")
	if err := rig.coordinator.RecordHanded(t.Context(), run, renewed, rig.now.Add(15*time.Minute)); err != nil {
		t.Fatalf("recording the renewal: %v", err)
	}
	part := partOf()
	if part.Launch != run.LaunchID || part.Sealed == "" {
		t.Fatalf("the older build reads the part as %+v, want the job's seal", part)
	}
	raw, err := keep.OpenHanded("t1", part.Sealed)
	if err != nil {
		t.Fatalf("opening the set: %v", err)
	}
	var older olderSet
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatalf("the older build cannot decode the set: %v", err)
	}
	for _, want := range []string{handedToken, renewed, "the-renewed-session-token"} {
		if !slices.Contains(older.Values, want) {
			t.Errorf("the older build's scrub takes %q, want %q among them", older.Values, want)
		}
	}

	// A SET MARKED AS HOLDING LESS THAN ITS BOX: the renewal past its bound.
	rig.handOver(t, "t1", HandedSecrets{Values: []string{strings.Repeat("k", MaxHandedBytes-64)}})
	if err := rig.coordinator.RecordHanded(t.Context(), run, renewed, rig.now.Add(15*time.Minute)); err != nil {
		t.Fatalf("recording the renewal past the bound: %v", err)
	}
	if part := partOf(); part.Launch != run.LaunchID || part.Sealed != "" {
		t.Errorf("the older build reads the marked set as %+v, want the job's with no seal, "+
			"which it refuses every capture of", part)
	}
}

// A CAPTURE TAKEN AFTER A RENEWAL HOLDS NO RENEWED VALUE. The poll rewrites a
// running box's token file as its token renews, and the run's transcript can
// print what it read there; the launch never saw that value, and reading it
// again at the capture would issue another. So the waiter hands its refresh a
// recorder bound to the job it polls, which records the token in the set
// beside the job's row before the file is written, and a capture taken after
// it is handed the token to scrub, whole and by every string it holds.
//
// Mutation: hand the refresh a recorder that records nothing, bind it to
// another job, or have the capture read a set the record did not grow, and a
// check below goes red.
func TestACaptureTakenAfterARenewalHoldsNoRenewedValue(t *testing.T) {
	t.Parallel()
	rig, _, _ := keepingRig(t)
	run := rig.launchKept(t, "t1")
	const file = ".aws/crewlet-credentials.json"
	renewed := awsSessionDocument("ASIARENEWEDKEY000001", "the-renewed-secret-key",
		"the-renewed-session-token", "2026-08-23T12:15:00Z")
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers, Drives: rigDrives(Direct),
		Now: func() time.Time { return rig.now },
		Refresh: func(ctx context.Context, _ *Manager, _ PendingRun, box Sandbox, record RecordHanded) {
			if err := record(ctx, renewed, rig.now.Add(15*time.Minute)); err != nil {
				t.Errorf("the poll's record of the renewal: %v", err)
				return
			}
			if err := box.WriteFile(ctx, file, []byte(renewed)); err != nil {
				t.Errorf("writing the renewal: %v", err)
			}
		},
		Record: rig.coordinator.RecordHanded,
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	if got := waiter.pollOne(t.Context(), rig.manager, run); got != pollRunning {
		t.Fatalf("the poll of the running job = %v, want it running", got)
	}
	if held, _ := rig.provider.Box(run.SandboxID).ReadFile(t.Context(), file); string(held) != renewed {
		t.Fatalf("the poll left %q in the token file, want the renewal", held)
	}
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	takes := rig.runner.Takes()
	if len(takes) == 0 {
		t.Fatal("the collection took no session")
	}
	scrub := takes[len(takes)-1].Scrub
	for _, want := range []string{handedToken, renewed, "ASIARENEWEDKEY000001", "the-renewed-secret-key",
		"the-renewed-session-token"} {
		if !slices.Contains(scrub, want) {
			t.Errorf("the capture taken after the renewal is not scrubbed of %q: %q", want, scrub)
		}
	}
}

// A RENEWAL PAST THE SET'S BOUND IS WRITTEN, AND REFUSES EVERY LATER CAPTURE.
// A set still past what a run keeps once its lapsed values are dropped cannot
// hold the token, and one without it would scrub a capture of less than the
// box holds; so it is marked, which every capture of the job reads as a
// refusal saying why, as a launch past the bound is, and the token is written
// all the same: the run needs a good token more than it needs a kept session.
// A later renewal finds the mark and is written too, the set asked nothing.
//
// Mutation: refuse the token past the bound, leave the set as it was, or ask a
// marked set again, and a check below goes red.
func TestARenewalPastTheSetsBoundIsWrittenAndRefusesEveryLaterCapture(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	run := rig.launchKept(t, "t1")
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken, strings.Repeat("k", MaxHandedBytes-128)}})
	if err := rig.coordinator.RecordHanded(t.Context(), run, strings.Repeat("r", 256),
		rig.now.Add(15*time.Minute)); err != nil {
		t.Fatalf("a renewal past the bound was refused, so the run is left without a good token: %v", err)
	}
	if _, err := rig.pending.Handed(t.Context(), rig.get("t1")); !errors.Is(err, ErrHandedIncomplete) {
		t.Fatalf("the set past its bound reads (%v), want it marked as holding less than its box", err)
	}
	if err := rig.coordinator.RecordHanded(t.Context(), run, "the-next-renewed-token",
		rig.now.Add(25*time.Minute)); err != nil {
		t.Errorf("a renewal of a marked set was refused: %v", err)
	}

	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("took %d sessions scrubbed by a set that holds less than its box", len(takes))
	}
	if len(writes.captured()) != 0 {
		t.Fatal("recorded a capture scrubbed by a set that holds less than its box")
	}
	if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused ||
		!strings.Contains(c.Reason, handedOverflowed) {
		t.Fatalf("the record holds %+v, want the capture refused for the renewals past the bound", c)
	}
}

// A TOKEN THE LAUNCH WRITES IN A KEPT BOX IS SEALED WITH ITS JOB. A job that
// continues in its turn's kept box has the box's token files brought up to
// date before it starts, which can write a token no poll recorded — the box was
// paused, and no poll reached it — and the job's own set is written only with
// its command, once the job has started. So the launch keeps what its refresh
// wrote and seals it with the job, its lapse beside it, with the job's own
// secrets and those carried from the job before it in the box.
//
// Mutation: hand the launch's refresh a recorder that keeps nothing, or seal
// the job without what it kept, and a check below goes red.
func TestATokenTheLaunchWritesInAKeptBoxIsSealedWithItsJob(t *testing.T) {
	t.Parallel()
	const renewed = "the-token-renewed-at-the-launch"
	expires := time.Date(2026, 8, 23, 12, 15, 0, 0, time.UTC)
	rig, _, keep := keepingRigWith(t, func(o *CoordinatorOptions) {
		o.Refresh = func(ctx context.Context, _ *Manager, _ PendingRun, box Sandbox, record RecordHanded) {
			if err := record(ctx, renewed, expires); err != nil {
				t.Errorf("the launch's record of the renewal: %v", err)
				return
			}
			if err := box.WriteFile(ctx, ".config/crewlet/github-token", []byte(renewed)); err != nil {
				t.Errorf("writing the renewal: %v", err)
			}
		}
	})
	run := rig.launch("t1")
	rig.handOver(t, "t1", HandedSecrets{Values: []string{"the-first-jobs-token"}})
	var relaunch LaunchResult
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		req := launchReq(r.TurnID)
		req.Secrets = []string{"the-relaunchs-own-token"}
		res, err := rig.coordinator.Launch(ctx, rig.manager, req)
		if err != nil {
			t.Errorf("relaunch: %v", err)
		}
		relaunch = res
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	rig.awaitKeeps()
	if !relaunch.Reused || relaunch.SandboxID != run.SandboxID {
		t.Fatalf("relaunch = %+v, want it continued in the turn's box %q", relaunch, run.SandboxID)
	}
	set := openedSet(t, rig, keep, "t1")
	for _, want := range []string{renewed, "the-first-jobs-token", "the-relaunchs-own-token"} {
		if !slices.Contains(set.Values, want) {
			t.Errorf("the relaunch's set holds %q, want %q among them", set.Values, want)
		}
	}
	if at, lapses := set.Expires[expiryKey(renewed)]; !lapses || !at.Equal(expires) {
		t.Errorf("the token the launch wrote lapses at %v (%v), want at its issuer's %v", at, lapses, expires)
	}
	if _, lapses := set.Expires[expiryKey("the-relaunchs-own-token")]; lapses {
		t.Error("a secret the launch handed was given a lapse")
	}
}
