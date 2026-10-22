package artifactsapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

const (
	pagePath  = "/artifacts/" + "%s"
	ifMatch2  = `"2"`
	publishV3 = `{"upload":"u-1","files":[{"path":"index.html","upload":true}],"capabilities":[]}`
)

func at(id artifacts.ID, rest string) string { return "/artifacts/" + id.String() + rest }

// A CALLER WITH NO RELATION IS TOLD THE ARTIFACT IS NOT FOUND, byte for byte as
// an id nothing holds, on every route that names one: a read, a write, a frame
// and a person's gesture alike, so no answer says a private artifact exists.
// Asked as a person, through a token and as an agent: a verb a token or an
// agent may not take is still not found to one with no relation, never the
// bar that would confirm the artifact exists.
func TestACallerWithNoRelationIsToldTheArtifactIsNotFound(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	agentDana := iam.Principal{Login: "dana", Kind: iam.KindSeat, Seat: "dana",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantArtifacts}}
	for _, route := range []struct{ method, rest, body string }{
		{"POST", "/tickets", ``},
		{"GET", "/versions/1/files/index.html", ``},
		{"GET", "/versions/1/source", ``},
		{"PATCH", "", `{"title":"Mine now"}`},
		{"POST", "/versions", publishV3},
		{"PUT", "/shares", `{"audience":{"kind":"seat","id":"dana"},"role":"editor"}`},
		{"DELETE", "", ``},
		{"POST", "/restore", ``},
		{"POST", "/transfer", `{"to":"@dana"}`},
		{"POST", "/work-items", `{"work_item":"ENG-12"}`},
		{"PUT", "/pin", ``},
	} {
		for who, p := range map[string]iam.Principal{"a person": person("dana"),
			"a token": token("dana"), "an agent": agentDana} {
			header := append(keyed(), "If-Match", ifMatch2)
			stranger := g.do(p, route.method, at(page, route.rest), route.body, header...)
			nothing := g.do(p, route.method, at(unknown, route.rest), route.body, header...)
			if stranger.Code != http.StatusNotFound || stranger.Body.String() != nothing.Body.String() ||
				nothing.Code != http.StatusNotFound {
				t.Errorf("%s %s, %s: a stranger is answered %d %s and an unknown id %d %s",
					route.method, route.rest, who, stranger.Code, stranger.Body, nothing.Code,
					nothing.Body)
			}
		}
	}
	if len(g.writes.all()) != 0 || len(g.pipeline.verbs()) != 0 || g.objects.opens() != 0 ||
		g.estate.versionReads() != 0 {
		t.Errorf("a refused caller reached the estate: writes %v, pipeline %v, %d opens, %d version reads",
			g.writes.all(), g.pipeline.verbs(), g.objects.opens(), g.estate.versionReads())
	}
}

// A RELATION BELOW THE VERB'S ROLE IS REFUSED NAMING THE ROLE, so the dashboard
// can say whom to ask for what; and the refusal comes before a byte is read or
// anything is written.
func TestARoleBelowTheVerbIsRefusedNamingTheRole(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", ifMatch2)...)
	if w.Code != http.StatusForbidden || errorOf(t, w) != "unauthorized" {
		t.Fatalf("a viewer publishing: %d %s", w.Code, w.Body)
	}
	if role := fields(t, w)["role"]; role != string(artifacts.RoleEditor) {
		t.Errorf("the refusal names role %v, want editor", role)
	}
	if v := g.pipeline.verbs(); len(v) != 0 {
		t.Errorf("a refused publish reached the pipeline: %v", v)
	}
}

// A PUBLISH NAMES ITS BASE: no If-Match is 428 unless forced, a malformed one
// is 400, and the base it names is the one the pipeline decides against.
func TestAPublishNamesTheVersionItWasBasedOn(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3, keyed()...); w.Code !=
		http.StatusPreconditionRequired || errorOf(t, w) != "bad_if_match" {
		t.Errorf("a publish naming no base: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3,
		append(keyed(), "If-Match", "2")...); w.Code != http.StatusBadRequest {
		t.Errorf("an unquoted If-Match: %d %s", w.Code, w.Body)
	}
	w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", ifMatch2)...)
	if w.Code != http.StatusOK {
		t.Fatalf("an editor's publish on the head: %d %s", w.Code, w.Body)
	}
	if got := g.pipeline.publishes[0]; got.Base != 2 || got.Force || got.Mode != "whole" {
		t.Errorf("the pipeline was asked base %d force %v mode %q", got.Base, got.Force, got.Mode)
	}
}

// A PUBLISH BASED ON A MOVED HEAD CARRIES WHAT A MERGE NEEDS: 412
// version_moved, the live head and its manifest, and the live entry's text —
// and a detail that names the artifact as the screen does, never by its id
// (artifactsdomain.Sentence): it read "artifact 0192f… is at version 2".
// Mutation: answer the moved head's Error as the detail and this goes red.
// The head names its folder only where the publisher sees it: sam is told
// nothing of jane's private Drafts the page is filed in.
func TestAPublishBasedOnAMovedHeadCarriesTheLiveEntry(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	live, _ := g.estate.Version(t.Context(), page, 2)
	filed := heads()[page]
	filed.Folder = drafts
	g.pipeline.err = &artifactsdomain.VersionMoved{Head: filed, Live: live}
	w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", `"1"`)...)
	if w.Code != http.StatusPreconditionFailed || errorOf(t, w) != "version_moved" {
		t.Fatalf("a publish on a moved head: %d %s", w.Code, w.Body)
	}
	if detail, _ := fields(t, w)["detail"].(string); !strings.HasPrefix(detail,
		"This artifact is at version 2") || strings.Contains(detail, page.String()) {
		t.Errorf("a publish on a moved head is told %q, want the artifact named "+
			"as the screen names it", detail)
	}
	var body struct {
		Head struct {
			HeadNumber int    `json:"head_number"`
			Folder     string `json:"folder"`
		} `json:"head"`
		Live  artifactsdomain.Version `json:"live"`
		Entry struct {
			Path string `json:"path"`
			Text string `json:"text"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Head.HeadNumber != 2 || body.Live.Number != 2 || body.Entry.Path != "index.html" ||
		body.Entry.Text != string(indexHTML) {
		t.Errorf("the 412 carries head %d, live %d, entry %q %q", body.Head.HeadNumber,
			body.Live.Number, body.Entry.Path, body.Entry.Text)
	}
	if body.Head.Folder != "" {
		t.Errorf("the 412 tells sam the folder %q he cannot see", body.Head.Folder)
	}
}

// A FORCED PUBLISH NEEDS A PERSON PRESENT (D08): a machine token is refused the
// force verb even where it may publish, and a person's force reaches the
// pipeline as one.
func TestAForcedPublishNeedsAPersonPresent(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	forced := `{"upload":"u-1","files":[{"path":"index.html","upload":true}],"force":true,"capabilities":[]}`
	if w := g.do(token("sam"), "POST", at(page, "/versions"), forced, keyed()...); w.Code !=
		http.StatusForbidden || errorOf(t, w) != "token_refused" {
		t.Errorf("a token forcing a publish: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "POST", at(page, "/versions"), forced, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("a person forcing a publish: %d %s", w.Code, w.Body)
	}
	if len(g.pipeline.publishes) != 1 || !g.pipeline.publishes[0].Force {
		t.Errorf("the pipeline saw %+v", g.pipeline.publishes)
	}
}

// A WRITE IS STAMPED WITH WHAT THE API DECIDED: the caller's exact role on the
// artifact, the surface it came through, the plan quotas and the work item it
// was made for, so the applier decides from the record alone.
func TestAWriteIsStampedWithTheCallersRoleAndOrigin(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := strings.Replace(publishV3, `"upload":"u-1"`, `"upload":"u-1","work_item":"CRW-412"`, 1)
	if w := g.do(person("eve"), "POST", at(page, "/versions"), body,
		append(keyed(), "If-Match", ifMatch2)...); w.Code != http.StatusOK {
		t.Fatalf("an ENG member publishing: %d %s", w.Code, w.Body)
	}
	a := g.pipeline.lastCall().Authority
	if a.Role != artifacts.RoleEditor || a.Origin != artifacts.OriginDashboard || a.WorkItem != "CRW-412" ||
		a.Limits != limits() || a.Actor.Name != "eve" || a.Elevated {
		t.Errorf("the publish was stamped %+v", a)
	}
	if w := g.do(token("jane"), "PATCH", at(page, ""), `{"title":"Renamed"}`, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("the owner renaming through a token: %d %s", w.Code, w.Body)
	}
	last := g.writes.all()[0]
	if last.c.Authority.Role != artifacts.RoleOwner || last.c.Authority.Origin != artifacts.OriginAPI {
		t.Errorf("the rename was stamped %+v", last.c.Authority)
	}
}

// EVERY WRITE NAMES ITS OPERATION: a write with no Idempotency-Key is refused
// before it is made, so a retry can never be a second write.
func TestEveryWriteNamesItsOperation(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	for _, route := range []struct{ method, path, body string }{
		{"PATCH", at(page, ""), `{"title":"x"}`},
		{"POST", at(page, "/versions"), publishV3},
		{"POST", "/artifacts", `{"kind":"html","title":"x"}`},
		{"POST", "/artifacts/uploads", `{"entries":[]}`},
		{"PUT", at(page, "/shares"), `{"audience":{"kind":"seat","id":"x"},"role":"viewer"}`},
		{"DELETE", at(page, ""), ``},
	} {
		w := g.do(person("jane"), route.method, route.path, route.body, "If-Match", ifMatch2)
		if w.Code != http.StatusBadRequest || errorOf(t, w) != "op_id_invalid" {
			t.Errorf("%s %s with no key: %d %s", route.method, route.path, w.Code, w.Body)
		}
	}
	if len(g.writes.all())+len(g.pipeline.verbs()) != 0 {
		t.Error("a write with no operation key was made")
	}
}

// THE API ORIGIN NEVER RENDERS AN ARTIFACT: a version's file
// is an attachment, any text and anything a browser could run is plain text,
// and the API's own policy forbids it loading or being framed; a page is
// opened only in a frame on the artifact origin, and a doc only
// as JSON for the dashboard to render.
func TestTheAPIOriginNeverRendersAnArtifact(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	for _, file := range []struct {
		path, want string
		body       []byte
	}{
		{"index.html", "text/plain; charset=utf-8", indexHTML},
		{"img/logo.svg", "text/plain; charset=utf-8", logoSVG},
		{"chart.png", "image/png", chartPNG},
	} {
		w := g.do(person("bob"), "GET", at(page, "/versions/2/files/"+file.path), ``)
		h := w.Result().Header
		switch {
		case w.Code != http.StatusOK:
			t.Errorf("%s: %d %s", file.path, w.Code, w.Body)
		case h.Get("Content-Type") != file.want:
			t.Errorf("%s served as %q, want %q", file.path, h.Get("Content-Type"), file.want)
		case !strings.HasPrefix(h.Get("Content-Disposition"), "attachment"):
			t.Errorf("%s is not an attachment: %q", file.path, h.Get("Content-Disposition"))
		case h.Get("Content-Security-Policy") != pagepolicy.API || h.Get("X-Content-Type-Options") != "nosniff":
			t.Errorf("%s carries policy %q and %q", file.path, h.Get("Content-Security-Policy"),
				h.Get("X-Content-Type-Options"))
		case w.Body.String() != string(file.body):
			t.Errorf("%s answered other bytes", file.path)
		}
	}
	src := g.do(person("bob"), "GET", at(notes, "/versions/1/source"), ``)
	if src.Code != http.StatusOK || !strings.HasPrefix(src.Result().Header.Get("Content-Type"), "application/json") ||
		fields(t, src)["text"] != string(notesMD) {
		t.Errorf("a doc's source: %d %q %s", src.Code, src.Result().Header.Get("Content-Type"), src.Body)
	}
	frame := g.do(person("bob"), "POST", at(page, "/tickets"), ``)
	if frame.Code != http.StatusCreated || !strings.HasPrefix(fields(t, frame)["frame_url"].(string),
		"https://"+string(page.Label())+".artifacts.example.net/") {
		t.Errorf("a page's frame: %d %s", frame.Code, frame.Body)
	}
}

// A FRAME OPENS ONE VERSION OF A PAGE FOR ONE CREDENTIAL, and only a page: a
// doc is the dashboard's to render, a deployment with no artifact
// origin says so, and the frame is bound to the credential the caller signed in
// through with the version's declared capabilities.
func TestAFrameOpensOneVersionOfAPageForOneCredential(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("bob"), "POST", at(notes, "/tickets"), ``); w.Code != http.StatusConflict ||
		errorOf(t, w) != "artifact_not_framed" {
		t.Errorf("a doc's frame: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "POST", at(binned, "/tickets"), ``); w.Code != http.StatusConflict ||
		errorOf(t, w) != "artifact_trashed" {
		t.Errorf("a trashed page's frame: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("bob"), "POST", at(page, "/tickets"), `{"version":1}`); w.Code != http.StatusCreated {
		t.Fatalf("a viewer's frame of version 1: %d %s", w.Code, w.Body)
	}
	r := g.framer.requests[0]
	if r.Version.Number != 1 || r.Lineage != person("bob").Via ||
		!slices.Equal(r.Granted, []artifacts.Capability{artifacts.CapabilityDB}) || r.Head.ID != page {
		t.Errorf("the frame was asked for %+v", r)
	}
	if len(g.marks.opened) != 1 || g.marks.opened[0].OpKey == "" {
		t.Errorf("the opening was recorded %d times: %+v", len(g.marks.opened), g.marks.opened)
	}

	bare := newRig(t, false)
	if w := bare.do(person("bob"), "POST", at(page, "/tickets"), ``); w.Code != http.StatusConflict ||
		errorOf(t, w) != "artifact_origin_unset" {
		t.Errorf("a frame with no artifact origin: %d %s", w.Code, w.Body)
	}
}

// A GALLERY'S PREVIEW OPENS NOTHING AND GRANTS ONLY A READ: a frame minted
// with `?purpose=preview` is decided as any open is, records no open, and is
// asked of the framer as a preview granted only what drawing needs (its
// assets, its datastore and a newcomer's state, which the dashboard's preview
// host answers with reads alone and a state of its own), so its page is
// offered no viewer and no room, and it files no ready report. An open still records its opening, and an
// administrator's preview of a private page is still recorded as the
// elevated read it is. A purpose this build does not know is refused.
func TestAPreviewsFrameRecordsNoOpeningAndGrantsOnlyARead(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("bob"), "POST", at(page, "/tickets?purpose=preview"), ``); w.Code != http.StatusCreated {
		t.Fatalf("a preview's frame: %d %s", w.Code, w.Body)
	}
	if len(g.framer.requests) != 1 {
		t.Fatalf("the framer was asked %d times", len(g.framer.requests))
	}
	r := g.framer.requests[0]
	if r.Purpose != artifacts.TicketPurposePreview || r.Head.ID != page {
		t.Errorf("the preview was asked for as %q of %s", r.Purpose, r.Head.ID)
	}
	// The page declares the datastore, and the preview may read it (the
	// dashboard's preview host answers reads alone); nothing past what the
	// preview capabilities name is granted.
	for _, c := range r.Granted {
		if !slices.Contains(artifacts.PreviewCapabilities, c) {
			t.Errorf("the preview was granted %q", c)
		}
	}
	if !slices.Contains(r.Granted, artifacts.CapabilityDB) {
		t.Errorf("the preview of a page declaring the datastore was granted %v", r.Granted)
	}
	if len(g.marks.opened) != 0 {
		t.Errorf("a preview recorded %d openings", len(g.marks.opened))
	}
	// AN OPEN STILL RECORDS ITS OPENING, granted what the version declares.
	if w := g.do(person("bob"), "POST", at(page, "/tickets"), ``); w.Code != http.StatusCreated ||
		len(g.marks.opened) != 1 {
		t.Errorf("an open recorded %d openings: %d %s", len(g.marks.opened), w.Code, w.Body)
	}
	if got := g.framer.requests[1]; got.Purpose != "" || !slices.Contains(got.Granted, artifacts.CapabilityDB) {
		t.Errorf("an open was asked for as %q with %v", got.Purpose, got.Granted)
	}
	// THE SAME AUTHORIZATION: a stranger's preview is refused as their open is.
	if w := g.do(person("dana"), "POST", at(page, "/tickets?purpose=preview"), ``); w.Code != http.StatusNotFound {
		t.Errorf("a stranger's preview: %d %s", w.Code, w.Body)
	}
	ada := person("ada", iam.GrantAdmin)
	if w := g.do(ada, "POST", at(page, "/tickets?purpose=preview"), ``); w.Code != http.StatusCreated {
		t.Fatalf("an administrator's preview: %d %s", w.Code, w.Body)
	}
	if len(g.marks.elevated) != 1 || len(g.marks.opened) != 1 {
		t.Errorf("an administrator's preview recorded %d elevated reads and %d openings, want 1 and 1",
			len(g.marks.elevated), len(g.marks.opened))
	}
	if w := g.do(person("bob"), "POST", at(page, "/tickets?purpose=embed"), ``); w.Code != http.StatusUnprocessableEntity ||
		errorOf(t, w) != "invalid" {
		t.Errorf("an unknown purpose: %d %s", w.Code, w.Body)
	}
}

// A BOARD READ AS A PREVIEW IS THE SAME BOARD and records no open; read as an
// open it records one, and a purpose this build does not know is refused.
func TestABoardReadAsAPreviewRecordsNoOpening(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	preview := g.do(person("bob"), "GET", at(pulse, "/versions/1/dashboard?purpose=preview"), ``)
	if preview.Code != http.StatusOK {
		t.Fatalf("a board's preview: %d %s", preview.Code, preview.Body)
	}
	if len(g.marks.opened) != 0 {
		t.Errorf("a board's preview recorded %d openings", len(g.marks.opened))
	}
	open := g.do(person("bob"), "GET", at(pulse, "/versions/1/dashboard"), ``)
	if open.Code != http.StatusOK || open.Body.String() != preview.Body.String() {
		t.Errorf("an open answered %d, and not the preview's board: %s", open.Code, open.Body)
	}
	if len(g.marks.opened) != 1 {
		t.Errorf("an open of the board recorded %d openings", len(g.marks.opened))
	}
	if w := g.do(person("dana"), "GET", at(pulse, "/versions/1/dashboard?purpose=preview"), ``); w.Code != http.StatusNotFound {
		t.Errorf("a stranger's preview of the board: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("bob"), "GET", at(pulse, "/versions/1/dashboard?purpose=embed"), ``); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown purpose: %d %s", w.Code, w.Body)
	}
}

// OPENING AN ARTIFACT TWICE IN ONE WINDOW IS ONE OPENING: the operation key is
// the window's, so the second collapses on the ledger.
func TestAnOpeningIsRecordedOncePerWindow(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	for range 2 {
		if w := g.do(person("bob"), "POST", at(page, "/tickets"), ``); w.Code != http.StatusCreated {
			t.Fatalf("a frame: %d %s", w.Code, w.Body)
		}
	}
	if len(g.marks.opened) != 2 || g.marks.opened[0].OpKey != g.marks.opened[1].OpKey {
		t.Fatalf("two openings in a window were keyed %+v", g.marks.opened)
	}
	if err := statelog.CheckCallerOpID(g.marks.opened[0].OpKey); err != nil {
		t.Errorf("the opening's key is not one the writer takes: %v", err)
	}
}

// AN ADMINISTRATOR'S READ OF A PRIVATE ARTIFACT IS RECORDED BEFORE IT IS
// ANSWERED (D09), as an elevated read of the version it reads; and a read
// whose record could not be written is not answered at all.
func TestAnAdministratorsReadIsRecordedBeforeItIsAnswered(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	ada := person("ada", iam.GrantAdmin)
	if w := g.do(ada, "GET", at(page, "/versions/1/files/index.html"), ``); w.Code != http.StatusOK {
		t.Fatalf("an administrator's download: %d %s", w.Code, w.Body)
	}
	if len(g.marks.elevated) != 1 || !g.marks.elevated[0].Authority.Elevated || g.marks.versions[0] != 1 {
		t.Fatalf("the elevated read was recorded %+v at %v", g.marks.elevated, g.marks.versions)
	}
	if w := g.do(person("sam"), "GET", at(page, "/versions/1/files/index.html"), ``); w.Code != http.StatusOK ||
		len(g.marks.elevated) != 1 {
		t.Errorf("an editor's own read was recorded as elevated (%d records)", len(g.marks.elevated))
	}

	failing := newRig(t, true)
	failing.marks.err = errBoom
	opens := failing.objects.opens()
	w := failing.do(ada, "GET", at(page, "/versions/1/files/index.html"), ``)
	if w.Code != http.StatusServiceUnavailable || failing.objects.opens() != opens {
		t.Errorf("an unrecorded elevated read was answered %d with %d opens", w.Code,
			failing.objects.opens()-opens)
	}
	if w := failing.do(ada, "POST", at(page, "/tickets"), ``); w.Code != http.StatusServiceUnavailable ||
		len(failing.framer.requests) != 0 {
		t.Errorf("an unrecorded elevated frame: %d, %d frames", w.Code, len(failing.framer.requests))
	}
}

// AN ADMINISTRATOR'S PUBLISH ON A MOVED HEAD IS A RECORDED READ (D09): the 412
// hands back the live entry's text, so a publisher only the admin path
// admitted is recorded as reading the live version before it is sent, and is
// answered 503 with nothing of it when the record cannot be written. An
// editor's 412 records nothing. Mutation: drop the record from versionMoved
// and the administrator reads the live page with no trace.
func TestAnAdministratorsPublishOnAMovedHeadIsARecordedRead(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	live, _ := g.estate.Version(t.Context(), page, 2)
	g.pipeline.err = &artifactsdomain.VersionMoved{Head: heads()[page], Live: live}
	ada := person("ada", iam.GrantAdmin)
	w := g.do(ada, "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", `"1"`)...)
	if w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), `"entry"`) {
		t.Fatalf("an administrator's publish on a moved head: %d %s", w.Code, w.Body)
	}
	if len(g.marks.elevated) != 1 || !g.marks.elevated[0].Authority.Elevated || g.marks.versions[0] != 2 {
		t.Fatalf("the live version's read was recorded %+v at %v", g.marks.elevated, g.marks.versions)
	}
	if w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3,
		append(keyed(), "If-Match", `"1"`)...); w.Code != http.StatusPreconditionFailed ||
		len(g.marks.elevated) != 1 {
		t.Errorf("an editor's 412 was recorded as elevated (%d %d records)", w.Code, len(g.marks.elevated))
	}

	failing := newRig(t, true)
	failing.marks.err = errBoom
	failing.pipeline.err = &artifactsdomain.VersionMoved{Head: heads()[page], Live: live}
	w = failing.do(ada, "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", `"1"`)...)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "alert(1)") ||
		strings.Contains(w.Body.String(), `"live"`) {
		t.Errorf("an unrecorded elevated 412: %d %s", w.Code, w.Body)
	}
}

// THE TRASH STOPS SERVING CONTENT AT ONCE: a trashed page's file, source and
// frame are refused artifact_trashed to its owner, and an administrator's read
// of it is refused before anything is recorded or read. Mutation: drop the
// status check from Gate.Content and the file is served.
func TestTheTrashStopsServingContent(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	opens := g.objects.opens()
	for _, who := range []iam.Principal{person("jane"), person("ada", iam.GrantAdmin)} {
		for _, route := range []struct{ method, rest string }{
			{"GET", "/versions/1/files/index.html"},
			{"GET", "/versions/1/source"},
			{"POST", "/tickets"},
		} {
			if w := g.do(who, route.method, at(binned, route.rest), ``); w.Code != http.StatusConflict ||
				errorOf(t, w) != "artifact_trashed" {
				t.Errorf("%s %s on a trashed page as %s: %d %s", route.method, route.rest,
					who.Seat, w.Code, w.Body)
			}
		}
	}
	if len(g.marks.elevated) != 0 || g.objects.opens() != opens || g.estate.versionReads() != 0 {
		t.Errorf("a trashed page's refused reads left %d records, %d opens and %d version reads",
			len(g.marks.elevated), g.objects.opens()-opens, g.estate.versionReads())
	}
}

// A PURGE IS AN ADMINISTRATOR'S, WITH A FRESH PROOF: the owner may not purge,
// an administrator whose proof of identity is old is asked to step up, and an
// administrator's purge of somebody else's artifact is stamped elevated.
func TestAPurgeIsAnAdministratorsWithAFreshProof(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("jane"), "POST", at(page, "/purge"), ``, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("the owner purging: %d %s", w.Code, w.Body)
	}
	stale := person("ada", iam.GrantAdmin)
	stale.ReauthAt = time.Now().Add(-time.Minute)
	if w := g.do(stale, "POST", at(page, "/purge"), ``, keyed()...); w.Code != http.StatusForbidden ||
		errorOf(t, w) != "step_up_required" {
		t.Errorf("a stale administrator purging: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("ada", iam.GrantAdmin), "POST", at(page, "/purge"), ``, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("an administrator purging: %d %s", w.Code, w.Body)
	}
	calls := g.writes.all()
	if len(calls) != 1 || calls[0].verb != "purge" || !calls[0].c.Authority.Elevated {
		t.Errorf("the purge was written %+v", calls)
	}
	if w := g.do(person("ada", iam.GrantAdmin), "POST", at(unknown, "/purge"), ``, keyed()...); w.Code !=
		http.StatusNotFound {
		t.Errorf("a purge of an id nothing holds: %d", w.Code)
	}
}

// THE OWNER'S GESTURES ARE A PERSON'S: an agent never trashes, and a transfer
// needs a person present, where a person's goes through naming the seat.
func TestTrashAndTransferAreAPersonsGestures(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	scribe := iam.Principal{Login: "scribe", Kind: iam.KindSeat, Seat: "scribe", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantArtifacts}}
	if w := g.do(scribe, "DELETE", at(report, ""), ``, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("an agent trashing its own artifact: %d %s", w.Code, w.Body)
	}
	if w := g.do(token("jane"), "POST", at(page, "/transfer"), `{"to":"sam"}`, keyed()...); w.Code !=
		http.StatusForbidden || errorOf(t, w) != "token_refused" {
		t.Errorf("a token transferring: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "POST", at(page, "/transfer"), `{"to":"@sam"}`, keyed()...); w.Code !=
		http.StatusOK {
		t.Fatalf("the owner transferring: %d %s", w.Code, w.Body)
	}
	if c := g.writes.all(); len(c) != 1 || c[0].verb != "transfer" || c[0].arg != "sam" {
		t.Errorf("the transfer was written %+v", c)
	}
}

// THE LEAD OF AN AGENT HOLDS ITS ARTIFACTS (D12), and of a person's nothing:
// lee, who leads scribe, renames scribe's report, and jane's notes, shared with
// bob alone, are no more lee's than a stranger's.
func TestTheLeadOfAnAgentHoldsItsArtifacts(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("lee"), "PATCH", at(report, ""), `{"title":"Better"}`, keyed()...); w.Code != http.StatusOK {
		t.Errorf("the lead renaming the agent's report: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("lee"), "PATCH", at(notes, ""), `{"title":"Mine"}`, keyed()...); w.Code !=
		http.StatusNotFound {
		t.Errorf("a lead renaming a person's artifact: %d %s", w.Code, w.Body)
	}
}

// THE SHARE DIALOG: a share and a withdrawal are the owner's, a share names
// its audience and role and the record names the grantor, and the effective
// access lists whom each unit share reaches, to the owner and editors only.
func TestTheShareDialogGrantsRevokesAndListsAccess(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	grant := `{"audience":{"kind":"unit","id":"OPS"},"role":"viewer"}`
	if w := g.do(person("sam"), "PUT", at(page, "/shares"), grant, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("an editor sharing: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "PUT", at(page, "/shares"), grant, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("the owner sharing: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "DELETE", at(page, "/shares/seat:bob"), ``, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("the owner withdrawing bob's share: %d %s", w.Code, w.Body)
	}
	calls := g.writes.all()
	if len(calls) != 2 || calls[0].arg.(artifacts.Share).Audience.ID != "OPS" ||
		calls[1].arg.(artifacts.Audience) != (artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}) {
		t.Errorf("the dialog wrote %+v", calls)
	}

	full, err := g.svc.Gate().Access(t.Context(), person("sam"), page)
	if err != nil {
		t.Fatal(err)
	}
	var eng []string
	for _, s := range full.Shares {
		if s.Audience.ID == "ENG" {
			eng = s.Members
		}
	}
	if !slices.Equal(eng, []string{"eve", "lee"}) || full.ShareCount != 3 {
		t.Errorf("an editor sees ENG reaching %v of %d shares", eng, full.ShareCount)
	}
	viewer, err := g.svc.Gate().Access(t.Context(), person("bob"), page)
	if err != nil || viewer.Shares != nil || viewer.ShareCount != 3 || viewer.Owner != "jane" {
		t.Errorf("a viewer sees %+v (%v)", viewer, err)
	}
	led, err := g.svc.Gate().Access(t.Context(), person("lee"), report)
	if err != nil || !slices.Equal(led.Leads, []string{"lee"}) {
		t.Errorf("an agent's artifact lists its leads as %v (%v)", led.Leads, err)
	}
}

// A SHARE NAMES A SEAT OR A UNIT THE COMPANY HOLDS, OR THE COMPANY. A login
// named as a seat (`token:maya`) was answered 200 and written, counted in
// Maya's Shared listing and admitted by nothing, since the authority table
// matches a seat share against a principal's seat alone; a seat or a unit
// nobody holds was written the same way. Each is refused with the sentence
// that says why, and nothing is written, on the share and on a create alike.
func TestAShareNamesOnlyAnAudienceTheCompanyHolds(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	for _, c := range []struct{ audience, says string }{
		{`{"kind":"seat","id":"token:maya"}`, "is not a seat handle"},
		{`{"kind":"seat","id":"maya.ortiz"}`, "is not a seat handle"},
		{`{"kind":"seat","id":"ghost"}`, `the company has no seat "ghost"`},
		{`{"kind":"unit","id":"NOPE"}`, `the company has no unit "NOPE"`},
		{`{"kind":"link","id":"x"}`, `"link" isn't someone you can share with. Use `},
	} {
		grant := `{"audience":` + c.audience + `,"role":"viewer"}`
		w := g.do(person("jane"), "PUT", at(page, "/shares"), grant, keyed()...)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(fields(t, w)["detail"].(string), c.says) {
			t.Errorf("sharing with %s: %d %s", c.audience, w.Code, w.Body)
		}
		create := `{"kind":"html","title":"Plan","upload":"u-1","files":[{"path":"index.html","upload":true}],` +
			`"shares":[` + grant + `]}`
		if w := g.do(person("dana"), "POST", "/artifacts", create, keyed()...); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("creating shared with %s: %d %s", c.audience, w.Code, w.Body)
		}
	}
	if calls := g.writes.all(); len(calls) != 0 {
		t.Errorf("a refused share was written: %+v", calls)
	}
	if len(g.pipeline.creates) != 0 {
		t.Errorf("a refused create reached the pipeline: %+v", g.pipeline.creates)
	}
	for _, ok := range []string{`{"kind":"seat","id":"bob"}`, `{"kind":"unit","id":"ENG"}`, `{"kind":"company"}`} {
		if w := g.do(person("jane"), "PUT", at(page, "/shares"), `{"audience":`+ok+`,"role":"viewer"}`,
			keyed()...); w.Code != http.StatusOK {
			t.Errorf("sharing with %s: %d %s", ok, w.Code, w.Body)
		}
	}
}

// A SHARE STORED BEFORE THE GRANT RULE IS THE OWNER'S TO SEE, CHANGE AND
// WITHDRAW. A login named as a seat reaches nobody, and the rule refuses a new
// one; the one an earlier build wrote is listed in the dialog's access, its
// role may change, and DELETE by its text form removes it, never a record the
// API could neither honour nor remove.
func TestAShareStoredBeforeTheGrantRuleIsListedAndWithdrawn(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	full, err := g.svc.Gate().Access(t.Context(), person("jane"), legacy)
	if err != nil {
		t.Fatal(err)
	}
	stored := artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "token:maya"}
	if !slices.ContainsFunc(full.Shares, func(s artifactsapi.ShareAccess) bool {
		return s.Audience == stored && s.Invalid
	}) {
		t.Errorf("the owner's access does not list the stored share as invalid: %+v", full.Shares)
	}
	change := `{"audience":{"kind":"seat","id":"token:maya"},"role":"commenter"}`
	if w := g.do(person("jane"), "PUT", at(legacy, "/shares"), change, keyed()...); w.Code != http.StatusOK {
		t.Errorf("changing the stored share's role: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "DELETE", at(legacy, "/shares/seat:token:maya"), ``, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("withdrawing the stored share: %d %s", w.Code, w.Body)
	}
	calls := g.writes.all()
	if len(calls) != 2 || calls[0].arg.(artifacts.Share).Audience != stored || calls[1].arg.(artifacts.Audience) != stored {
		t.Errorf("the writes were %+v", calls)
	}
	// The same audience is still refused as a NEW grant on another artifact.
	if w := g.do(person("jane"), "PUT", at(page, "/shares"), change, keyed()...); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a new share to the login: %d %s", w.Code, w.Body)
	}
}

// A READER BELOW EDITOR SEES THE OWNER AND A COUNT, NOT THE AUDIENCE, and not
// the elevated reads in the history.
func TestAViewerSeesTheOwnerAndACountNotTheAudience(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	viewer, err := g.svc.Gate().Detail(t.Context(), g.estate, person("bob"), page)
	if err != nil {
		t.Fatal(err)
	}
	if viewer.Shares != nil || viewer.ShareCount != 3 || viewer.Role != artifacts.RoleViewer || viewer.Writer {
		t.Errorf("a viewer is shown %+v", viewer)
	}
	editor, err := g.svc.Gate().Detail(t.Context(), g.estate, person("sam"), page)
	if err != nil || len(editor.Shares) != 3 || !editor.Writer {
		t.Errorf("an editor is shown %d shares, writer %v (%v)", len(editor.Shares), editor.Writer, err)
	}
	a, err := g.svc.Gate().Read(t.Context(), person("bob"), page, 0)
	if err != nil {
		t.Fatal(err)
	}
	history := artifactsdomain.HistoryPage{Entries: []artifactsdomain.HistoryEntry{
		{Op: string(artifactsdomain.OpPublish), Number: 2},
		{Op: string(artifactsdomain.OpReadElevated), Number: 2},
		{Op: string(artifactsdomain.OpShare), Summary: artifactsdomain.HistorySummary{Audience: "seat:bob", Role: "viewer"}},
	}}
	shown, err := g.svc.Gate().History(t.Context(), g.estate, a, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown.Entries) != 2 || shown.Entries[1].Summary.Audience != "" {
		t.Errorf("a viewer is shown the history %+v", shown.Entries)
	}
}

// WHO HAS OPENED AN ARTIFACT IS SHOWN TO ITS OWNER AND EDITORS, and only while
// the company shows it at all (D21).
func TestSeenByIsShownToEditorsWhileTheCompanyShowsIt(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if seen, err := g.svc.Gate().SeenBy(t.Context(), g.estate, person("sam"), page); err != nil ||
		!seen.Shown || len(seen.Reads) != 1 {
		t.Errorf("an editor is shown %+v (%v)", seen, err)
	}
	if seen, err := g.svc.Gate().SeenBy(t.Context(), g.estate, person("bob"), page); err != nil || seen.Shown ||
		len(seen.Reads) != 0 {
		t.Errorf("a viewer is shown %+v (%v)", seen, err)
	}
	g.company = config.CompanyArtifacts{SeenBy: config.SeenByNobody}
	if seen, err := g.svc.Gate().SeenBy(t.Context(), g.estate, person("jane"), page); err != nil || seen.Shown {
		t.Errorf("the owner is shown %+v while the company shows nobody (%v)", seen, err)
	}
}

// A LISTING IS FILTERED FOR THE VIEWER'S RELATIONS AND DECIDED ROW BY ROW: the
// estate is asked with the units the chart seats the viewer in and the agents
// it says they lead, and a row the table does not show is left out.
func TestAListingIsFilteredForTheViewersRelations(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	h := heads()
	g.estate.listItems = []artifactsdomain.ListItem{{Head: h[page]}, {Head: h[report]}, {Head: h[notes]}}
	got, err := g.svc.Gate().List(t.Context(), g.estate, person("lee"), artifactsdomain.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	v := g.estate.listed[0]
	if v.Record != "lee" || !slices.Equal(v.Units, []string{"ENG"}) || !slices.Equal(v.LedOwners,
		[]string{"scribe"}) || v.Admin {
		t.Errorf("the estate was asked for %+v", v)
	}
	var ids []artifacts.ID
	for _, item := range got.Contents {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, []artifacts.ID{page, report}) {
		t.Errorf("lee is listed %v, want the page (ENG) and the report (led), not jane's notes", ids)
	}
	if got.Contents[0].Shares == nil || got.Contents[0].Role != artifacts.RoleEditor {
		t.Errorf("an editor's row is shaped %+v", got.Contents[0].Artifact)
	}

	admin, err := g.svc.Gate().List(t.Context(), g.estate, person("ada", iam.GrantAdmin), artifactsdomain.ListQuery{})
	if err != nil || len(admin.Contents) != 0 {
		t.Errorf("an administrator's listing surfaced %d private artifacts (%v)", len(admin.Contents), err)
	}
}

// THE TRANSFER LEG IS DECIDED AGAIN ON THE UPLOAD'S OWN VERB: a caller who has
// lost the artifact since the intent is answered as a stranger and no byte
// moves, and one who still holds it streams the file to the path it names.
func TestTheTransferLegIsDecidedAgainOnTheUploadsOwnVerb(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.pipeline.upload = artifactupload.Upload{ID: "u-1", Artifact: page}
	path := artifactupload.TransferPath("u-1", "img/logo one.svg")
	if w := g.do(person("bob"), "PUT", path, "<svg/>"); w.Code != http.StatusForbidden {
		t.Errorf("a viewer streaming into a publish: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("dana"), "PUT", path, "<svg/>"); w.Code != http.StatusNotFound {
		t.Errorf("a stranger streaming into a publish: %d %s", w.Code, w.Body)
	}
	if len(g.pipeline.transfers) != 0 {
		t.Fatalf("a refused transfer moved bytes: %v", g.pipeline.transfers)
	}
	if w := g.do(person("sam"), "PUT", path, "<svg/>"); w.Code != http.StatusOK {
		t.Fatalf("an editor streaming: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(g.pipeline.transfers, []string{"u-1/img/logo one.svg=<svg/>"}) {
		t.Errorf("the pipeline was handed %v", g.pipeline.transfers)
	}
}

// A TRANSFER WHOSE BODY STOPPED IS TOLD IN A PERSON'S WORDS ([paced.Unread]):
// the detail was the read's own failure as Go spells it ("the body could not
// be read: unexpected EOF"), which goes to the log. Mutation: answer
// err.Error() again and this goes red.
func TestATransferWhoseBodyStoppedIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.pipeline.upload = artifactupload.Upload{ID: "u-1", Artifact: page}
	g.pipeline.err = fmt.Errorf("%w: %w", paced.ErrBody, io.ErrUnexpectedEOF)
	w := g.do(person("sam"), "PUT", artifactupload.TransferPath("u-1", "a.css"), "b{}")
	if w.Code != http.StatusBadRequest || fields(t, w)["error"] !=
		string(httpjson.CodeUnreadableBody) || fields(t, w)["detail"] != paced.Unread {
		t.Errorf("a transfer whose body stopped answered %d %s, want 400 "+
			"unreadable_body saying %q", w.Code, w.Body, paced.Unread)
	}
}

// A BODY THAT DOES NOT DECODE IS TOLD IN WORDS ([plainjson.Unreadable]): the
// detail was encoding/json's own sentence (`json: unknown field "bogus"`,
// "invalid character … looking for beginning of value"). Mutation: answer
// err.Error() again and both rows go red.
func TestABodyThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	for _, tc := range []struct{ body, says string }{
		{`{"kind":"html","title":"Plan","bogus":1}`,
			`In the body, "bogus" isn't a field this takes.`},
		{`{"kind":`, "The body isn't valid JSON: it ends too soon."},
	} {
		w := g.do(person("dana"), "POST", "/artifacts", tc.body, keyed()...)
		if w.Code != http.StatusBadRequest || fields(t, w)["error"] !=
			string(httpjson.CodeInvalidBody) || fields(t, w)["detail"] != tc.says {
			t.Errorf("%s answered %d %s, want 400 invalid_body saying %q", tc.body,
				w.Code, w.Body, tc.says)
		}
	}
}

// A CREATE IS OWNED BY ITS CREATOR and asks the artifacts app: a person without
// it is refused naming the app, and a person with it creates an artifact they
// own, with the shares and the work item they named.
func TestACreateIsOwnedByItsCreator(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kind":"html","title":"Plan","upload":"u-1","files":[{"path":"index.html","upload":true}],` +
		`"shares":[{"audience":{"kind":"seat","id":"bob"},"role":"viewer"}],"work_item":"CRW-7"}`
	noApp := person("dana")
	noApp.Grants = nil
	if w := g.do(noApp, "POST", "/artifacts", body, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("a person without the app creating: %d %s", w.Code, w.Body)
	}
	w := g.do(person("dana"), "POST", "/artifacts", body, keyed()...)
	if w.Code != http.StatusCreated || fields(t, w)["artifact"] != artifacts.IDFor("created").String() {
		t.Fatalf("a create: %d %s", w.Code, w.Body)
	}
	req := g.pipeline.creates[0]
	if req.Owner != "dana" || req.OwnerKind != iam.ActorHuman || len(req.Shares) != 1 ||
		req.Shares[0].Audience.ID != "bob" {
		t.Errorf("the pipeline was asked %+v", req)
	}
	if c := g.pipeline.lastCall().Authority; c.Role != artifacts.RoleOwner || c.WorkItem != "CRW-7" {
		t.Errorf("the create was stamped %+v", c)
	}
}

// A DESIGN'S LAYOUT REACHES THE PIPELINE AS IT WAS SENT: the boards the
// person laid out, with their briefs, and the notes beside them.
func TestADesignLayoutReachesThePipeline(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kind":"design","title":"Launch","starter":true,"layout":{` +
		`"boards":[{"title":"Home","x":0,"y":0,"w":1440,"h":1024,"brief":"Hero"}],` +
		`"notes":[{"id":"s1","kind":"sticky","x":0,"y":1100,"w":240,"h":160,"text":"Brand"}]}}`
	if w := g.do(person("dana"), "POST", "/artifacts", body, keyed()...); w.Code != http.StatusCreated {
		t.Fatalf("a design from a layout: %d %s", w.Code, w.Body)
	}
	req := g.pipeline.creates[0]
	if !req.Starter || req.Layout == nil || len(req.Layout.Boards) != 1 ||
		req.Layout.Boards[0].Brief != "Hero" || req.Layout.Boards[0].W != 1440 ||
		len(req.Layout.Notes) != 1 || req.Layout.Notes[0].Text != "Brand" {
		t.Errorf("the pipeline was asked %+v (layout %+v)", req, req.Layout)
	}
}

// EVERY REFUSAL OF THE PIPELINE AND THE DOMAIN HAS ITS OWN ANSWER, so a client
// branches on a code, and none of them reads as the node failing.
func TestEveryRefusalOfAPublishHasItsOwnAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{&artifactsdomain.QuotaExceeded{Limit: 1, Usage: 2, Key: "artifacts.limits.company_bytes"}, 409, "quota_exceeded"},
		{&artifactsdomain.CarriedObjectGone{Paths: []string{"a.css"}}, 409, "carried_object_gone"},
		{&artifactupload.UploadMismatch{Path: "a.css"}, 422, "upload_mismatch"},
		{&artifactupload.UploadMissing{Paths: []string{"a.css"}}, 409, "upload_missing"},
		{&artifactupload.UploadsOutstanding{Scope: "principal", Until: time.Now().Add(time.Minute)}, 429, "uploads_outstanding"},
		{&artifactupload.ContentRefused{Path: "a.png"}, 422, "content_refused"},
		{&edits.Refused{Index: 1, Path: "index.html"}, 422, "edit_refused"},
		{&lint.Refusal{Findings: []lint.Finding{{Path: "index.html", Message: "no"}}}, 422, "policy_refused"},
		{artifactupload.ErrMediaType, 415, "media_type_refused"},
		{artifactupload.ErrCapabilityUnknown, 422, "capability_unknown"},
		{artifactupload.ErrContractUnknown, 422, "contract_unknown"},
		{artifactupload.ErrUploadNotFound, 404, "upload_not_found"},
		{artifactupload.ErrUploadExpired, 410, "upload_expired"},
		{artifactsdomain.ErrUploadStale, 409, "upload_expired"},
		{artifactupload.ErrUploadConsumed, 409, "upload_consumed"},
		{artifactsdomain.ErrLabelTaken, 409, "label_taken"},
		{artifactsdomain.ErrLabelsFull, 409, "labels_full"},
		{artifactsdomain.ErrExists, 409, "exists"},
		{artifactsdomain.ErrTrashed, 409, "artifact_trashed"},
		{artifactsdomain.ErrPurging, 409, "artifact_purging"},
		{artifactsdomain.ErrAboveOwnRole, 403, "forbidden"},
		{artifactsdomain.ErrInvalid, 422, "invalid"},
		{statelog.ErrUnavailable, 503, "unavailable"},
	} {
		g := newRig(t, true)
		g.pipeline.err = c.err
		w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3, append(keyed(), "If-Match", ifMatch2)...)
		if w.Code != c.status || errorOf(t, w) != c.code {
			t.Errorf("%v: answered %d %s, want %d %s", c.err, w.Code, errorOf(t, w), c.status, c.code)
		}
	}
}

// EVERY ROUTE IS MOUNTED THROUGH THE AUTHORITY TABLE AND DOCUMENTED, and the
// path the pipeline hands a client for an engine-streamed file is one the
// surface routes. The router refuses a route with no policy, so a route that
// mounts at all states its authority; the reference is what an operator reads,
// and a route missing from it is invisible to them.
func TestEveryRouteIsGuardedAndDocumented(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	g := newRig(t, true)
	if err := g.svc.Routes(rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.patterns) != 38 {
		t.Fatalf("the surface mounts %d routes, and this case knows 38: %v", len(rec.patterns), rec.patterns)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "reference", "api-endpoints.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range rec.patterns {
		_, route, _ := strings.Cut(pattern, " ")
		route = strings.ReplaceAll(route, "...}", "}")
		if !strings.Contains(string(doc), "/"+strings.TrimPrefix(route, "/")) {
			t.Errorf("%s is mounted and docs/reference/api-endpoints.md does not document %s", pattern, route)
		}
	}
	req, _ := http.NewRequest(http.MethodPut, artifactupload.TransferPath("u-1", "a/b.css"), nil)
	if _, pattern := g.mux.Handler(req); pattern != "PUT /artifacts/uploads/{upload}/files/{path...}" {
		t.Errorf("a transfer path routes to %q", pattern)
	}
}

type recorder struct{ patterns []string }

func (r *recorder) Handle(pattern string, _ http.Handler) { r.patterns = append(r.patterns, pattern) }

// THE GATE REFUSES TO BE BUILT WITHOUT WHAT IT DECIDES BY.
func TestTheSurfaceRefusesAGateMissingWhatItDecidesBy(t *testing.T) {
	t.Parallel()
	for name, gate := range map[string]artifactsapi.Gate{
		"heads":  {Chart: chart{}, Limits: limits, Marks: &marks{}, Roster: roster{}},
		"chart":  {Heads: &estate{}, Limits: limits, Marks: &marks{}, Roster: roster{}},
		"limits": {Heads: &estate{}, Chart: chart{}, Marks: &marks{}, Roster: roster{}},
		"marks":  {Heads: &estate{}, Chart: chart{}, Limits: limits, Roster: roster{}},
		"roster": {Heads: &estate{}, Chart: chart{}, Limits: limits, Marks: &marks{}},
	} {
		if _, err := artifactsapi.New(artifactsapi.Options{Gate: gate, Reads: &estate{}, Writes: &writes{},
			Objects: &objects{}, Uploads: func() (artifactsapi.Pipeline, error) { return nil, nil }}); err == nil {
			t.Errorf("a gate with no %s was accepted", name)
		}
	}
}

var _ = authz.ActionArtifactRead

// A DOC BEGUN WITH ITS TEXT reaches the pipeline as the starter and the text,
// so the dashboard's New doc lands the doc its writer wrote as version 1.
func TestADocBegunWithItsTextReachesThePipeline(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kind":"doc","title":"Runbook","starter":true,"doc":{"body":"## Steps\n\nPage the on-call."}}`
	if w := g.do(person("dana"), "POST", "/artifacts", body, keyed()...); w.Code != http.StatusCreated {
		t.Fatalf("a create: %d %s", w.Code, w.Body)
	}
	req := g.pipeline.creates[0]
	if !req.Starter || req.Doc == nil || req.Doc.Body != "## Steps\n\nPage the on-call." {
		t.Errorf("the pipeline was asked %+v", req)
	}
}
