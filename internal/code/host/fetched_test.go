package host_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// unitCase is a solo fleet of o holding codetest.RepoName as a `visibility:
// unit` repository and `open` as a company one, both with one push of main
// by ceo, the fleet itself and the pushed commit.
func unitCase(t *testing.T, o codetest.SoloOptions) (*codetest.Case, *codetest.SoloFleet, string) {
	t.Helper()
	f := codetest.SoloNode(t, o)
	unit := codetest.Settings()
	unit.Visibility = code.VisibilityUnit
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: unit},
		code.Declared{Name: "open", Settings: codetest.Settings()})
	c := &codetest.Case{T: t, Fleet: f, Ctx: t.Context()}
	p := codetest.NewPerson(t, f, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	if res := p.Push(c.Ctx, work, f.Nodes()[0].URL()+"/git/open", "main"); res.Err != nil {
		t.Fatalf("push open: %s", res)
	}
	return c, f, head
}

// fetchRounds counts the fetch requests node 0 has answered — one per round a
// stateless negotiation makes (crewlet.code.fetch.duration).
func fetchRounds(c *codetest.Case) uint64 {
	n, _ := observed(c.Fleet.Nodes()[0].Metrics(), metrics.CodeFetchDuration, map[string]string{"command": "fetch"})
	return n
}

// A UNIT REPOSITORY'S FETCH IS RECORDED: one record for every fetch whose
// request ended well and whose answer carried a pack — as the credential that
// fetched and the person it is bound to (none for a token bound to nobody),
// its bytes what the client read — and none for anything else: a company
// repository's clone, an `ls-refs`, a negotiation round that sent no object
// (so a fetch negotiated over several stateless rounds is ONE record, under
// either protocol), and a fetch the node ended after its pack began.
//
// Mutation: record every request a fetch command makes, and the negotiation
// rounds are records; drop the visibility check, and the company repository's
// clone is one; record `ls-refs` too, and it is one.
func TestAUnitRepositorysFetchIsRecorded(t *testing.T) {
	c, f, head := unitCase(t, codetest.SoloOptions{})
	repo := c.Rows().Repo.ID
	seen := func() int { return len(f.Fetched()) }

	t.Run("a person's clone is one record", func(t *testing.T) {
		before := seen()
		codetest.NewPerson(t, f, "ceo").Clone(c.URL(0))
		got := f.Fetched()[before:]
		if len(got) != 1 {
			t.Fatalf("a clone was recorded %d times: %+v", len(got), got)
		}
		want := types.CodeFetched{RepoID: repo, RepoName: codetest.RepoName,
			PrincipalKind: types.CodeReaderOperator, Principal: iam.TokenLogin("ceo"), Seat: "ceo",
			Via: types.FetchedViaGit, Bytes: got[0].Bytes}
		if got[0] != want || got[0].Bytes <= 0 {
			t.Errorf("the clone is recorded as %+v, want %+v with the bytes it was sent", got[0], want)
		}
	})

	t.Run("a fetch's bytes are what its client read", func(t *testing.T) {
		before := seen()
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
		if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "packfile") {
			t.Fatalf("the fetch answered %d: %.200q", resp.StatusCode, answer)
		}
		got := f.Fetched()[before:]
		if len(got) != 1 || got[0].Bytes != int64(len(answer)) {
			t.Fatalf("the fetch is recorded as %+v, want one record of the %d bytes the client read",
				got, len(answer))
		}
	})

	t.Run("a token bound to nobody names no seat", func(t *testing.T) {
		before := seen()
		post(t, c, 0, "", "git-upload-pack", v2, fetchWants([]string{head}))
		got := f.Fetched()[before:]
		if len(got) != 1 || got[0].Principal != iam.TokenLogin("ci") || got[0].Seat != "" {
			t.Fatalf("the unbound token's fetch is recorded as %+v, want it by its credential, bound to nobody",
				got)
		}
	})

	t.Run("an ls-refs and a company repository's clone are none", func(t *testing.T) {
		before := seen()
		post(t, c, 0, "ceo", "git-upload-pack", v2, lsRefs())
		codetest.NewPerson(t, f, "ceo").Clone(f.Nodes()[0].URL() + "/git/open")
		if got := f.Fetched()[before:]; len(got) != 0 {
			t.Fatalf("an ls-refs and a company repository's clone were recorded: %+v", got)
		}
	})

	// A DIVERGED CLIENT'S NEGOTIATION: a round offering only commits the
	// repository does not hold is answered with acknowledgments and no
	// object, and the round that says `done` with the pack — two requests,
	// one fetch, one record.
	for _, proto := range []struct {
		name, protocol, round, done string
	}{
		{"over protocol v2", v2,
			pkt("command=fetch\n") + "0001" + pkt("no-progress\n") + pkt("want "+head+"\n") +
				pkt("have "+randomOID()+"\n") + "0000",
			fetchWants([]string{head})},
		{"over protocol v0", "",
			pkt("want "+head+" multi_ack_detailed side-band-64k ofs-delta\n") + "0000" +
				pkt("have "+randomOID()+"\n") + "0000",
			pkt("want "+head+" multi_ack_detailed side-band-64k ofs-delta\n") + "0000" +
				pkt("have "+randomOID()+"\n") + pkt("done\n")},
	} {
		t.Run("a negotiation is one record "+proto.name, func(t *testing.T) {
			before, rounds := seen(), fetchRounds(c)
			resp, answer := post(t, c, 0, "ceo", "git-upload-pack", proto.protocol, proto.round)
			if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "NAK") {
				t.Fatalf("the round answered %d: %.200q", resp.StatusCode, answer)
			}
			if got := f.Fetched()[before:]; len(got) != 0 {
				t.Fatalf("a negotiation round that sent no object was recorded: %+v", got)
			}
			resp, answer = post(t, c, 0, "ceo", "git-upload-pack", proto.protocol, proto.done)
			if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "PACK") {
				t.Fatalf("the round saying done answered %d: %.200q", resp.StatusCode, answer)
			}
			if got := fetchRounds(c) - rounds; got != 2 {
				t.Fatalf("the premise: the negotiation took %d requests, want its two rounds", got)
			}
			if got := f.Fetched()[before:]; len(got) != 1 || got[0].Bytes != int64(len(answer)) {
				t.Fatalf("the negotiation is recorded as %+v, want one record of its pack's answer", got)
			}
		})
	}
}

// A FETCH THE NODE ENDED IS NO RECORD, even once its pack had begun: its
// request did not end well, so its client was not sent the repository whole.
// The stand-in upload-pack writes a v2 answer's pack section and its pack's
// first packet, and then nothing until the stall ends it.
func TestAFetchTheNodeEndedIsNoRecord(t *testing.T) {
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
*" upload-pack "*) cat >/dev/null; printf '000dpackfile\n0009\001PACK'; exec sleep 600 ;;
esac`)
	timing := fast
	timing.Stall = 300 * time.Millisecond
	c, f, head := unitCase(t, codetest.SoloOptions{GitPath: fake, Shape: codetest.Shape{Timing: timing}})
	before := len(f.Fetched())
	n := c.Fleet.Nodes()[0]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost, n.URL()+"/git/"+codetest.RepoName+
		"/git-upload-pack", strings.NewReader(fetchWants([]string{head})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
	resp, err := n.Client().Do(req)
	if err != nil {
		t.Fatalf("POST git-upload-pack: %v", err)
	}
	// CUT SHORT by the node, which is the point: what the client read is not
	// asked, only that it was cut.
	_, cut := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || cut == nil {
		t.Fatalf("the premise: the fetch's answer had begun with its pack, so it is answered 200 and cut — "+
			"it answered %d, cut %v", resp.StatusCode, cut)
	}
	if got := f.Fetched()[before:]; len(got) != 0 {
		t.Fatalf("a fetch the node ended was recorded: %+v", got)
	}
}
