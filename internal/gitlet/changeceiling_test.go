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

// A CODE WRITE WHOSE CARRIER AND CEILING DISAGREE IS REFUSED (ADR-0109), by
// every node that decides one — the committer a change's lifecycle and a
// dequeue, the change writer its conversation, the Gitlet node an engine write
// (the rule it holds one to: gitlet.EngineWrite.Validate, and WrittenForOn
// against its chart): the engine composes whom a seat's write is made for and
// its ceiling from one bound ceiling, so a request whose two disagree — a
// colleague's ceiling beside another colleague or a credential, nobody's
// beside a seat, a seat the chart holds as an agent's with no ceiling, a
// credential with no ceiling, a person's seat bounded as though it were a
// colleague's — was sent by a site that dropped one, and is refused naming
// written_for with nothing appended. A seat the deciding chart does not place,
// with no ceiling, is held as a colleague's and refused so too, its refusal
// naming the seat and saying this node's chart does not place it, so a chart
// that lags the company's reads as one. So is a stranger's carrier, for whom
// no code write is made, and a carrier on a person's own write — those two on
// the requests no ceiling rides on as well, a push, a merge and an enqueue,
// each built from a write its builder held to that write's ceiling already,
// whose own rule holds the carrier to its asker alone and admits every other
// row's. What agrees is made: a person's seat with no ceiling, a colleague
// with its own ceiling, a credential with nobody's, and nobody. And a ceiling
// that comes with no carrier — what a seat or a Gitlet node of a build before
// the carrier sends, its member absent, which arrives stated nobody — is
// decided by its ceiling as ever: a colleague's grant still bounds it, and
// nobody's still spares only the seat's own work.
//
// Mutations: the agreement skipped — the colleague, credential and nobody rows
// made, red; a stranger's carrier admitted — the push, the merge and the
// enqueue made for one admitted, red; a ceiling with no carrier refused — the
// older sender's rows refused, red; a decide skipping the chart's half — the
// agent seat with no ceiling and the person's seat under its ceiling made
// there, red; a credential with no ceiling admitted — refused only by the
// chart's half, saying it does not place a seat, red; a seat the chart does
// not place refused as an agent's — its refusal not saying the chart does not
// place it, red.
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
	// older is whom a request a build before the carrier sends is made for,
	// as that build sends it: no member at all.
	var older types.WrittenFor
	const (
		made = iota
		refused
		// refusedAlone is refused whatever ceiling rides beside it, and on
		// the requests no ceiling rides on too: a carrier no ceiling makes
		// right.
		refusedAlone
		bounded
	)
	version := int64(1)
	for _, tc := range []struct {
		name       string
		asker      gitlet.Principal
		ceiling    *gitlet.WriteCeiling
		writtenFor types.WrittenFor
		// want is whether the write is made, refused naming written_for —
		// where a ceiling rides, or wherever it is carried — and saying says
		// where it is set, or refused by its ceiling, saying says of a
		// change's edit.
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

		// THE CHANGE WRITER, a comment on it.
		before = n.end()
		comment := saying(repo, change, tc.asker, "", "by "+tc.name)
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
		}

		// THE COMMITTER again, a dequeue: of no entry, which a dequeue it
		// decides at all is refused for.
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

		// THE REQUESTS NO CEILING RIDES ON — a push, a merge and an enqueue:
		// each is built from a write its builder held to that write's ceiling
		// already, so its own rule holds the carrier to the asker alone,
		// refusing what no ceiling could make right and admitting the rest.
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
}

// WHOM A CODE WRITE IS MADE FOR IS A SEAT OR A CREDENTIAL, WELL FORMED
// (ADR-0109): a seat by its handle and nothing beside it; a credential acting
// as no seat by its coloned machine login, with the apps its turn was held to
// — each an app this build knows, none twice, at least one, since a
// credential naming none is a stranger's to every reader — and no seat; and
// nothing of another kind: not the zero, which names none, nor a kind this
// build does not know. So what a record carries of one is a handle or a login
// and a few apps, bounded. Anything else is refused by the rule every node
// holds a request to, naming the member. (Off the wire a kind or an app this
// build does not know never reaches the rule: the request's decoder refuses it
// as a name the build does not declare, so the walk asks a node that knows it
// — internal/estate's TestACodeWriteMovesOffANodeThatDoesNotKnowItsCarriersKind
// and TestACodeWriteForACredentialMovesOffANodeThatDoesNotKnowItsApp.)
//
// Mutations: an app this build does not know admitted — the credential naming
// admin is made, red; the zero kind admitted as a seat's — the carrier naming
// no kind is made, red.
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
		// says is what its refusal names, "" for a carrier well formed.
		says string
	}{
		{"a colleague's seat", reader, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "reader"}, ""},
		{"a credential", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantDoclet), ""},
		{"a seat by no handle", nil, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "Reader!"},
			"written_for.seat"},
		{"a seat naming a login", reader, types.WorkingFor{Kind: types.WorkingForSeat, Seat: "reader",
			Login: "token:ci"}, "names the seat alone"},
		{"a credential by a person's login", nobody, credential("jane.doe", iam.GrantGitlet), "written_for.login"},
		{"a credential naming a seat", nobody, types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
			Seat: "reader", Apps: []iam.Grant{iam.GrantGitlet}}, "written_for.seat"},
		{"a credential naming no app", nobody, credential("token:ci"), "written_for.apps"},
		{"a credential naming admin", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantAdmin),
			"not an app this build knows"},
		{"a credential naming an app twice", nobody, credential("token:ci", iam.GrantGitlet, iam.GrantGitlet),
			"named twice"},
		{"a kind this build does not know", nil, types.WorkingFor{Kind: "colleague", Seat: "reader"},
			"written_for.kind"},
		{"the zero kind, which names none", nil, types.WorkingFor{Seat: "reader"}, "written_for.kind"},
	} {
		p := retitled("0190f3c4-0000-7000-8000-0000000000aa", uuid.Must(uuid.NewV7()).String(), dev, 1, tc.name)
		p.Ceiling, p.WrittenFor = tc.ceiling, types.ForTurn(&tc.carrier)
		err := p.Validate()
		switch {
		case tc.says == "" && err != nil:
			t.Errorf("a write made for %s: %v, want it admitted", tc.name, err)
		case tc.says != "" && (!errors.Is(err, gitlet.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.says)):
			t.Errorf("a write made for %s: %v, want it refused naming %s", tc.name, err, tc.says)
		}
	}
}

// arrived is v as the node it is asked of decodes it: through the decoder it is
// handed, as the estate's wire reads every request, so a carrier its sender
// left off — nobody, or no member at all from a build before the carrier —
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
