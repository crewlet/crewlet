package gitlet_test

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHANGE'S WRITE IS HELD TO WHOEVER ITS SEAT'S TURN WORKS FOR (M4-decisions
// D30 under M3-decisions U40 (a)): a colleague must hold the write's least access
// on the repository and nothing beyond it (owning or authoring the change is the
// seat's own to meet); nobody (a stranger, a person who may not act) steers only
// the seat's own work, a change it owns or any change of a repository it owns. A
// nil ceiling is the seat's alone.
//
// Mutations: drop the own-change or own-repository spare (that write refused for
// nobody), admit nobody (Platform's change lands), or compare the colleague's
// access against read (reader admitted to a review): each red.
func TestAChangeWriteIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	r := testChart()
	platform := ownedBy("platform", gitlet.OwnerUnit, "Platform")
	scratch := ownedBy("dev.notes", gitlet.OwnerSeat, "dev")
	seat := func(handle string) *gitlet.WriteCeiling {
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: handle}
	}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	for _, tc := range []struct {
		name    string
		asker   string
		ceiling *gitlet.WriteCeiling
		repo    gitlet.RepoRef
		own     bool
		need    gitlet.Access
		// says is what a refusal says, "" for a write that is made.
		says string
	}{
		{"for nobody in particular", "plat-lead", nil, platform, false, gitlet.AccessReview, ""},
		{"its own change, for nobody", "plat-lead", nobody, platform, true, gitlet.AccessRead, ""},
		{"a change of its own repository, for nobody", "dev", nobody, scratch, false, gitlet.AccessRead, ""},
		{"another's change, for nobody", "plat-lead", nobody, platform, false, gitlet.AccessRead,
			"who may change nothing of this company's code"},
		{"a change of another seat's repository, for nobody", "plat-lead", nobody, scratch, false,
			gitlet.AccessRead, "who may change nothing of this company's code"},
		{"a read's write, for a colleague who reads", "plat-lead", seat("vp-sales"), platform, false,
			gitlet.AccessRead, ""},
		{"a review, for a colleague who only reads", "plat-lead", seat("reader"), platform, false,
			gitlet.AccessReview, "this work is for seat reader, whose ask started this turn, which holds read on " +
				"this repository"},
		{"a review, for a colleague who writes", "plat-lead", seat("dev"), platform, false, gitlet.AccessReview, ""},
		{"a write, for a colleague the chart lacks", "plat-lead", seat("ghost"), platform, false, gitlet.AccessRead,
			"holds none on this repository"},
	} {
		got := gitlet.ChangeCeilingRefusal(tc.ceiling, agent(tc.asker), tc.repo, tc.own, tc.need, r)
		switch {
		case tc.says == "" && got != "":
			t.Errorf("%s: refused %q, want made", tc.name, got)
		case tc.says != "" && !strings.Contains(got, tc.says):
			t.Errorf("%s: told %q, want a refusal saying %q", tc.name, got, tc.says)
		}
	}
}

// A CHANGE'S WRITE IS DECIDED AGAIN FOR WHOEVER WOKE THE TURN, where it is decided:
// the committer and the change writer judge a seat's write's ceiling
// (ChangeProposal.Ceiling, ConversationWrite.Ceiling) on their own chart, once the
// seat itself may make the write, and a refusal appends nothing. plat-lead reviews
// dev's change for dev, its author, who could never review it itself, and not for
// reader, who only reads; dev's own namespace and own change are its own work, made
// for nobody. A ceiling on a write no seat asks, or naming its own asker, is
// refused before it is queued.
//
// Mutations: either decide skipping the ceiling (the refusals land), the open's
// own-namespace spare dropped, or the conversation holding a review at read
// (reader's review lands): each red.
func TestAChangeWriteIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	seat := func(handle string) *gitlet.WriteCeiling {
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: handle}
	}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}

	version := int64(1)
	for _, tc := range []struct {
		name    string
		asker   gitlet.Principal
		ceiling *gitlet.WriteCeiling
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
			requireRefused(t, n, before, res, err, gitlet.ChangeRefusalNotPermitted, tc.says)
			continue
		}
		requireChanged(t, res, err)
		version = res.Version
	}

	own := gitlet.SeatNamespaceRoot + "dev/retry"
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(own, gitlet.ZeroOID, oidOf("S"))))
	before := n.end()
	p := opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = nobody
	res, err := c.ProposeChange(ctx, p)
	requireRefused(t, n, before, res, err, gitlet.ChangeRefusalNotPermitted,
		"may not open a change from refs/heads/release for them")
	p = opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = seat("ghost")
	res, err = c.ProposeChange(ctx, p)
	requireRefused(t, n, before, res, err, gitlet.ChangeRefusalNotPermitted, "which holds none on this repository")
	p = opening(repo, own, mainRef, dev)
	p.Ceiling = nobody
	res, err = c.ProposeChange(ctx, p)
	requireChanged(t, res, err)
	p = opening(repo, releaseRef, mainRef, dev)
	p.Ceiling = seat("vp-sales")
	res, err = c.ProposeChange(ctx, p)
	requireChanged(t, res, err)

	before = n.end()
	review := reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil)
	review.Ceiling = seat("reader")
	got, err := w.Converse(ctx, review)
	requireConversationRefused(t, n, before, got, err, gitlet.ConversationRefusalNotPermitted,
		"that needs review, which reader does not hold")
	review = reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil)
	review.Ceiling = seat("dev")
	got, err = w.Converse(ctx, review)
	requireReviewed(t, got, err)

	before = n.end()
	comment := saying(repo, change, reader, "", "for a stranger")
	comment.Ceiling = nobody
	got, err = w.Converse(ctx, comment)
	requireConversationRefused(t, n, before, got, err, gitlet.ConversationRefusalNotPermitted,
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
		if !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), "ceiling") {
			t.Errorf("%s carrying a ceiling: %v, want ErrInvalidProposal naming the ceiling", name, err)
		}
	}
}

// A CODE WRITE WHOSE CARRIER AND CEILING DISAGREE IS REFUSED (ADR-0109) by every node
// deciding one (committer, change writer, the Gitlet node's EngineWrite.Validate and
// WrittenForOn), naming written_for and appending nothing: the engine composes both
// from one bound ceiling, so a disagreement means a site dropped one. A seat the
// deciding chart does not place, with no ceiling, is refused as a colleague's but
// says the chart does not place it, so a lagging chart reads as one. A carrier no
// ceiling makes right, a stranger's or one on a person's own write, is refused
// everywhere; a ceiling with no carrier (an older sender's) is decided by its ceiling.
//
// Mutations: skip the agreement or the chart's half of a decide, admit a stranger's
// carrier or a credential with no ceiling, refuse a ceiling with no carrier or an
// unplaced seat as an agent's, or admit an asker-set payload carrier: each red.
func TestACodeWriteWhoseCarrierAndCeilingDisagreeIsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	seat := func(handle string) *gitlet.WriteCeiling {
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: handle}
	}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	forSeat := func(handle string) types.WrittenFor {
		return types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle})
	}
	credential := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantGitlet}})
	stranger := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForStranger})
	// older is the carrier a build before the carrier sends: no member at all.
	var older types.WrittenFor
	const (
		made = iota
		refused
		// refusedAlone is a carrier no ceiling makes right: refused on the
		// requests no ceiling rides on too.
		refusedAlone
		bounded
	)
	version := int64(1)
	for _, tc := range []struct {
		name       string
		asker      gitlet.Principal
		ceiling    *gitlet.WriteCeiling
		writtenFor types.WrittenFor
		// want is made, refused naming written_for (where a ceiling rides;
		// refusedAlone wherever carried) and saying says where set, or bounded:
		// refused by its ceiling, saying says of a change's edit.
		want int
		says string
	}{
		{"a colleague's ceiling beside another colleague", cto, seat("reader"), forSeat("vp-sales"), refused, ""},
		{"a colleague's ceiling beside a credential", cto, seat("reader"), credential, refused, ""},
		{"nobody's ceiling beside a seat", cto, nobody, forSeat("reader"), refused, ""},
		{"a colleague with no ceiling", cto, nil, forSeat("reader"), refused,
			"seat reader, which this node's chart holds as an agent's"},
		{"a credential with no ceiling", dev, nil, credential, refused,
			"credential token:ci carries the ceiling of nobody"},
		{"a seat this node's chart does not place, with no ceiling", cto, nil, forSeat("ghost"), refused,
			"this node's chart does not place seat ghost"},
		{"a person's seat under that seat's ceiling", cto, seat("founder"), forSeat("founder"), refused, ""},
		{"a stranger", cto, nil, stranger, refusedAlone, ""},
		{"a person's write carrying one", ada, nil, forSeat("founder"), refusedAlone, ""},
		{"a person's seat with no ceiling", cto, nil, forSeat("founder"), made, ""},
		{"a colleague under its own ceiling", cto, seat("reader"), forSeat("reader"), made, ""},
		{"a credential under nobody's ceiling, the seat's own work", dev, nobody, credential, made, ""},
		{"nobody", cto, nil, types.ForTurn(nil), made, ""},
		{"an older sender's colleague who reads", cto, seat("reader"), older, made, ""},
		{"an older sender's credential, the seat's own work", dev, nobody, older, made, ""},
		{"an older sender's colleague the chart lacks", cto, seat("ghost"), older, bounded,
			"holds none on this repository"},
		{"an older sender's credential, another's change", cto, nobody, older, bounded,
			"who may change nothing of this company's code"},
	} {
		refuses := tc.want == refused || tc.want == refusedAlone

		// THE COMMITTER, a change's lifecycle: an edit of dev's change.
		before := n.end()
		p := retitled(repo, change, tc.asker, version, "by "+tc.name)
		p.Ceiling, p.WrittenFor = tc.ceiling, tc.writtenFor
		p = arrived(t, p)
		res, err := c.ProposeChange(ctx, p)
		switch {
		case refuses:
			requireWrittenForRefused(t, n, before, "the edit made for "+tc.name, tc.says, err)
		case tc.want == bounded:
			requireRefused(t, n, before, res, err, gitlet.ChangeRefusalNotPermitted, tc.says)
		default:
			requireChanged(t, res, err)
			version = res.Version
		}

		// THE CHANGE WRITER, a comment on it waking plat-lead.
		before = n.end()
		comment := saying(repo, change, tc.asker, "", "by "+tc.name)
		comment.Comment.Mentions = []string{"plat-lead"}
		comment.Ceiling, comment.WrittenFor = tc.ceiling, tc.writtenFor
		comment = arrived(t, comment)
		said, err := w.Converse(ctx, comment)
		switch {
		case refuses:
			requireWrittenForRefused(t, n, before, "the comment made for "+tc.name, tc.says, err)
		case tc.want == bounded:
			requireConversationRefused(t, n, before, said, err, gitlet.ConversationRefusalNotPermitted, "may not")
		default:
			requireConversed(t, said, err)
			// AN OLDER SENDER'S WRITE RECORDS NOBODY, at version 1, though its
			// routing still wakes plat-lead.
			if v, _, carried := carriedRecord(t, n, said.Position); !tc.writtenFor.Stated() &&
				(v != 1 || len(carried) != 1 || carried[0] != nil) {
				t.Errorf("the comment made for %s records %v at version %d, want a routing carrying nobody at "+
					"version 1", tc.name, carried, v)
			}
		}

		// THE COMMITTER again, a dequeue of no entry: decided at all, it is
		// refused not_queued.
		before = n.end()
		out := dequeueing(repo, uuid.Must(uuid.NewV7()).String(), tc.asker)
		out.Ceiling, out.WrittenFor = tc.ceiling, tc.writtenFor
		out = arrived(t, out)
		queued, err := c.ProposeQueue(ctx, out)
		if refuses {
			requireWrittenForRefused(t, n, before, "the dequeue made for "+tc.name, tc.says, err)
		} else if err != nil || queued.Refusal != gitlet.MergeRefusalNotQueued {
			t.Errorf("the dequeue made for %s answered %+v %v, want it decided: not_queued, for an entry its "+
				"queues hold none of", tc.name, queued, err)
		}

		// THE GITLET NODE'S RULE, an engine write: Validate on every party
		// to it, and against its chart where it is built.
		ew := aWrite()
		ew.Principal = tc.asker
		if tc.asker.Kind == gitlet.PrincipalAgent {
			ew.Principal.Run = gitlet.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001"}
		}
		ew.Ceiling, ew.WrittenFor = tc.ceiling, tc.writtenFor
		ew = arrived(t, ew)
		err = ew.Validate()
		if err == nil {
			err = ew.WrittenForOn(testChart())
		}
		switch {
		case refuses && (!errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "written_for") ||
			!strings.Contains(err.Error(), tc.says)):
			t.Errorf("the engine write made for %s: %v, want it refused, ErrInvalid naming written_for and "+
				"saying %q", tc.name, err, tc.says)
		case !refuses && err != nil:
			t.Errorf("the engine write made for %s: %v, want it admitted", tc.name, err)
		}

		// THE REQUESTS NO CEILING RIDES ON — a push, a merge, an enqueue — are
		// each built from a write already held to its ceiling, so their rule
		// refuses only what no ceiling could make right.
		cmd := update("refs/heads/carried", gitlet.ZeroOID, oidOf("carried"))
		push := proposal(repo, cmd)
		if tc.asker.Kind == gitlet.PrincipalAgent {
			push = seatProposal(repo, tc.asker.Seat, cmd)
		} else {
			push.Principal = tc.asker
		}
		merge := merging(repo, change, tc.asker, gitlet.MergeMethodFastForward, oidOf("A"), oidOf("B"),
			oidOf("B"))
		enqueue := enqueueing(repo, change, tc.asker, oidOf("B"))
		push.WrittenFor, merge.WrittenFor, enqueue.WrittenFor = tc.writtenFor, tc.writtenFor, tc.writtenFor
		for what, refusal := range map[string]error{
			"push":    arrived(t, push).Validate(),
			"merge":   arrived(t, merge).Validate(),
			"enqueue": arrived(t, enqueue).Validate(),
		} {
			switch alone := tc.want == refusedAlone; {
			case alone && (!errors.Is(refusal, gitlet.ErrInvalidProposal) ||
				!strings.Contains(refusal.Error(), "written_for")):
				t.Errorf("the %s made for %s: %v, want it refused naming written_for", what, tc.name, refusal)
			case !alone && refusal != nil:
				t.Errorf("the %s made for %s: %v, want it admitted, its builder having held it to its ceiling",
					what, tc.name, refusal)
			}
		}
	}

	// A CARRIER ON A PAYLOAD THE ASKER SET: the committer stamps an open's,
	// reopen's, merge's and enqueue's from the request, so one an asker sends
	// is refused naming the payload's working_for, as an asker-set principal
	// is, the committer appending nothing.
	madeFor := forSeat("founder")
	open := opening(repo, releaseRef, mainRef, cto)
	open.WrittenFor, open.Open.WorkingFor = madeFor, madeFor.Carrier()
	reopen := reopening(repo, change, cto, version)
	reopen.WrittenFor, reopen.Reopen.WorkingFor = madeFor, madeFor.Carrier()
	merge := merging(repo, change, cto, gitlet.MergeMethodFastForward, oidOf("A"), oidOf("B"), oidOf("B"))
	merge.WrittenFor, merge.Merge.WorkingFor = madeFor, madeFor.Carrier()
	enqueue := enqueueing(repo, change, cto, oidOf("B"))
	enqueue.WrittenFor, enqueue.Enqueue.WorkingFor = madeFor, madeFor.Carrier()
	for what, refusal := range map[string]error{
		"open": open.Validate(), "reopen": reopen.Validate(), "merge": merge.Validate(),
		"enqueue": enqueue.Validate(),
	} {
		if !errors.Is(refusal, gitlet.ErrInvalidProposal) || !strings.Contains(refusal.Error(), what+".working_for") {
			t.Errorf("the %s whose payload its asker set carrying founder: %v, want it refused naming %s.working_for",
				what, refusal, what)
		}
	}
	before := n.end()
	if res, err := c.ProposeChange(ctx, arrived(t, open)); !errors.Is(err, gitlet.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "open.working_for") || n.end() != before {
		t.Errorf("the committer answered the open whose payload its asker set carrying founder %+v %v, appending "+
			"%d records, want it refused naming open.working_for with none", res, err, n.end()-before)
	}
}

// WHOM A CODE WRITE IS MADE FOR IS A SEAT OR A CREDENTIAL, WELL FORMED (ADR-0109): a
// seat by its handle alone; a credential acting as no seat by its coloned machine
// login and the apps its turn was held to, none twice and at least one (naming none,
// it is a stranger's to every reader); never the zero kind. So a record's carrier
// stays bounded; anything else is refused naming the member. A kind or app this
// build does not know is a newer build's, not malformed: Validate and
// EngineWrite.WrittenForOn answer it as unknown, never ErrInvalidProposal or
// ErrInvalid, so the walk asks a node that knows it (internal/estate's
// TestAnInProcessCommitterAnswersAStoredCarriersUnknownAppAsAnotherBuild).
//
// Mutations: admit admin as an app or the zero kind, refuse an unknown value as
// malformed, or drop or wrap (refusedCarrier) the rule's check of one: each red.
func TestWhomACodeWriteIsMadeForIsWellFormed(t *testing.T) {
	t.Parallel()
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	reader := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: "reader"}
	credential := func(login string, apps ...iam.Grant) types.WorkingFor {
		return types.WorkingFor{Kind: types.WorkingForCredential, Login: login, Apps: apps}
	}
	for _, tc := range []struct {
		name    string
		ceiling *gitlet.WriteCeiling
		carrier types.WorkingFor
		// says is what its refusal names, "" for a carrier well formed; and
		// unknown a carrier answered as a value this build does not know.
		says    string
		unknown bool
	}{
		{"a colleague's seat", reader, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "reader"}, "", false},
		{"a credential", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantDoclet), "", false},
		{"a seat by no handle", nil, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "Reader!"},
			"written_for.seat", false},
		{"a seat naming a login", reader, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "reader",
			Login: "token:ci"}, "names the seat alone", false},
		{"a credential by a person's login", nobody, credential("jane.doe", iam.GrantGitlet), "written_for.login",
			false},
		{"a credential naming a seat", nobody, types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
			Seat: "reader", Apps: []iam.Grant{iam.GrantGitlet}}, "written_for.seat", false},
		{"a credential naming no app", nobody, credential("token:ci"), "written_for.apps", false},
		{"a credential naming admin", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantAdmin),
			"is no app", false},
		{"a credential naming an app this build does not know", nobody, credential("token:ci", iam.GrantGitlet,
			"ledger"), "", true},
		{"a credential naming an app twice", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantGitlet),
			"named twice", false},
		{"a kind this build does not know", nil, types.WorkingFor{Kind: "colleague", Seat: "reader"}, "", true},
		{"the zero kind, which names none", nil, types.WorkingFor{Seat: "reader"}, "written_for.kind", false},
	} {
		p := retitled("0190f3c4-0000-7000-8000-0000000000aa", uuid.Must(uuid.NewV7()).String(), dev, 1, tc.name)
		p.Ceiling, p.WrittenFor = tc.ceiling, types.ForTurn(&tc.carrier)
		err := p.Validate()
		switch {
		case tc.unknown && (errors.Is(err, gitlet.ErrInvalidProposal) || !gitlet.IsUnknownToThisBuild(err)):
			t.Errorf("a write made for %s: %v, want it answered as a value this build does not know, never "+
				"malformed", tc.name, err)
		case tc.unknown:
		case tc.says == "" && err != nil:
			t.Errorf("a write made for %s: %v, want it admitted", tc.name, err)
		case tc.says != "" && (!errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.says)):
			t.Errorf("a write made for %s: %v, want it refused naming %s", tc.name, err, tc.says)
		}
		if !tc.unknown {
			continue
		}
		// THE RULE ITSELF, asked by the chart's check of an engine write.
		w := aWrite()
		w.Ceiling, w.WrittenFor = tc.ceiling, types.ForTurn(&tc.carrier)
		if err := w.WrittenForOn(testChart()); errors.Is(err, gitlet.ErrInvalid) || !gitlet.IsUnknownToThisBuild(err) {
			t.Errorf("an engine write made for %s, held to its chart: %v, want it answered as a value this build "+
				"does not know, never malformed", tc.name, err)
		}
	}
}

// arrived is v as a node decodes it off the estate's wire, so a carrier its
// sender left off (nobody, or no member at all from a build before the carrier)
// arrives stated nobody.
func arrived[T any](t *testing.T, v T) T {
	t.Helper()
	raw, err := jsonv2.Marshal(v, jsonv2.OmitZeroStructFields(true))
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var out T
	if err := jsonv2.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return out
}

// requireWrittenForRefused fails t unless err refuses a request as no proposal
// any node decides, naming whom it is made for and saying says, and nothing was
// appended to n's log since before.
func requireWrittenForRefused(t *testing.T, n *node, before uint64, what, says string, err error) {
	t.Helper()
	if !errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), "written_for") ||
		!strings.Contains(err.Error(), says) {
		t.Errorf("%s answered %v, want it refused as no proposal any node decides, naming written_for and "+
			"saying %q", what, err, says)
	}
	if got := n.end(); got != before {
		t.Errorf("%s appended %d records, want none", what, got-before)
	}
}
