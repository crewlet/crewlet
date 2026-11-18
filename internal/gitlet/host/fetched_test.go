package host_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// unitCase is a solo fleet of o holding gitlettest.RepoName as a `visibility:
// unit` repository and `open` as a company one, each with one push of main by
// ceo; it returns the case, the fleet and the pushed commit.
func unitCase(t *testing.T, o gitlettest.SoloOptions) (*gitlettest.Case, *gitlettest.SoloFleet, string) {
	t.Helper()
	f := gitlettest.SoloNode(t, o)
	unit := gitlettest.Settings()
	unit.Visibility = gitlet.VisibilityUnit
	f.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: unit},
		gitlet.Declared{Name: "open", Settings: gitlettest.Settings()})
	c := &gitlettest.Case{T: t, Fleet: f, Ctx: t.Context()}
	p := gitlettest.NewPerson(t, f, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	if res := p.Push(c.Ctx, work, f.Nodes()[0].URL()+"/git/open", "main"); res.Err != nil {
		t.Fatalf("push open: %s", res)
	}
	return c, f, head
}

// fetchRounds counts the fetch requests node 0 has answered, one per
// stateless negotiation round (crewlet.code.fetch.duration).
func fetchRounds(c *gitlettest.Case) uint64 {
	n, _ := observed(c.Fleet.Nodes()[0].Metrics(), metrics.CodeFetchDuration, map[string]string{"command": "fetch"})
	return n
}

// A unit repository's fetch is recorded: one read per fetch that ended well
// with a pack, as its credential and bound person (none for an unbound token),
// its bytes what the client read. Nothing else is: a company repository's
// clone, an `ls-refs`, a negotiation round that sent no object (so a
// multi-round stateless fetch is ONE record, either protocol), or a fetch the
// node ended after its pack began.
//
// Mutation: record every request of a fetch, drop the visibility check, or
// record `ls-refs`, and a record too many appears.
func TestAUnitRepositorysFetchIsRecorded(t *testing.T) {
	c, f, head := unitCase(t, gitlettest.SoloOptions{})
	repo := c.Rows().Repo.ID
	seen := func() int { return len(f.Fetched()) }

	t.Run("a person's clone is one record", func(t *testing.T) {
		before := seen()
		gitlettest.NewPerson(t, f, "ceo").Clone(c.URL(0))
		got := f.Fetched()[before:]
		if len(got) != 1 {
			t.Fatalf("a clone was recorded %d times: %+v", len(got), got)
		}
		want := types.CodeFetched{RepoID: repo, RepoName: gitlettest.RepoName,
			PrincipalKind: types.CodeReaderOperator, Principal: iam.TokenLogin("ceo"), Seat: "ceo",
			Via: types.FetchedViaGit, Reads: 1, Bytes: got[0].Bytes}
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
		gitlettest.NewPerson(t, f, "ceo").Clone(f.Nodes()[0].URL() + "/git/open")
		if got := f.Fetched()[before:]; len(got) != 0 {
			t.Fatalf("an ls-refs and a company repository's clone were recorded: %+v", got)
		}
	})

	// A diverged client: a round offering only unknown commits gets
	// acknowledgments and no object, the `done` round the pack; two
	// requests, one record.
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

// A fetch the node ended is no record, even once its pack had begun: its
// client was not sent the repository whole. The stand-in upload-pack writes a
// v2 pack section and its first packet, then nothing until the stall ends it.
//
// The short stall (host.Timing.Stall) must hit only the stand-in, so the
// commit is seeded by an engine write, never a push: a real receive-pack is
// quiet until its hook answers, which on a loaded runner outlasts the stall
// and fails a push that landed.
//
// Mutation: seed main with a push, and the premise that the node served no
// push fails.
func TestAFetchTheNodeEndedIsNoRecord(t *testing.T) {
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
*" upload-pack "*) cat >/dev/null; printf '000dpackfile\n0009\001PACK'; exec sleep 600 ;;
esac`)
	timing := fast
	timing.Stall = 300 * time.Millisecond
	unit := gitlettest.Settings()
	unit.Visibility = gitlet.VisibilityUnit
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Shape: gitlettest.Shape{Timing: timing}}, unit)
	f := c.Fleet.(*gitlettest.SoloFleet)
	seeded, err := f.Write(c.Ctx, files(c.Rows().Repo.ID, pat(), "refs/heads/main", "", "", text("README.md", "one\n")))
	if err != nil || seeded.Outcome != statelog.OutcomeApplied || seeded.Commit == "" {
		t.Fatalf("the premise: main's first commit, an engine write, answered %+v, %v", seeded, err)
	}
	n := c.Fleet.Nodes()[0]
	if pushes, _ := observed(n.Metrics(), metrics.CodePushDuration, nil); pushes != 0 {
		t.Fatalf("the premise: the node served a push (%d in all), a real receive-pack held to the stand-in's stall",
			pushes)
	}
	before := len(f.Fetched())
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost, n.URL()+"/git/"+gitlettest.RepoName+
		"/git-upload-pack", strings.NewReader(fetchWants([]string{seeded.Commit})))
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
	// Only that the node cut the answer is asked, not what the client read.
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
