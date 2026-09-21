package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
)

// `crewlet chat import`, and the properties a migration is worth nothing
// without.
//
// Every test here drives the whole command — the archive on disk, the
// operator's map, the resolution against a company, and the writes — against
// a node that keeps what it is sent. What is asserted is never "a request was
// made": it is that a year of somebody else's workspace arrives as the
// conversation it was, wakes nobody, and can be replayed a second time
// without becoming two conversations.

// importFake is a node that accepts imported messages and keeps them.
//
// # Why the duplicate rule here is the applier's own
//
// A fake that agreed only with itself would prove nothing. This one declines
// a message whose (source, vendor id) it has already stored, which is exactly
// the pair [chat.Imported] carries and exactly the pair the applier's row
// guard queries — internal/chat's own TestASecondImportOfOneArchiveWritesNothingNew
// and TestAnImportWakesNobodyAndRunsTwice are what hold the real applier to
// it, against a real broker and a real store. What is under test HERE is the
// half those cannot reach: that this command derives the same pair on a
// second pass over one directory.
type importFake struct {
	server *httptest.Server

	rooms  []map[string]any
	agents []map[string]any

	// stored is every message this node holds, in arrival order, keyed by
	// the pair that identifies an imported message.
	stored []importFakeRow
	byPair map[string]bool

	// raw is every body this node was sent, decoded loosely, so a test can
	// assert about fields the typed shape does NOT have.
	raw []map[string]any

	declined int
}

type importFakeRow struct {
	ChannelID string
	In        importSubmission
}

func newImportFake(t *testing.T, rooms []map[string]any, agents []string) *importFake {
	t.Helper()
	fake := &importFake{rooms: rooms, byPair: map[string]bool{}}
	for _, handle := range agents {
		fake.agents = append(fake.agents, map[string]any{"handle": handle})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /chat/channels", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"read_level": "stale", "complete": true,
			"position": map[string]any{"stream": "CREWLET_CHAT_LOG", "seq": 9},
			"channels": fake.rooms,
		})
	})
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, fake.agents)
	})
	mux.HandleFunc("POST /chat/channels/{channel_id}/import",
		func(w http.ResponseWriter, r *http.Request) {
			var in importSubmission
			var loose map[string]any
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading the import body: %v", err)
			}
			if err := json.Unmarshal(body, &in); err != nil {
				t.Errorf("the import body was not the expected JSON: %v", err)
			}
			if err := json.Unmarshal(body, &loose); err != nil {
				t.Errorf("the import body was not JSON at all: %v", err)
			}
			fake.raw = append(fake.raw, loose)
			pair := in.Source + "\x00" + in.VendorID
			if fake.byPair[pair] {
				// THE APPLIER'S OWN ANSWER to a second pass: the
				// record is accepted and the row is not written
				// twice.
				fake.declined++
				writeTestJSON(t, w, map[string]any{
					"outcome": "applied", "op_id": in.VendorID,
				})
				return
			}
			fake.byPair[pair] = true
			fake.stored = append(fake.stored, importFakeRow{
				ChannelID: r.PathValue("channel_id"), In: in,
			})
			writeTestJSON(t, w, map[string]any{
				"outcome": "applied", "op_id": in.VendorID,
			})
		})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encoding the answer: %v", err)
	}
}

func importRoomRow(id, name, kind string) map[string]any {
	return map[string]any{"channel": map[string]any{
		"id": id, "name": name, "kind": kind}}
}

// writeImportMap drops an operator's map in a temp dir.
func writeImportMap(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// slackMap is the map the Slack fixture needs: every author it holds, and no
// channel renaming.
const slackMap = `authors:
  U0FOUNDER: founder
  U0ENGINEER: engineer
  U0STRANGER: stranger
`

func slackFake(t *testing.T) *importFake {
	t.Helper()
	return newImportFake(t, []map[string]any{
		importRoomRow("c-eng", "engineering", "public"),
		importRoomRow("c-leads", "leads", "private"),
	}, []string{"engineer"})
}

// importCLI runs one `crewlet chat import` against a fake node.
func importCLI(t *testing.T, fake *importFake, args ...string) (string, string, error) {
	t.Helper()
	full := append([]string{"chat", "import"}, args...)
	full = append(full, "-url", fake.server.URL, "-token", "t0ken")
	return cli(t, full...)
}

// A SECOND PASS OVER ONE ARCHIVE WRITES NOTHING, which is the whole of what
// makes an interrupted import safe to re-run.
//
// There is no cursor anywhere and nothing to remember where a run stopped: a
// message's id is derived from (source, vendor id), so the second pass
// derives the same ids and the applier declines every one of them. This test
// counts the rows on both passes and holds the DERIVED PAIR identical — the
// mutation that turns it red is minting anything per run (a uuid, a
// timestamp, a counter) into the vendor id.
func TestASecondImportOfOneArchiveWritesNothingNew(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	mapPath := writeImportMap(t, slackMap)

	out, _, err := importCLI(t, fake, "testdata/chatimport/slack", "-map", mapPath)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if len(fake.stored) != 3 {
		t.Fatalf("the first pass wrote %d message(s), want 3:\n%s",
			len(fake.stored), out)
	}
	first := append([]importFakeRow(nil), fake.stored...)

	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", mapPath); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(fake.stored) != 3 {
		t.Fatalf("a second pass over one archive left %d message(s), want 3 — "+
			"an interrupted import would be a second copy of the conversation",
			len(fake.stored))
	}
	if fake.declined != 3 {
		t.Errorf("the second pass presented %d message(s) the node already "+
			"held, want 3", fake.declined)
	}
	for i, row := range first {
		if row.In.VendorID != fake.stored[i].In.VendorID {
			t.Errorf("message %d derived %q on the first pass and %q on the "+
				"second", i, row.In.VendorID, fake.stored[i].In.VendorID)
		}
	}
}

// AN IMPORT WAKES NOBODY, and this command is one of the two halves of that.
//
// The other half is the record's own shape, which internal/chat holds
// (TestAnImportedMessageCarriesNoRoutingSnapshot). This half is that nothing
// a routing snapshot could be BUILT from is ever sent: no resolved mentions
// and no `@channel` flag, on any message, however much of either the archive
// contained. The Slack fixture's first message names somebody and the second
// is a thread reply, so both routing reasons are represented.
func TestAnImportSendsNothingAWakeCouldBeDerivedFrom(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	out, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(fake.raw) == 0 {
		t.Fatal("nothing was sent, so this test proves nothing")
	}
	for i, body := range fake.raw {
		for _, routed := range []string{"mentions", "collective", "notify"} {
			if _, present := body[routed]; present {
				t.Errorf("message %d carried %q: a year of mentions arriving "+
					"as live wakes is tens of thousands of turns", i, routed)
			}
		}
		for _, required := range []string{"source", "vendor_id", "author",
			"author_kind", "authored_at"} {
			if body[required] == nil || body[required] == "" {
				t.Errorf("message %d carried no %q, and provenance is what "+
					"tells an imported message from a live one", i, required)
			}
		}
	}
	if !strings.Contains(out, "Nobody was woken") {
		t.Errorf("the run never said so:\n%s", out)
	}
}

// A THREAD SURVIVES THE ROUND TRIP, and the root is DERIVED rather than read
// back — which is what lets a resumed run file a reply under the same root
// the first pass did, without finding it.
func TestAThreadSurvivesTheImport(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap)); err != nil {
		t.Fatalf("import: %v", err)
	}
	root, reply := findImported(t, fake, "C0ENG/1932093300.000200"),
		findImported(t, fake, "C0ENG/1932093360.000300")
	if root.In.ReplyTo != "" {
		t.Errorf("the thread's first message was filed under %q", root.In.ReplyTo)
	}
	want, err := chat.ImportedMessageID("slack", root.In.VendorID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.In.ReplyTo != want.String() {
		t.Errorf("the reply was filed under %q, want the root's derived id %s "+
			"— a thread that loses its root is a range that no longer reads "+
			"as a conversation", reply.In.ReplyTo, want)
	}
}

// THE ORIGINAL INSTANT SURVIVES. Every other instant on this log is the
// broker's, which is what makes one node's copy identical to another's; an
// import that rendered at the instant it was replayed would compress a year
// into an afternoon.
func TestTheOriginalInstantSurvivesTheImport(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap)); err != nil {
		t.Fatalf("import: %v", err)
	}
	for vendorID, want := range map[string]string{
		"C0ENG/1932093300.000200":   "2031-03-24T04:35:00Z",
		"C0ENG/1932093360.000300":   "2031-03-24T04:36:00Z",
		"G0LEADS/1932093300.000200": "2031-03-24T04:35:00Z",
	} {
		got := findImported(t, fake, vendorID).In.AuthoredAt.UTC().Format(
			"2006-01-02T15:04:05Z")
		if got != want {
			t.Errorf("%s was imported as said at %s, want %s", vendorID, got, want)
		}
	}
}

// A SLACK TIMESTAMP IS UNIQUE WITHIN A CHANNEL AND NOT ACROSS A WORKSPACE, so
// the channel is half the vendor id.
//
// The fixture says the same `ts` in two rooms on purpose. Without the channel
// in the id the second one declines as a duplicate of the first and is lost
// with no error anywhere — the worst shape a data-loss bug can have.
func TestASlackTimestampIsOnlyUniqueWithinItsChannel(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap)); err != nil {
		t.Fatalf("import: %v", err)
	}
	eng := findImported(t, fake, "C0ENG/1932093300.000200")
	leads := findImported(t, fake, "G0LEADS/1932093300.000200")
	if eng.ChannelID == leads.ChannelID {
		t.Fatalf("both messages landed in %s", eng.ChannelID)
	}
	if eng.In.Body == leads.In.Body {
		t.Errorf("two different remarks arrived as one: %q", eng.In.Body)
	}
}

// SLACK'S MARKUP BECOMES PROSE, because a year of `<@U0FOUNDER>` is not a
// transcript and the keyword index would rank on the vendor's ids rather than
// on the people named. What it does NOT become is a routed mention — that is
// TestAnImportSendsNothingAWakeCouldBeDerivedFrom's half.
func TestSlackMarkupBecomesProse(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap)); err != nil {
		t.Fatalf("import: %v", err)
	}
	body := findImported(t, fake, "C0ENG/1932093300.000200").In.Body
	for _, want := range []string{"@engineer", "the plan (https://example.com/plan)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the imported body %q never says %q", body, want)
		}
	}
	if strings.Contains(body, "U0ENGINEER") || strings.Contains(body, "<") {
		t.Errorf("the imported body kept Slack's markup: %q", body)
	}
	// AND THE THREE ENTITIES SLACK ESCAPES ARE PUT BACK.
	reply := findImported(t, fake, "C0ENG/1932093360.000300").In.Body
	if !strings.Contains(reply, "Ship & measure") {
		t.Errorf("an escaped ampersand survived as an entity: %q", reply)
	}
}

// AN AUTHOR THE COMPANY CANNOT BE TOLD ABOUT STOPS THE RUN, and the refusal
// is the operator's remedy rather than a report of one.
//
// The three alternatives are each worse, and each irreversibly: dropping the
// message takes a thread's shape with it, a placeholder handle makes "who
// said this" unanswerable in the one store where that is the point, and
// attributing to the credential running the migration has one person saying
// everything in the archive. So nothing is written until the operator says
// who, and the refusal prints the lines they need with the name the vendor
// displayed beside each id.
func TestAnUnmappedAuthorStopsTheImportAndNamesEveryOne(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)

	// No map at all: every author is unmapped.
	_, _, err := importCLI(t, fake, "testdata/chatimport/slack")
	if err == nil {
		t.Fatal("an archive with no author map imported anyway")
	}
	for _, want := range []string{"U0FOUNDER", "U0ENGINEER", "U0STRANGER",
		"authors:", "The Founder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%v", want, err)
		}
	}
	if len(fake.stored) != 0 {
		t.Errorf("%d message(s) were written before the refusal, so the "+
			"archive is now half in", len(fake.stored))
	}

	// EVERY ONE AT ONCE, not the first: an operator fixing one line per
	// run has been made to pay once per person in their workspace.
	_, _, err = importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, "authors:\n  U0FOUNDER: founder\n"))
	if err == nil {
		t.Fatal("a partial author map imported anyway")
	}
	if strings.Contains(err.Error(), "U0FOUNDER") {
		t.Errorf("a mapped author was reported as unmapped:\n%v", err)
	}
	if !strings.Contains(err.Error(), "U0ENGINEER") ||
		!strings.Contains(err.Error(), "U0STRANGER") {
		t.Errorf("the refusal did not name both remaining authors:\n%v", err)
	}
}

// A HANDLE THIS COMPANY NO LONGER EMPLOYS IS STILL AN AUTHOR, and an agent
// seat is recorded as one.
//
// `GET /agents` answers agent seats and no others, so membership in it IS the
// kind — and the fallback is the right way round: a handle that is no seat at
// all is somebody who has left, and people are what a chat archive is mostly
// made of.
func TestAnImportedAuthorsKindComesFromTheCompanysOwnAgentSeats(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap)); err != nil {
		t.Fatalf("import: %v", err)
	}
	for vendorID, want := range map[string]chat.AuthorKind{
		"C0ENG/1932093300.000200":   chat.AuthorHuman, // founder, not an agent seat
		"C0ENG/1932093360.000300":   chat.AuthorAgent, // engineer, an agent seat
		"G0LEADS/1932093300.000200": chat.AuthorHuman, // stranger, no seat at all
	} {
		got := findImported(t, fake, vendorID).In.AuthorKind
		if got != string(want) {
			t.Errorf("%s was imported as a %q, want %q", vendorID, got, want)
		}
	}
}

// AN ARCHIVE CHANNEL WITH NO ROOM IS REFUSED, and nothing is created.
//
// A room's kind is a disclosure decision and its membership is what a private
// room's readability IS: an invented room is either unreachable — including
// by the import itself, which takes the room's own membership gate — or the
// whole conversation published to the company.
func TestAnArchiveChannelWithNoRoomIsRefusedAndNothingIsCreated(t *testing.T) {
	t.Parallel()
	// The rail holds `engineering` and not `leads`.
	fake := newImportFake(t, []map[string]any{
		importRoomRow("c-eng", "engineering", "public"),
	}, []string{"engineer"})

	_, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap))
	if err == nil {
		t.Fatal("an archive naming a room this company has not imported anyway")
	}
	if !strings.Contains(err.Error(), "leads") {
		t.Errorf("the refusal does not name the missing room:\n%v", err)
	}
	if !strings.Contains(err.Error(), "channels:") {
		t.Errorf("the refusal does not print the mapping to add:\n%v", err)
	}
	if len(fake.stored) != 0 {
		t.Errorf("%d message(s) were written for the rooms that DID resolve, "+
			"leaving the archive half in", len(fake.stored))
	}

	// AND THE MAP IS THE WAY THROUGH, so a company that renamed a room on
	// the way across is not made to rename it back.
	fake2 := newImportFake(t, []map[string]any{
		importRoomRow("c-eng", "engineering", "public"),
		importRoomRow("c-mgmt", "management", "private"),
	}, []string{"engineer"})
	if _, _, err := importCLI(t, fake2, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap+
			"channels:\n  leads: management\n")); err != nil {
		t.Fatalf("a mapped channel was still refused: %v", err)
	}
	if got := findImported(t, fake2, "G0LEADS/1932093300.000200"); got.ChannelID != "c-mgmt" {
		t.Errorf("the mapped room's message landed in %q", got.ChannelID)
	}
}

// A MALFORMED ARCHIVE IS REFUSED NAMING THE FILE AND THE LINE.
//
// encoding/json's own error carries a byte offset, which is worse than
// nothing: it looks like something an editor can use and is not. An operator
// handed "the archive is broken" has to read a year of somebody's workspace
// to find out where.
func TestAMalformedArchiveIsRefusedNamingTheFileAndTheLine(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		dir  string
		file string
		line string
	}{
		{"slack", "testdata/chatimport/broken-slack",
			"engineering/2031-03-24.json", ":3"},
		{"mattermost", "testdata/chatimport/broken-mattermost",
			"mattermost.jsonl", ":4"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fake := slackFake(t)
			// THE MAP IS IRRELEVANT HERE and is passed anyway: the
			// archive is read before it is resolved, so a file this
			// build cannot finish reading must refuse whatever else
			// the operator got right.
			_, _, err := importCLI(t, fake, arm.dir,
				"-map", writeImportMap(t, slackMap))
			if err == nil {
				t.Fatal("a malformed archive was read without complaint")
			}
			if !strings.Contains(err.Error(), arm.file) {
				t.Errorf("the refusal does not name the file:\n%v", err)
			}
			if !strings.Contains(err.Error(), arm.line) {
				t.Errorf("the refusal does not name the line (want %s):\n%v",
					arm.line, err)
			}
			if len(fake.stored) != 0 {
				t.Errorf("%d message(s) were written from a file this build "+
					"could not finish reading", len(fake.stored))
			}
		})
	}
}

// A SLACK DIRECTORY NEITHER LIST NAMES IS REFUSED rather than imported with a
// guessed kind.
//
// Slack states a room's privacy only in channels.json and groups.json. A
// directory in neither is a room whose privacy nothing in the archive
// declares, and the safe-looking default — public — is the disclosure that
// cannot be taken back.
func TestASlackDirectoryNoListNamesIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("users.json", `[{"id":"U0FOUNDER","name":"founder"}]`)
	write("channels.json", `[{"id":"C0ENG","name":"engineering"}]`)
	write("engineering/2031-03-24.json",
		`[{"user":"U0FOUNDER","text":"hello","ts":"1932093300.000200"}]`)
	write("board-private/2031-03-24.json",
		`[{"user":"U0FOUNDER","text":"the offer","ts":"1932093300.000300"}]`)

	fake := slackFake(t)
	_, _, err := importCLI(t, fake, dir, "-map", writeImportMap(t, slackMap))
	if err == nil {
		t.Fatal("a room whose privacy nothing declares was imported anyway")
	}
	if !strings.Contains(err.Error(), "board-private") {
		t.Errorf("the refusal does not name the directory:\n%v", err)
	}
	if len(fake.stored) != 0 {
		t.Errorf("%d message(s) were written", len(fake.stored))
	}
}

// A MATTERMOST BULK EXPORT IMPORTS WITH ITS THREADS AND INSTANTS, and its
// posts get an identity the format does not give them.
//
// The format carries no post id at all — it is written to be imported into a
// fresh installation, which mints its own — so the identity is derived from
// (team, channel, instant, user). That is the only tuple stable across two
// passes over one file, which is what makes this vendor's import re-runnable
// on the same terms as Slack's.
func TestAMattermostBulkExportImportsWithItsThreadsAndInstants(t *testing.T) {
	t.Parallel()
	fake := newImportFake(t, []map[string]any{
		importRoomRow("c-eng", "engineering", "public"),
	}, []string{"engineer"})
	mapPath := writeImportMap(t, "authors:\n  founder: founder\n  engineer: engineer\n")

	if _, _, err := importCLI(t, fake, "testdata/chatimport/mattermost",
		"-map", mapPath); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(fake.stored) != 2 {
		t.Fatalf("the bulk export wrote %d message(s), want 2", len(fake.stored))
	}
	root := findImported(t, fake, "nimbus/engineering/1932093300000/founder")
	reply := findImported(t, fake, "nimbus/engineering/1932093360000/engineer")
	if root.In.Source != "mattermost" {
		t.Errorf("the provenance says %q", root.In.Source)
	}
	if got := root.In.AuthoredAt.UTC().Format("2006-01-02T15:04:05Z"); got !=
		"2031-03-24T04:35:00Z" {
		t.Errorf("the root was imported as said at %s", got)
	}
	want, err := chat.ImportedMessageID("mattermost", root.In.VendorID)
	if err != nil {
		t.Fatal(err)
	}
	if reply.In.ReplyTo != want.String() {
		t.Errorf("a nested reply was filed under %q, want %s",
			reply.In.ReplyTo, want)
	}

	// AND A SECOND PASS WRITES NOTHING, on the derived identity.
	if _, _, err := importCLI(t, fake, "testdata/chatimport/mattermost",
		"-map", mapPath); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(fake.stored) != 2 {
		t.Errorf("a second pass over a bulk export left %d message(s), want 2",
			len(fake.stored))
	}
}

// A CHANNEL KIND THIS BUILD CANNOT READ IS REFUSED, never defaulted.
//
// Mattermost's channel type is an open enum. Reading an unknown one as public
// is the failure that cannot be taken back, and reading it as private hides a
// room somebody expected to find.
func TestAMattermostChannelKindThisBuildCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mattermost.jsonl")
	if err := os.WriteFile(path, []byte(
		`{"type":"version","version":1}`+"\n"+
			`{"type":"channel","channel":{"team":"nimbus","name":"engineering","type":"X"}}`+"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	fake := slackFake(t)
	_, _, err := importCLI(t, fake, dir, "-map", writeImportMap(t, slackMap))
	if err == nil {
		t.Fatal("a channel kind this build cannot read was accepted")
	}
	if !strings.Contains(err.Error(), "mattermost.jsonl:2") {
		t.Errorf("the refusal does not name the file and line:\n%v", err)
	}
	if !strings.Contains(err.Error(), "type: X") {
		t.Errorf("the refusal does not name the kind it could not read:\n%v", err)
	}
}

// -check RESOLVES EVERYTHING AND WRITES NOTHING, which is what lets an
// operator find every unmapped author and every missing room before a node is
// touched at all.
func TestACheckRunResolvesEverythingAndWritesNothing(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	out, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", writeImportMap(t, slackMap), "-check")
	if err != nil {
		t.Fatalf("-check: %v", err)
	}
	if len(fake.stored) != 0 {
		t.Errorf("-check wrote %d message(s)", len(fake.stored))
	}
	for _, want := range []string{"3 message(s) in 2 room(s)",
		"engineering", "c-eng", "nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("-check never reported %q:\n%s", want, out)
		}
	}
	// AND WHAT IT DID NOT IMPORT IS ACCOUNTED FOR. The fixture holds a
	// join notice and an upload with no text; a message that is not in the
	// company's rooms afterwards has to be visible somewhere.
	if !strings.Contains(out, "skipped") {
		t.Errorf("-check never accounted for what it skipped:\n%s", out)
	}
}

// -limit IS A SLICE, AND A SLICE RESUMES. An operator taking a trial run over
// a large archive reads it back and then finishes the job, and the second run
// writes only what the first did not.
func TestALimitedImportIsASliceThatResumes(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	mapPath := writeImportMap(t, slackMap)

	out, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", mapPath, "-limit", "1")
	if err != nil {
		t.Fatalf("-limit 1: %v", err)
	}
	if len(fake.stored) != 1 {
		t.Fatalf("-limit 1 wrote %d message(s)", len(fake.stored))
	}
	if !strings.Contains(out, "Re-run to continue") {
		t.Errorf("a limited run never said how to finish:\n%s", out)
	}

	if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
		"-map", mapPath); err != nil {
		t.Fatalf("the rest: %v", err)
	}
	if len(fake.stored) != 3 {
		t.Errorf("finishing a limited import left %d message(s), want 3",
			len(fake.stored))
	}
	if fake.declined != 1 {
		t.Errorf("the finishing run re-presented %d message(s), want the 1 "+
			"the slice had already written", fake.declined)
	}
}

// AN EXPORT THIS BUILD DOES NOT RECOGNISE IS REFUSED NAMING WHAT IT LOOKED
// FOR. The vendor is detected rather than declared, because the vendor is
// half of every message's identity — an archive imported under the wrong
// source would import cleanly a second time, as a second copy.
func TestADirectoryThatIsNotAnExportIsRefusedNamingWhatWasLookedFor(t *testing.T) {
	t.Parallel()
	fake := slackFake(t)
	_, _, err := importCLI(t, fake, t.TempDir(), "-map", writeImportMap(t, slackMap))
	if err == nil {
		t.Fatal("an empty directory was read as an export")
	}
	for _, want := range []string{"users.json", ".jsonl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never names %q:\n%v", want, err)
		}
	}
}

// findImported is one stored message, by the pair that identifies it.
func findImported(t *testing.T, fake *importFake, vendorID string) importFakeRow {
	t.Helper()
	for _, row := range fake.stored {
		if row.In.VendorID == vendorID {
			return row
		}
	}
	held := make([]string, 0, len(fake.stored))
	for _, row := range fake.stored {
		held = append(held, row.In.VendorID)
	}
	t.Fatalf("no message with vendor id %q was imported; the node holds %s",
		vendorID, fmt.Sprint(held))
	return importFakeRow{}
}

// A PRIVATE CHANNEL MAY NOT WIDEN ON THE WAY ACROSS.
//
// This is the disclosure every other refusal in this command is shaped around,
// arriving by the one door they leave open: the archive's channel resolves,
// the authors are all mapped, every body fits — and the room it resolves to
// is one the whole company reads. Importing there publishes a year of a
// private conversation at once, on every node, and no gesture takes it back.
//
// A kind this build cannot classify counts as NOT restricted, for the reason
// the write path refuses an unclassifiable room outright: an open enum read
// two-valued falls out as the permissive answer.
func TestAPrivateArchiveChannelMayNotLandInARoomTheCompanyReads(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		kind string
	}{
		{"public", "public"},
		{"a unit's own room", "unit"},
		{"a kind this build does not know", "broadcast-from-a-newer-build"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fake := newImportFake(t, []map[string]any{
				importRoomRow("c-eng", "engineering", "public"),
				importRoomRow("c-leads", "leads", arm.kind),
			}, []string{"engineer"})

			_, _, err := importCLI(t, fake, "testdata/chatimport/slack",
				"-map", writeImportMap(t, slackMap))
			if err == nil {
				t.Fatal("a private channel was imported into a room the whole " +
					"company reads")
			}
			if !strings.Contains(err.Error(), "leads") ||
				!strings.Contains(err.Error(), "private") {
				t.Errorf("the refusal does not say which room widened:\n%v", err)
			}
			if len(fake.stored) != 0 {
				t.Errorf("%d message(s) were written before the refusal",
					len(fake.stored))
			}
		})
	}

	// AND A PRIVATE ROOM IS THE WAY THROUGH — including a direct
	// conversation, whose readership is its participants.
	for _, kind := range []string{"private", "dm", "group"} {
		fake := newImportFake(t, []map[string]any{
			importRoomRow("c-eng", "engineering", "public"),
			importRoomRow("c-leads", "leads", kind),
		}, []string{"engineer"})
		if _, _, err := importCLI(t, fake, "testdata/chatimport/slack",
			"-map", writeImportMap(t, slackMap)); err != nil {
			t.Errorf("a private channel into a %s room was refused: %v", kind, err)
		}
	}
}
