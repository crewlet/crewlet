package mcpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/sandbox"
)

func reviewDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// listedAs is tracker's tool raw listed under the definition named by seed.
func listedAs(raw, seed string) coord.MCPListedTool {
	hints := coord.MCPToolHints{ReadOnly: "yes", Destructive: "no", Idempotent: "no", OpenWorld: "no"}
	part := reviewDigest(seed)[:coord.MCPToolPartLength]
	return coord.MCPListedTool{Name: "tr_" + raw, Raw: raw, Description: "does " + raw + " " + seed,
		Hints: hints, Declared: hints, Digest: reviewDigest(seed), Instances: 1,
		Parts: coord.MCPToolParts{Title: part, Description: part, Schema: part, Hints: part}}
}

// fakeReviews is tracker's review: close_issue accepted at v1 and listed at
// v2, export added since the first listing, purge accepted and no longer
// listed. It records what is written, refusing a version that moved.
type fakeReviews struct {
	mu      sync.Mutex
	review  queries.MCPReview
	written []coord.MCPAcceptedTool
	moved   map[string]bool
	// failing is the tools whose write the store fails.
	failing map[string]bool
}

func trackerReview() *fakeReviews {
	entry := &config.MCPServer{Name: "tracker", Command: "tracker-mcp", ToolPrefix: "tr_"}
	accepted := map[string]coord.MCPAcceptedTool{}
	for i, a := range mcpreview.Baseline(coord.MCPListing{Server: "tracker",
		Tools: []coord.MCPListedTool{listedAs("close_issue", "v1"), listedAs("purge", "p1")}}, time.Now()) {
		a.Version = uint64(10 + i)
		accepted[a.Tool] = a
	}
	listings := []coord.MCPListing{{Server: "tracker", Node: "node-a", Epoch: 1, Instances: 1, Tools: []coord.MCPListedTool{listedAs("close_issue", "v2"), listedAs("export", "e1")}}}
	return &fakeReviews{review: queries.MCPReview{Server: "tracker", Listings: listings, Accepted: accepted,
		Changes: mcpreview.Changes(listings, accepted), Entry: entry}, moved: map[string]bool{}}
}

func (f *fakeReviews) MCPReview(_ context.Context, server string) (queries.MCPReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if server != "tracker" {
		return queries.MCPReview{Server: server, Accepted: map[string]coord.MCPAcceptedTool{}}, nil
	}
	return f.review, nil
}

func (f *fakeReviews) PutMCPAccepted(_ context.Context, a coord.MCPAcceptedTool, version uint64) (coord.MCPAcceptedTool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moved[a.Tool] {
		return coord.MCPAcceptedTool{}, false, nil
	}
	if f.failing[a.Tool] {
		return coord.MCPAcceptedTool{}, false, errors.New("the coordination store did not answer")
	}
	if held, ok := f.review.Accepted[a.Tool]; ok != (version != 0) || (ok && held.Version != version) {
		return coord.MCPAcceptedTool{}, false, nil
	}
	if err := a.Validate(); err != nil {
		return coord.MCPAcceptedTool{}, false, err
	}
	f.written = append(f.written, a)
	return a, true, nil
}

// reviewSurface is the surface over reviews and docs.
func reviewSurface(t *testing.T, reviews *fakeReviews, docs *fakeToolDocuments, audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	opts := mcpapi.Options{Company: func() *config.Company { return nil },
		Registry: &fakeRegistry{}, Detail: listedDetail(), Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: docs, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: audit, Guard: authz.ContextGuard(authz.NoChart{})}
	if reviews != nil {
		opts.Reviews = reviews
	}
	svc, err := mcpapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// A CHANGE IS ACCEPTED OR KEPT BLOCKED BY THE DIGEST REVIEWED, as a
// configuration write at a recent step-up. Accepting writes the definition
// listed now as the tool's accepted one, by who accepted it; blocking sets the
// tool's level to blocked as one revision and accepts the definition the
// decision was taken on; a removal is accepted by null. Audited by the tools'
// names, and answered with exactly the keys the API reference names.
//
// Mutation: accept by name without the digest, write a block without the
// level, or accept a removal by deleting its record, and this is red.
func TestAChangeIsAcceptedOrKeptBlockedByTheDefinitionReviewed(t *testing.T) {
	t.Parallel()
	reviews, docs, audit := trackerReview(), &fakeToolDocuments{}, &recordedAudit{}
	svc := reviewSurface(t, reviews, docs, audit)
	body := `{"accept": {"close_issue": "` + reviewDigest("v2") + `", "purge": null}, ` +
		`"block": {"export": "` + reviewDigest("e1") + `"}}`
	code, got := answer(t, svc, proved(time.Minute), "/mcp-servers/tracker/changes", body)
	if code != http.StatusOK {
		t.Fatalf("POST = %d %v", code, got)
	}
	if keys, want := keysOf(got), []string{"accepted", "blocked", "moved", "revision", "server"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("the answer's keys are %v, want exactly %v", keys, want)
	}
	if fmtList(got["accepted"]) != "close_issue,purge" || fmtList(got["blocked"]) != "export" {
		t.Errorf("answered accepted %v and blocked %v", got["accepted"], got["blocked"])
	}
	write := docs.last(t)
	if level := write.Change.Levels["export"]; level == nil || *level != config.MCPToolBlocked || len(write.Change.Levels) != 1 {
		t.Errorf("the block wrote %+v, want export blocked alone", write.Change.Levels)
	}
	byTool := map[string]coord.MCPAcceptedTool{}
	for _, a := range reviews.written {
		byTool[a.Tool] = a
	}
	if a := byTool["close_issue"]; a.Digest != reviewDigest("v2") || a.By != "jane.doe" || a.First {
		t.Errorf("close_issue was accepted as %+v, want v2 by jane.doe", a)
	}
	if a := byTool["purge"]; !a.Removed || a.Digest != "" {
		t.Errorf("purge's removal was accepted as %+v", a)
	}
	if a := byTool["export"]; a.Digest != reviewDigest("e1") {
		t.Errorf("export's blocked definition was accepted as %+v, want e1", a)
	}
	rows := audit.rows
	if len(rows) != 1 || rows[0].Action != "review_changes" || strings.Join(rows[0].Accepted, ",") != "close_issue,purge" ||
		strings.Join(rows[0].Blocked, ",") != "export" {
		t.Errorf("audited %+v", rows)
	}
}

// A SERVER CANNOT MAKE A CHANGE LOOK ACCEPTED: a decision naming a definition
// the fleet does not list for the tool now (the server swapped it between the
// review and the answer) is refused, naming the tool, and nothing is written,
// the other decisions in the body included; a decision somebody else wrote
// between the read and the write is answered moved.
//
// Mutation: accept the definition listed now whatever the body names, and
// this is red.
func TestAServerCannotMakeAChangeLookAccepted(t *testing.T) {
	t.Parallel()
	reviews, docs := trackerReview(), &fakeToolDocuments{}
	svc := reviewSurface(t, reviews, docs, &recordedAudit{})
	body := `{"accept": {"close_issue": "` + reviewDigest("v3") + `", "export": "` + reviewDigest("e1") + `"}}`
	code, got := answer(t, svc, proved(time.Minute), "/mcp-servers/tracker/changes", body)
	if code != http.StatusConflict || got["error"] != "stale_version" || fmtList(got["tools"]) != "close_issue" {
		t.Fatalf("POST = %d %v, want 409 naming close_issue", code, got)
	}
	if len(reviews.written) != 0 || len(docs.writes) != 0 {
		t.Fatalf("a refused review wrote %+v and %+v", reviews.written, docs.writes)
	}
	// AN ACCEPTED TOOL HAS NO CHANGE TO ACCEPT, and a removal is not
	// accepted for a tool still listed.
	code, _ = answer(t, svc, proved(time.Minute), "/mcp-servers/tracker/changes",
		`{"accept": {"close_issue": null}}`)
	if code != http.StatusConflict {
		t.Errorf("a removal named for a listed tool answered %d", code)
	}
	reviews.moved["export"] = true
	code, got = answer(t, svc, proved(time.Minute), "/mcp-servers/tracker/changes",
		`{"accept": {"export": "`+reviewDigest("e1")+`"}}`)
	if code != http.StatusOK || fmtList(got["moved"]) != "export" || fmtList(got["accepted"]) != "" {
		t.Errorf("a decision another writer moved answered %d %v, want it moved", code, got)
	}
}

// A REVIEW IS A CONFIGURATION WRITE: refused without a recent step-up, to a
// member, for a server the company does not declare, for a body that decides
// nothing or decides one tool twice, and on a node with nothing to read the
// changes from.
func TestAReviewIsAConfigurationWriteAtARecentStepUp(t *testing.T) {
	t.Parallel()
	svc := reviewSurface(t, trackerReview(), &fakeToolDocuments{}, &recordedAudit{})
	accept := `{"accept": {"export": "` + reviewDigest("e1") + `"}}`
	for _, c := range []struct {
		name, path, body string
		code             int
	}{
		{"a stale step-up", "/mcp-servers/tracker/changes", accept, http.StatusForbidden},
		{"a server nobody declares", "/mcp-servers/ghost/changes", accept, http.StatusNotFound},
		{"nothing decided", "/mcp-servers/tracker/changes", `{}`, http.StatusBadRequest},
		{"one tool twice", "/mcp-servers/tracker/changes",
			`{"accept": {"export": null}, "block": {"export": null}}`, http.StatusBadRequest},
	} {
		as := proved(time.Minute)
		if c.name == "a stale step-up" {
			as = proved(3 * time.Hour)
		}
		if code, body := answer(t, svc, as, c.path, c.body); code != c.code {
			t.Errorf("%s: POST = %d %v, want %d", c.name, code, body, c.code)
		}
	}
	if code, _ := answer(t, svc, seated("swe", time.Minute), "/mcp-servers/tracker/changes", accept); code != http.StatusForbidden {
		t.Errorf("a member's review answered %d, want 403", code)
	}
	bare := reviewSurface(t, nil, &fakeToolDocuments{}, &recordedAudit{})
	if code, _ := answer(t, bare, proved(time.Minute), "/mcp-servers/tracker/changes", accept); code != http.StatusServiceUnavailable {
		t.Errorf("a node with no reviews answered %d, want 503", code)
	}
}

// ALWAYS ALLOW ACCEPTS THE DEFINITION THE CALL WAS MADE UNDER: the level is
// decided on it, so a tool held only because it changed does not ask again.
//
// Mutation: drop acceptHeld from allowAlways, and this is red.
func TestAlwaysAllowAcceptsTheDefinitionTheCallWasMadeUnder(t *testing.T) {
	t.Parallel()
	reviews, docs := trackerReview(), &fakeToolDocuments{}
	run := waitingRun()
	run.HeldCall.Raw, run.HeldCall.Digest = "close_issue", reviewDigest("v2")
	opts := mcpapi.Options{Company: func() *config.Company { return nil },
		Registry: &fakeRegistry{}, Detail: listedDetail(), Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: docs, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: &recordedAudit{}, Approvals: &fakeApprovals{run: run},
		Reviews: reviews, Guard: authz.ContextGuard(leadOf{lead: "cto", seat: "swe"})}
	svc, err := mcpapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	code, body := answer(t, svc, proved(time.Minute), "/mcp-approvals/"+heldID, `{"answer": "`+sandbox.HeldAlways+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d %v", code, body)
	}
	if len(reviews.written) != 1 || reviews.written[0].Tool != "close_issue" ||
		reviews.written[0].Digest != reviewDigest("v2") {
		t.Fatalf("always accepted %+v, want close_issue at the definition the call was made under", reviews.written)
	}
}

// A DECISION WRITTEN IN PART SAYS WHICH: decisions are written one by one, and
// one the store fails answers 503 naming what was decided and what was not,
// so the dashboard can say exactly that rather than "nothing was changed".
//
// Mutation: answer the failure without naming the decided and the undecided,
// and this is red.
func TestADecisionWrittenInPartSaysWhich(t *testing.T) {
	t.Parallel()
	reviews := trackerReview()
	reviews.failing = map[string]bool{"export": true}
	svc := reviewSurface(t, reviews, &fakeToolDocuments{}, &recordedAudit{})
	body := `{"accept": {"close_issue": "` + reviewDigest("v2") + `", "export": "` + reviewDigest("e1") +
		`", "purge": null}}`
	code, got := answer(t, svc, proved(time.Minute), "/mcp-servers/tracker/changes", body)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("POST = %d %v, want 503", code, got)
	}
	if fmtList(got["accepted"]) != "close_issue" || fmtList(got["undecided"]) != "export,purge" ||
		fmtList(got["blocked"]) != "" || fmtList(got["moved"]) != "" {
		t.Errorf("answered accepted %v, undecided %v, blocked %v, moved %v; want close_issue decided and "+
			"export and purge not", got["accepted"], got["undecided"], got["blocked"], got["moved"])
	}
	detail, _ := got["detail"].(string)
	if !strings.Contains(detail, `Decided already: "close_issue"`) || !strings.Contains(detail, "export") {
		t.Errorf("the detail %q does not name what was decided", detail)
	}
	if len(reviews.written) != 1 || reviews.written[0].Tool != "close_issue" {
		t.Errorf("wrote %+v, want close_issue alone", reviews.written)
	}
}

// cloudTools is how many tools the large server lists, and changedTool the
// one it changed since its first listing: past the 256th by name.
const (
	cloudTools  = 500
	changedTool = "tool_0336"
)

// cloudReview is a server listing 500 tools, its listing held to the bounds a
// store writes it under, every one accepted as first listed and changedTool
// listed now under another definition, v2.
func cloudReview(t *testing.T) *fakeReviews {
	t.Helper()
	var was, now []coord.MCPListedTool
	for i := 1; i <= cloudTools; i++ {
		raw := fmt.Sprintf("tool_%04d", i)
		was = append(was, listedAs(raw, raw+"/v1"))
		if raw == changedTool {
			now = append(now, listedAs(raw, raw+"/v2"))
		} else {
			now = append(now, listedAs(raw, raw+"/v1"))
		}
	}
	entry := &config.MCPServer{Name: "tracker", Command: "tracker-mcp", ToolPrefix: "tr_"}
	accepted := map[string]coord.MCPAcceptedTool{}
	for i, a := range mcpreview.Baseline(coord.MCPListing{Server: "tracker", Tools: was}, time.Now()) {
		a.Version = uint64(10 + i)
		accepted[a.Tool] = a
	}
	listing := coord.MCPListing{Server: "tracker", Node: "node-a", Epoch: 1, At: time.Now(), Instances: 1,
		Tools: now}.Bounded()
	if err := listing.Validate(); err != nil {
		t.Fatal(err)
	}
	listings := []coord.MCPListing{listing}
	return &fakeReviews{review: queries.MCPReview{Server: "tracker", Listings: listings, Accepted: accepted,
		Changes: mcpreview.Changes(listings, accepted), Entry: entry}, moved: map[string]bool{}}
}

// alwaysAllow answers the held call to raw at digest Always allow, over reviews.
func alwaysAllow(t *testing.T, reviews *fakeReviews, raw, digest string) {
	t.Helper()
	run := waitingRun()
	run.HeldCall.Raw, run.HeldCall.Tool, run.HeldCall.Digest = raw, "tr_"+raw, digest
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Registry: &fakeRegistry{}, Detail: listedDetail(), Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: &fakeToolDocuments{}, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: &recordedAudit{},
		Approvals: &fakeApprovals{run: run}, Reviews: reviews,
		Guard: authz.ContextGuard(leadOf{lead: "cto", seat: "swe"})})
	if err != nil {
		t.Fatal(err)
	}
	code, body := answer(t, svc, proved(time.Minute), "/mcp-approvals/"+heldID, `{"answer": "`+sandbox.HeldAlways+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d %v", code, body)
	}
}

// ALWAYS ALLOW ON A TOOL PAST THE 256TH STICKS: a server listing 500 tools is
// listed whole, so the definition a call to its 336th tool was held on is
// accepted with the level, and the tool does not ask again.
//
// Mutation: cut a listing at 256 tools, or accept only a change the review's
// own list finds, and this is red.
func TestAlwaysAllowOnAToolPastThe256thAcceptsItsDefinition(t *testing.T) {
	t.Parallel()
	reviews := cloudReview(t)
	if n := len(reviews.review.Listings[0].Tools); n != cloudTools {
		t.Fatalf("the listing names %d tools of %d", n, cloudTools)
	}
	alwaysAllow(t, reviews, changedTool, reviewDigest(changedTool+"/v2"))
	if len(reviews.written) != 1 || reviews.written[0].Tool != changedTool ||
		reviews.written[0].Digest != reviewDigest(changedTool+"/v2") || reviews.written[0].By != "jane.doe" {
		t.Fatalf("always accepted %+v, want %s at the definition the call was made under", reviews.written,
			changedTool)
	}
	index := mcpreview.IndexOf([]coord.MCPAcceptedTool{reviews.written[0]})
	if index.Unreviewed("tracker", changedTool, reviewDigest(changedTool+"/v2")) {
		t.Error("the accepted definition still reads as unreviewed")
	}
}

// AN ALWAYS ALLOW THAT ACCEPTS NOTHING SAYS WHY in the log: a held call whose
// definition no live listing names (the server lists another since) at WARN,
// naming the definitions listed, and one already accepted at INFO.
//
// Not parallel: it installs the process's log sink.
//
// Mutation: return without a line where nothing is accepted, and this is red.
func TestAnAlwaysAllowThatAcceptsNothingSaysWhy(t *testing.T) {
	var out bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })

	reviews := cloudReview(t)
	alwaysAllow(t, reviews, changedTool, reviewDigest(changedTool+"/v3"))
	if len(reviews.written) != 0 {
		t.Fatalf("a definition no listing names was accepted: %+v", reviews.written)
	}
	alwaysAllow(t, reviews, "tool_0001", reviewDigest("tool_0001/v1"))
	if len(reviews.written) != 0 {
		t.Fatalf("an accepted definition was accepted again: %+v", reviews.written)
	}
	var unlisted, accepted string
	for line := range strings.Lines(out.String()) {
		switch {
		case strings.Contains(line, "mcp_held_definition_not_accepted") && strings.Contains(line, changedTool):
			unlisted = line
		case strings.Contains(line, "mcp_held_definition_left") && strings.Contains(line, "tool_0001"):
			accepted = line
		}
	}
	if !strings.Contains(unlisted, "level=WARN") || !strings.Contains(unlisted, "no live listing names it") ||
		!strings.Contains(unlisted, reviewDigest(changedTool+"/v2")) {
		t.Errorf("the unlisted definition logged %q, want a WARN saying no listing names it and what is listed",
			unlisted)
	}
	if !strings.Contains(accepted, "level=INFO") || !strings.Contains(accepted, "accepted already") {
		t.Errorf("the accepted definition logged %q, want an INFO saying it is accepted already", accepted)
	}
}

// fmtList is a JSON list of strings, comma-joined.
func fmtList(v any) string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return strings.Join(out, ",")
}
