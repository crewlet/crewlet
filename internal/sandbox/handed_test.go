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
	"github.com/crewlet/crewlet/internal/objstore"
)

// EVERY SECRET A LAUNCH HANDS ITS BOX IS KEPT FOR ITS SESSION'S SCRUB, AND
// NOTHING ELSE IS: credential positions ([LaunchRequest.Secrets]), MCP servers'
// env, headers and resolved urls, the run's own export token, every string of
// the seeded login and where it lives. Not a seat's handle, email, setting,
// telemetry protocol, unresolved reference or setup step's file: scrubbed by
// value, each blanks transcripts. A JSON secret goes whole and by every
// string, settings too, as only names tell them apart (handed.go), but for a
// service-account key's constants. None blank or twice; the seal keeps it all.
//
// Mutation: leave a class out of handedSecrets, add the run's environment or
// setup steps back whole, read the login only at capture, keep a JSON secret
// whole alone or a blank value, and a check below goes red.
func TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse(t *testing.T) {
	t.Parallel()
	login := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"login-access-value",`+
		`"refreshToken":"login-refresh-value","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const keyDocument = `{"type":"service_account","project_id":"a-project-of-the-seats",` +
		`"private_key":"a-private-key-of-the-seats"}`
	req := LaunchRequest{
		Spec: Spec{CredentialFiles: map[string]string{".claude/.credentials.json": login}},
		Secrets: []string{"resolved-env-token", "credential-named-token", "  ", "resolved-env-token",
			keyDocument},
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
	}
	handed := handedSecrets(req, env)
	for class, want := range map[string]string{
		"a value the engine resolved from a reference": "resolved-env-token",
		"a variable named for a credential":            "credential-named-token",
		"an MCP server's header":                       "Bearer mcp-header-value",
		"an MCP server's env":                          "mcp-env-value",
		"what an MCP server's url's reference held":    "url-path-secret-value",
		"the run's own export token":                   "the-runs-export-token",
		"the seeded login's access token":              "login-access-value",
		"the seeded login's refresh token":             "login-refresh-value",
		"a JSON-valued secret, whole":                  keyDocument,
		"the credential a JSON-valued secret holds":    "a-private-key-of-the-seats",
		"a setting a JSON-valued secret holds":         "a-project-of-the-seats",
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

// A SERVICE-ACCOUNT KEY'S PUBLISHED CONSTANTS STAY IN ITS SESSIONS, and every
// other string of it goes. Google's format fixes a key's `type`, `auth_uri`,
// `token_uri`, `auth_provider_x509_cert_url` and `universe_domain`, the same in
// every key, so none is a secret, and scrubbed they would blank every
// googleapis.com URL from a seat's transcripts. The account's own members go,
// its client certificate's URL among them since it names the client's email,
// whether the launch handed the key or seeded it as the box's login; a document
// of any other format keeps no constant, even under the same names.
//
// Mutation: drop the format check, and the other format's `type` and URLs are
// kept; exempt a member of the account's, and its value is kept; walk the
// constants too, and they go.
func TestAServiceAccountKeysPublishedConstantsStayInItsSessions(t *testing.T) {
	t.Parallel()
	constants := map[string]string{
		"type":                        "service_account",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
		"universe_domain":             "googleapis.com",
	}
	account := map[string]string{
		"project_id":     "nimbus-prod-4711",
		"private_key_id": "0123456789abcdef0123456789abcdef01234567",
		"private_key":    "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----\n",
		"client_email":   "agent-swe@nimbus-prod-4711.iam.gserviceaccount.com",
		"client_id":      "103456789012345678901",
		"client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/" +
			"agent-swe%40nimbus-prod-4711.iam.gserviceaccount.com",
	}
	document := func(members ...map[string]string) string {
		t.Helper()
		whole := map[string]string{}
		for _, m := range members {
			for name, value := range m {
				whole[name] = value
			}
		}
		raw, err := json.Marshal(whole)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	key := document(constants, account)
	for how, values := range map[string][]string{
		"handed by the launch":      handedSecrets(LaunchRequest{Secrets: []string{key}}, nil).Values,
		"seeded as the box's login": loginValues([]byte(key)),
	} {
		for member, value := range account {
			if !slices.Contains(values, value) {
				t.Errorf("a service-account key %s keeps its %s (%q) in its sessions: %q", how, member, value, values)
			}
		}
		for member, value := range constants {
			if slices.Contains(values, value) {
				t.Errorf("a service-account key %s takes its format's %s (%q) out of its sessions: %q",
					how, member, value, values)
			}
		}
	}

	other := map[string]string{
		"type":            "vendor_service_key",
		"token_uri":       "https://login.vendor.example/oauth/token",
		"universe_domain": "vendor.example.com",
		"client_email":    "agent-swe@vendor.example.com",
	}
	values := handedSecrets(LaunchRequest{Secrets: []string{document(other)}}, nil).Values
	for member, value := range other {
		if !slices.Contains(values, value) {
			t.Errorf("a document of another format keeps its %s (%q) in its sessions: %q", member, value, values)
		}
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
// as the document its credential_process prints, and a transcript prints its
// parts bare (`aws configure export-credentials`, a helper's trace), which the
// whole document never matches; so the set holds it whole and by every string,
// and a capture is handed them all beside what the launch handed.
//
// Mutation: keep a launch's value whole only, or a renewal's, or record a
// renewal in a set the capture does not read, and a check below goes red.
func TestAnIssuedDocumentIsScrubbedByEveryStringItHolds(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
	})
}

// A RENEWED VALUE STAYS UNTIL ITS ISSUER'S CLOCK IS PAST ITS EXPIRY too: the
// expiry is the issuer's instant, the drop is judged on the node's clock, and
// the two may stand twice objstore.AssumedClockSkew apart with no clock_skew
// raised. An issuer behind the node by that much still honours the token until
// the node reads its expiry plus the whole of it, so the token is kept until
// then and dropped just after.
//
// Mutation: a lapse of an hour, or anything under twice the skew, drops the
// still-good token and the first check goes red; never dropping fails the
// second.
func TestARenewedValueStaysUntilItsIssuerCountsItLapsed(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	behind := 2 * objstore.AssumedClockSkew
	set := HandedSecrets{}
	set.record("a-token-an-issuer-behind-renewed", expires)
	// The node reads expires+behind as the issuer reads expires: the token's
	// last good instant.
	set.drop(expires.Add(behind))
	if !slices.Contains(set.Values, "a-token-an-issuer-behind-renewed") {
		t.Fatalf("at %v by the node's clock, the issuer %v behind it still honours the token, but the set "+
			"dropped it", expires.Add(behind), behind)
	}
	set.drop(expires.Add(behind + time.Second))
	if len(set.Values) != 0 {
		t.Errorf("a second past the issuer's own expiry the set still holds %q", set.Values)
	}
}

// A RENEWED VALUE IS DROPPED THE LAPSE PAST ITS EXPIRY, and nothing a launch
// handed ever is. Dropping lapsed tokens keeps a long run's renewals (an AWS
// session's 144 a day) within the set's bound. A value recorded twice, or held
// by both sets a launch joins, lapses at the later expiry, and one either holds
// for good never lapses. The poll's record drops lapsed values as it adds the
// next, and a launch continuing in the box as it seals the set it carried.
//
// Mutation: drop at the expiry itself, never drop, drop a value the launch
// handed, keep the earlier of two expiries (in a record or in a union), give a
// value held for good a lapse, drop only at a launch, or seal a carried set
// without dropping, and a check below goes red.
func TestARenewedValueIsDroppedTheLapsePastItsExpiry(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
		holds("the lapse past the first expiry,", "the-launchs-own-token", "the-first-renewed-token",
			"the-second-renewed-token")
		set.drop(at.Add(handedLapse + time.Second))
		holds("a second later,", "the-launchs-own-token", "the-second-renewed-token")
		set.drop(at.Add(30*time.Minute + handedLapse + time.Second))
		holds("the lapse past the later expiry,", "the-launchs-own-token")
		if len(set.Expires) != 0 {
			t.Errorf("the set keeps %d expiries of values it no longer holds", len(set.Expires))
		}

		// AND AS TWO SETS ARE JOINED, either one carrying the other.
		var sooner, later, forGood HandedSecrets
		sooner.record("a-renewal-both-hold", at)
		later.record("a-renewal-both-hold", at.Add(30*time.Minute))
		forGood.Values = []string{"a-renewal-both-hold"}
		for name, joined := range map[string]HandedSecrets{
			"carrying the later": sooner.union(later), "carried by the later": later.union(sooner),
		} {
			if got := joined.Expires[expiryKey("a-renewal-both-hold")]; !got.Equal(at.Add(30 * time.Minute)) {
				t.Errorf("a value both sets hold, %s, lapses at %v, want the later expiry", name, got)
			}
		}
		for name, joined := range map[string]HandedSecrets{
			"carrying it": sooner.union(forGood), "carried by it": forGood.union(sooner),
		} {
			if when, lapses := joined.Expires[expiryKey("a-renewal-both-hold")]; lapses {
				t.Errorf("a value a set holds for good, %s, lapses at %v", name, when)
			}
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
				"the first renewal the lapse past its expiry dropped", stored.Values)
		}

		// AND AS A KEPT BOX'S NEXT JOB IS SEALED, with what the job before it
		// handed the box carried forward.
		relaunchRig, _, relaunchKeep := keepingRig(t)
		firstJob := relaunchRig.launch("t1")
		carried := HandedSecrets{Values: []string{"the-first-jobs-token"}}
		carried.record("a-renewal-long-lapsed", relaunchRig.now.Add(-handedLapse-time.Minute))
		carried.record("a-renewal-still-good", relaunchRig.now.Add(10*time.Minute))
		relaunchRig.handOver(t, "t1", carried)
		var relaunch LaunchResult
		relaunchRig.resumer.during = func(ctx context.Context, r PendingRun) {
			res, err := relaunchRig.coordinator.Launch(ctx, relaunchRig.manager, launchReq(r.TurnID))
			if err != nil {
				t.Errorf("relaunch: %v", err)
			}
			relaunch = res
		}
		relaunchRig.runner.Finish(Result{Success: true, Text: "first pass"})
		payload, ev := relaunchRig.completion("t1")
		if err := relaunchRig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		relaunchRig.awaitKeeps()
		if !relaunch.Reused || relaunch.SandboxID != firstJob.SandboxID {
			t.Fatalf("relaunch = %+v, want it continued in the turn's box %q", relaunch, firstJob.SandboxID)
		}
		sealed := openedSet(t, relaunchRig, relaunchKeep, "t1")
		if slices.Contains(sealed.Values, "a-renewal-long-lapsed") ||
			!slices.Contains(sealed.Values, "a-renewal-still-good") || !slices.Contains(sealed.Values, "the-first-jobs-token") {
			t.Errorf("the relaunch's set holds %q, want what the job before it handed and its renewal still good, "+
				"the one the lapse past its expiry dropped", sealed.Values)
		}
		if when, lapses := sealed.Expires[expiryKey("a-renewal-still-good")]; !lapses ||
			!when.Equal(relaunchRig.now.Add(10*time.Minute)) {
			t.Errorf("the renewal carried into the relaunch's set lapses at %v (%v), want its own expiry", when, lapses)
		}
	})
}

// AN OLDER BUILD'S SCRUB TAKES THE RENEWED VALUES, which live in the one list it
// reads: the build before decodes the set as its values and logins alone, so a
// renewal kept elsewhere would be in no capture it takes mid-upgrade; expiries
// ride beside, which it reads past. A set marked as holding less than its box
// reads to it as the empty seal, a job that keeps none, whose every capture it
// refuses as this build does.
//
// Mutation: keep a renewal in a field of its own, or mark a set by a flag
// beside the seal it still holds, and a check below goes red.
func TestAnOlderBuildsScrubTakesTheRenewedValues(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
	})
}

// A CAPTURE TAKEN AFTER A RENEWAL HOLDS NO RENEWED VALUE. The poll rewrites a
// running box's token file as it renews, and a transcript can print it; the
// launch never saw it, and reading it again at the capture would issue another.
// So the waiter hands its refresh a recorder bound to the polled job, which
// records the token in the set beside its row before the file is written, and a
// later capture scrubs it whole and by every string.
//
// Mutation: hand the refresh a recorder that records nothing, bind it to
// another job, or have the capture read a set the record did not grow, and a
// check below goes red.
func TestACaptureTakenAfterARenewalHoldsNoRenewedValue(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		const file = ".aws/crewlet-credentials.json"
		renewed := awsSessionDocument("ASIARENEWEDKEY000001", "the-renewed-secret-key",
			"the-renewed-session-token", "2026-08-23T12:15:00Z")
		waiter, err := NewWaiter(WaiterOptions{
			Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
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
	})
}

// A RENEWAL THE STORE DID NOT ANSWER IS KEPT OUT OF THE BOX until a poll's
// record lands. The refresh writes a token only on a nil record, so a record
// the store did not take answers its error: nil would put a live token in the
// box that no capture is scrubbed of, and [ErrJobNotRunning] would leave a
// running job on an expiring token. The next poll asks again.
//
// Mutation: answer a record the store refused as one the refresh may write, or
// as a job no longer running, and a check below goes red.
func TestARenewalTheStoreDidNotAnswerIsKeptOutOfTheBox(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		rig, _, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		const file, renewed = ".config/crewlet/github-token", "the-token-the-poll-renewed"
		box := rig.provider.Box(run.SandboxID)
		box.Put(file, "the-token-the-launch-wrote")
		var refused []error
		waiter, err := NewWaiter(WaiterOptions{
			Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
			Now: func() time.Time { return rig.now },
			Refresh: func(ctx context.Context, _ *Manager, _ PendingRun, box Sandbox, record RecordHanded) {
				if err := record(ctx, renewed, rig.now.Add(15*time.Minute)); err != nil {
					refused = append(refused, err)
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

		store := &refusingStore{inner: rig.coordinator.pending, refuse: refusals("AddHanded")}
		rig.coordinator.pending = store
		if got := waiter.pollOne(ctx, rig.manager, run); got != pollRunning {
			t.Fatalf("the poll of the running job = %v, want it running", got)
		}
		if len(refused) != 1 || errors.Is(refused[0], ErrJobNotRunning) {
			t.Fatalf("the record the store did not answer was answered %v, want the store's error", refused)
		}
		if held, _ := box.ReadFile(ctx, file); string(held) != "the-token-the-launch-wrote" {
			t.Fatalf("a renewal the store did not record was written: the file holds %q", held)
		}
		if slices.Contains(openedSet(t, rig, keep, "t1").Values, renewed) {
			t.Fatal("the set holds a renewal the store refused to record")
		}

		rig.coordinator.pending = store.inner
		if got := waiter.pollOne(ctx, rig.manager, run); got != pollRunning {
			t.Fatalf("the next poll of the running job = %v, want it running", got)
		}
		if held, _ := box.ReadFile(ctx, file); string(held) != renewed {
			t.Errorf("the poll whose record landed left %q in the file, want the renewal", held)
		}
		if !slices.Contains(openedSet(t, rig, keep, "t1").Values, renewed) {
			t.Error("the poll wrote a renewal its job's set does not hold")
		}
	})
}

// A LATER BUILD'S KEYS IN THE HANDED SET SURVIVE THIS BUILD'S RENEWALS, in the
// part beside the row and in the set it seals. Every renewal, on whichever node
// polls the box, decodes both into this build's shapes and writes them back,
// and a launch continuing in the box joins the set it carries with its own; a
// later build's key dropped there is gone from the fleet.
//
// Mutation: drop either section's Extra on its decode or its encode, or leave
// the carried set's out of the union, and a check below goes red.
func TestALaterBuildsKeysInTheHandedSetSurviveARenewal(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		future := json.RawMessage(`{"a":"later build"}`)
		rig, _, keep := keepingRig(t)
		run := rig.launch("t1")
		// THE SET AND ITS PART AS A LATER BUILD WROTE THEM, a key of its own in each.
		set, err := json.Marshal(map[string]any{"values": []string{handedToken}, "set_later": future})
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := keep.SealHanded("t1", set)
		if err != nil {
			t.Fatal(err)
		}
		part, err := json.Marshal(map[string]any{"launch": run.LaunchID, "launch_seq": run.LaunchSeq,
			"sealed": sealed, "part_later": future})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := rig.pending.runs.MutateSandboxRunPart(ctx, "t1", coord.RunHanded,
			func(coord.Record, bool) ([]byte, error) { return part, nil }); err != nil {
			t.Fatalf("writing the later build's part: %v", err)
		}

		if err := rig.coordinator.RecordHanded(ctx, run, "a-token-the-poll-renewed",
			rig.now.Add(15*time.Minute)); err != nil {
			t.Fatalf("recording the renewal: %v", err)
		}
		record, found, err := rig.pending.runs.SandboxRunPart(ctx, "t1", coord.RunHanded)
		if err != nil || !found {
			t.Fatalf("the part beside the row reads (%v, %v)", found, err)
		}
		var written map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &written); err != nil {
			t.Fatalf("the part does not decode: %v", err)
		}
		if string(written["part_later"]) != string(future) {
			t.Errorf("a later build's key in the part did not survive the renewal: %s", record.Value)
		}
		grown := openedSet(t, rig, keep, "t1")
		if !slices.Contains(grown.Values, "a-token-the-poll-renewed") {
			t.Fatalf("the renewal was not recorded, so nothing was written back: %q", grown.Values)
		}
		if string(grown.Extra["set_later"]) != string(future) {
			t.Errorf("a later build's key in the set did not survive the renewal: %v", grown.Extra)
		}

		// AND AS A LAUNCH CARRIES THE SET INTO THE BOX'S NEXT JOB.
		joined := HandedSecrets{Values: []string{"the-next-jobs-token"}}.union(grown)
		if string(joined.Extra["set_later"]) != string(future) {
			t.Errorf("a later build's key in the carried set did not survive the carry: %v", joined.Extra)
		}
	})
}

// A RENEWAL PAST THE SET'S BOUND IS WRITTEN, AND REFUSES EVERY LATER CAPTURE. A
// set still past its bound once lapsed values are dropped cannot hold the token,
// and one without it would scrub a capture of less than the box holds, so it is
// marked, which every capture reads as a refusal saying why; the token is
// written anyway, since the run needs a good token more than a kept session.
// The marking node logs it once (`executor_session_scrub_incomplete`, with
// why); a later renewal is written, the set unasked and nothing logged again.
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: refuse the token past the bound, leave the set as it was, ask a
// marked set again, or say the mark under another name or not at all, and a
// check below goes red.
func TestARenewalPastTheSetsBoundIsWrittenAndRefusesEveryLaterCapture(t *testing.T) {
	inBubble(t, func(t *testing.T) {
		logs := captureLogs(t)
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
		said := logs.String()
		if n := strings.Count(said, "executor_session_scrub_incomplete"); n != 1 || !strings.Contains(said, handedOverflowed) {
			t.Errorf("the mark was said %d times, want once, naming the bound it passed: %q", n, said)
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
	})
}

// A TOKEN THE LAUNCH WRITES IN A KEPT BOX IS SEALED WITH ITS JOB. A job
// continuing in its turn's kept box has the box's token files refreshed before
// it starts, which can write a token no poll recorded (the box was paused), and
// the job's own set is written only once it has started. So the launch keeps
// what its refresh wrote and seals it, its lapse beside it, with the job's own
// secrets and those carried from the job before it.
//
// Mutation: hand the launch's refresh a recorder that keeps nothing, or seal
// the job without what it kept, and a check below goes red.
func TestATokenTheLaunchWritesInAKeptBoxIsSealedWithItsJob(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
	})
}

// THE NAME OF AN MCP SERVER THE LAUNCH DECLARES IS NEVER TAKEN OUT BY VALUE,
// whatever credential position also holds it: every tool name of that server
// carries it, and scrubbed from a kept session it leaves the tool definitions
// the harness restores named outside the provider's pattern.
//
// Mutation: collect the values without withoutServerNames, and it goes red.
func TestAServerNameALaunchDeclaresIsNeverTakenOutByValue(t *testing.T) {
	t.Parallel()
	req := LaunchRequest{
		Secrets: []string{"cloudhost", `{"profile":"cloudhost","secret":"a-real-secret-value"}`},
		MCPServers: map[string]MCPServer{
			"cloudhost": {Name: "cloudhost", Transport: TransportStdio, Command: "cloud",
				Env: map[string]string{"CLOUD_PROFILE": "cloudhost", "CLOUD_KEY": "an-env-secret-value"}},
		},
	}
	handed := handedSecrets(req, nil)
	if slices.Contains(handed.Values, "cloudhost") {
		t.Errorf("the handed set %q holds the server's own name, which every tool name of it carries",
			handed.Values)
	}
	for _, want := range []string{"a-real-secret-value", "an-env-secret-value"} {
		if !slices.Contains(handed.Values, want) {
			t.Errorf("the handed set %q lost %q with the server's name", handed.Values, want)
		}
	}
}

// A LAUNCH'S GITLET CREDENTIAL IS WRITTEN INTO ITS BOX AND SEALED WITH ITS JOB.
// In no variable, it is in nothing else a capture's scrub reads, so the
// launch writes it to the box's file before the job starts and keeps it in
// the job's set beside its row, where a transcript that prints the file is
// scrubbed of it.
//
// Mutations, each red: write no file; keep the credential out of the job's
// set.
func TestALaunchsGitletCredentialIsWrittenIntoItsBoxAndSealedWithItsJob(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		const access = "the-launchs-gitlet-credential"
		rig, _, keep := keepingRigWith(t, nil)
		req := launchReq("t1")
		req.RunEnv = func(string, EngineHost) (LaunchEnv, error) { return LaunchEnv{GitAccess: access}, nil }
		res, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if got, err := rig.provider.Box(res.SandboxID).ReadFile(t.Context(), GitAccessFile); err != nil ||
			string(got) != access {
			t.Errorf("the box's credential file holds %q (%v), want the launch's", got, err)
		}
		if set := openedSet(t, rig, keep, "t1"); !slices.Contains(set.Values, access) {
			t.Errorf("the job's set holds %q, want the launch's Gitlet credential among them", set.Values)
		}
	})
}
