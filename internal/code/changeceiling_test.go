package code_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// A CHANGE'S WRITE IS HELD TO WHOEVER ITS SEAT'S TURN WORKS FOR (M4-decisions
// D30 under M3-decisions U40 (a)): a colleague seat must hold the write's
// least access on the repository and nothing beyond it — the relation, who
// owns the change or wrote it, is the seat's own to meet — and nobody (a
// stranger, a person who may not act) steers only the seat's own work: a
// change it owns, or any change of a repository it owns. A write for nobody
// in particular, nil, is the seat's alone.
//
// Mutations: the own-change spare dropped — plat-lead's own change refused
// for nobody, red; the own-repository spare dropped — dev's scratch
// repository's change refused for nobody, red; nobody admitted — Platform's
// change lands for nobody, red; the colleague's access compared against read
// whatever the write needs — reader admitted to a review, red.
func TestAChangeWriteIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	r := testChart()
	platform := ownedBy("platform", code.OwnerUnit, "Platform")
	scratch := ownedBy("dev.notes", code.OwnerSeat, "dev")
	seat := func(handle string) *code.WriteCeiling {
		return &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: handle}
	}
	nobody := &code.WriteCeiling{Kind: code.WriteCeilingNobody}
	for _, tc := range []struct {
		name    string
		asker   string
		ceiling *code.WriteCeiling
		repo    code.RepoRef
		own     bool
		need    code.Access
		// says is what a refusal says, "" for a write that is made.
		says string
	}{
		{"for nobody in particular", "plat-lead", nil, platform, false, code.AccessReview, ""},
		{"its own change, for nobody", "plat-lead", nobody, platform, true, code.AccessRead, ""},
		{"a change of its own repository, for nobody", "dev", nobody, scratch, false, code.AccessRead, ""},
		{"another's change, for nobody", "plat-lead", nobody, platform, false, code.AccessRead,
			"who may change nothing of this company's code"},
		{"a change of another seat's repository, for nobody", "plat-lead", nobody, scratch, false,
			code.AccessRead, "who may change nothing of this company's code"},
		{"a read's write, for a colleague who reads", "plat-lead", seat("vp-sales"), platform, false,
			code.AccessRead, ""},
		{"a review, for a colleague who only reads", "plat-lead", seat("reader"), platform, false,
			code.AccessReview, "this work is for seat reader, whose ask started this turn, which holds read on " +
				"this repository"},
		{"a review, for a colleague who writes", "plat-lead", seat("dev"), platform, false, code.AccessReview, ""},
		{"a write, for a colleague the chart lacks", "plat-lead", seat("ghost"), platform, false, code.AccessRead,
			"holds none on this repository"},
	} {
		got := code.ChangeCeilingRefusal(tc.ceiling, agent(tc.asker), tc.repo, tc.own, tc.need, r)
		switch {
		case tc.says == "" && got != "":
			t.Errorf("%s: refused %q, want made", tc.name, got)
		case tc.says != "" && !strings.Contains(got, tc.says):
			t.Errorf("%s: told %q, want a refusal saying %q", tc.name, got, tc.says)
		}
	}
}

// A CHANGE'S WRITE IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN, where it is
// decided: the committer and the change writer judge the ceiling a seat's
// write carries (ChangeProposal.Ceiling, ConversationWrite.Ceiling) from their
// own chart, once the seat itself may make the write, and a refusal appends
// nothing. cto — Engineering's lead, who maintains web — retitles dev's change
// for reader, who reads web, and not for a colleague the chart lacks nor for
// nobody; dev retitles its own for nobody. dev opens a change of its own
// namespace for nobody, and of release only for a colleague who reads web.
// plat-lead reviews dev's change for dev — its author, who could never review
// it — and not for reader, who reads and does not review. reader comments for
// vp-sales and not for nobody; dev comments on its own for nobody. A ceiling
// on a write no seat asks, or naming the seat that asks, is refused before it
// is queued.
//
// Mutations: either decide skipping the ceiling — the refusals land, red; the
// open's own-namespace spare dropped — dev's namespace open refused, red; the
// conversation holding a review at read — reader's review lands, red.
func TestAChangeWriteIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	seat := func(handle string) *code.WriteCeiling {
		return &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: handle}
	}
	nobody := &code.WriteCeiling{Kind: code.WriteCeilingNobody}

	version := int64(1)
	for _, tc := range []struct {
		name    string
		asker   code.Principal
		ceiling *code.WriteCeiling
		says    string
	}{
		{"the maintainer for a colleague who reads", cto, seat("reader"), ""},
		{"the maintainer for a colleague the chart lacks", cto, seat("ghost"),
			"this work is for seat ghost, whose ask started this turn, which holds none on this repository"},
		{"the maintainer for nobody", cto, nobody, "who may change nothing of this company's code"},
		{"its owner for nobody", dev, nobody, ""},
	} {
		before := n.end()
		p := retitled(repo, change, tc.asker, version, "by "+tc.name)
		p.Ceiling = tc.ceiling
		res, err := c.ProposeChange(ctx, p)
		if tc.says != "" {
			requireRefused(t, n, before, res, err, code.ChangeRefusalNotPermitted, tc.says)
			continue
		}
		requireChanged(t, res, err)
		version = res.Version
	}

	own := code.SeatNamespaceRoot + "dev/retry"
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(own, code.ZeroOID, oidOf("S"))))
	before := n.end()
	p := opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = nobody
	res, err := c.ProposeChange(ctx, p)
	requireRefused(t, n, before, res, err, code.ChangeRefusalNotPermitted,
		"may not open a change from refs/heads/release for them")
	p = opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = seat("ghost")
	res, err = c.ProposeChange(ctx, p)
	requireRefused(t, n, before, res, err, code.ChangeRefusalNotPermitted, "which holds none on this repository")
	p = opening(repo, own, mainRef, dev)
	p.Ceiling = nobody
	res, err = c.ProposeChange(ctx, p)
	requireChanged(t, res, err)
	p = opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = seat("vp-sales")
	res, err = c.ProposeChange(ctx, p)
	requireChanged(t, res, err)

	before = n.end()
	review := reviewing(repo, change, platLead, 1, code.VerdictApprove, nil)
	review.Ceiling = seat("reader")
	got, err := w.Converse(ctx, review)
	requireConversationRefused(t, n, before, got, err, code.ConversationRefusalNotPermitted,
		"that needs review, which reader does not hold")
	review = reviewing(repo, change, platLead, 1, code.VerdictApprove, nil)
	review.Ceiling = seat("dev")
	got, err = w.Converse(ctx, review)
	requireReviewed(t, got, err)

	before = n.end()
	comment := saying(repo, change, reader, "", "for a stranger")
	comment.Ceiling = nobody
	got, err = w.Converse(ctx, comment)
	requireConversationRefused(t, n, before, got, err, code.ConversationRefusalNotPermitted,
		"seat reader may not write web!1's conversation for them")
	comment = saying(repo, change, reader, "", "for a colleague")
	comment.Ceiling = seat("vp-sales")
	got, err = w.Converse(ctx, comment)
	requireConversed(t, got, err)
	comment = saying(repo, change, dev, "", "its own, for nobody")
	comment.Ceiling = nobody
	got, err = w.Converse(ctx, comment)
	requireConversed(t, got, err)

	landed := landing(repo, change, 1, oidOf("B"), mainRef, oidOf("A"))
	landed.Ceiling = nobody
	self := retitled(repo, change, dev, version, "for itself")
	self.Ceiling = seat("dev")
	person := saying(repo, change, ada, "", "a person's own")
	person.Ceiling = seat("dev")
	for name, err := range map[string]error{
		"a landing, which no seat asks":       landed.Validate(),
		"a write made for its own asker":      self.Validate(),
		"a person's write, which none bounds": person.Validate(),
	} {
		if !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), "ceiling") {
			t.Errorf("%s carrying a ceiling: %v, want ErrInvalidProposal naming the ceiling", name, err)
		}
	}
}
