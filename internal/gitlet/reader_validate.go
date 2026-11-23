package gitlet

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// The read limits, past which a query is refused naming the bound rather than
// clamped. A page's BYTES are held to MaxPageBytes whatever its count,
// measured with the commands it read to condense pushes ([scanIntroduced]),
// so these bound what one request READS and are sized to a screen: a default
// that fills one, and a maximum whose typical rows stay well under the byte
// budget (500 repositories at MaxProtectionsPerRepo is 32 000 rule rows for
// one statement; 200 typical pushes under 500 KB; 1 000 longest refs about
// 385 KB; revisions and reviewers as narrow as a ref; changes, comments,
// reviews, findings and checks a push history's) while a page of the widest
// rows ends at the byte budget first. latestCheckLimit is MaxChecksPerFile,
// so a branch's latest checks are one page. The maxima are exported for a
// client that pages, and [ReadQuery.PageMax] says which one a query is held to.
const (
	defaultRepoLimit     = 100
	MaxReposPerPage      = 500
	defaultPushLimit     = 50
	MaxPushesPerPage     = 200
	defaultRefLimit      = 200
	MaxRefsPerPage       = 1_000
	defaultChangeLimit   = 50
	MaxChangesPerPage    = 200
	defaultCheckLimit    = 50
	latestCheckLimit     = MaxChecksPerFile
	MaxChecksPerPage     = 200
	defaultRevisionLimit = 200
	MaxRevisionsPerPage  = 1_000
)

// PageMax is the most rows one page of q carries, the bound its Limit is
// refused past: the one a caller that reads a kind to its end pages at, never
// [MaxSnapshotItems], which caps every page the reader serves rather than
// naming any kind's. 0 is a read no limit pages (one row, one answer, or the
// rows it names). TestEveryKindsPageMaxIsTheBoundItsReadRefusesPast holds it
// to every kind's validator, and TestEveryEngineReadAsksAPageItsKindCarries
// every limit the module's own reads ask to it.
func (q ReadQuery) PageMax() int {
	switch q.Kind {
	case ReadRepos:
		if q.DayBounds != nil {
			return MaxSparkReposPerPage
		}
		return MaxReposPerPage
	case ReadPushes:
		if q.Push != "" {
			return 0
		}
		return MaxPushesPerPage
	case ReadRefs:
		return MaxRefsPerPage
	case ReadChanges:
		return MaxChangesPerPage
	case ReadChange:
		switch {
		case q.DetailOnly, len(q.FindingIDs) > 0:
			return 0
		case q.FindingPaths:
			return MaxFindingPathsPerPage
		case q.Viewed:
			return MaxViewedPerPage
		}
		return q.Part.maxPerPage()
	case ReadChangeRouting:
		return MaxRoutedRecordsPerPage
	case ReadReviews:
		return MaxReviewsPerPage
	case ReadChecks:
		return MaxChecksPerPage
	case ReadQueue:
		return MaxQueueRowsPerPage
	case ReadLandings:
		return MaxLandingsPerPage
	case ReadInbox:
		return inbox.MaxRows
	case ReadIntroduced, ReadRunPushes, ReadChecksQueued:
		return MaxSnapshotItems
	}
	return 0
}

// BriefCommands is how many of a push's commands a row of its history read
// IN BRIEF carries ([ReadQuery.Brief]): the lines a screen gives one push.
// A person's, seat's or engine write's push is whole in five; a mirror's of
// up to MaxCommandsPerPush refs is five and a count ([PushRow.Omitted]),
// sparing 140-190 KB on a row a poll resends every minute, the rest one read
// away ([ReadQuery.Push]).
const BriefCommands = 5

// validate refuses a query naming no kind this build reads, no valid asker, a
// repository that cannot be one, a limit past its bound (never clamped), or a
// field outside the kind that reads it, so no field is answered ignored.
func (q ReadQuery) validate() error {
	if !q.Kind.Valid() {
		return refusedf("%w: kind: %q is not one of %s", ErrInvalid, q.Kind, words.Or(ReadKinds()))
	}
	// A RESOLUTION ONLY WHERE A NAME IS JUDGED, before any kind's own
	// rules, so no kind's list of what it refuses has to spell it.
	if q.Resolve && !q.resolves() {
		return refusedf("%w: resolve: only a change read, or a listing of one repository's changes or "+
			"checks, resolves the repository it names", ErrInvalid)
	}
	// A FIELD ONE PRINCIPAL'S KIND READS ALONE is refused on every other kind
	// here, before any kind's own lists, which a field added later is missing
	// from.
	if err := q.kindOwnedField(); err != nil {
		return err
	}
	switch q.Kind {
	case ReadIntroduced:
		return q.validateIntroduced()
	case ReadRunPushes:
		return q.validateRunPushes()
	case ReadCompactionDue:
		return q.validateCompactionDue()
	case ReadPublicationDue:
		return q.validatePublicationDue()
	case ReadExpungeDue:
		return q.validateExpungeDue()
	case ReadImportsDue:
		return q.validateBareListing("an imports_due read lists every repository being imported")
	case ReadMirrorsDue:
		return q.validateBareListing("a mirrors_due read lists every repository with a mirror")
	case ReadChangesDue:
		return q.validateChangesDue()
	case ReadIndexed:
		return q.validateIndexed()
	case ReadDefaultTips:
		return q.validateDefaultTips()
	case ReadChecksDue:
		return q.validateChecksDue()
	case ReadExpungeTips:
		return q.validateExpungeTips()
	case ReadMirrorPush:
		return q.validateMirrorPush()
	case ReadBranchChanges:
		return q.validateBranchChanges()
	case ReadCheckBox:
		return q.validateCheckBox()
	case ReadChecksQueued:
		return q.validateChecksQueued()
	case ReadChecksRunning:
		return q.validateChecksRunning()
	}
	// WHO ASKS IS NAMED, or nothing is read: an answer is held to the asker's
	// grant.
	if why := q.Asker.invalid(); why != "" {
		return refusedf("%w: asker: %s", ErrInvalid, why)
	}
	if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	switch {
	case q.Introduced != nil:
		return refusedf("%w: introduced: only an introduced read names a range of pushes", ErrInvalid)
	case q.RunPushes != nil:
		return refusedf("%w: run_pushes: only a run_pushes read names an executor", ErrInvalid)
	case q.ChangesDue != nil:
		return refusedf("%w: changes_due: only a changes_due read names the repository its home owes "+
			"change work in", ErrInvalid)
	case q.ChecksDue != nil:
		return refusedf("%w: checks_due: only a checks_due read names the repository its home owes "+
			"check work in", ErrInvalid)
	case q.Checks != nil && q.Kind != ReadChecks && q.Kind != ReadCheck:
		return refusedf("%w: checks: only a checks or a check read names a change's, a commit's or an "+
			"attempt's checks", ErrInvalid)
	case q.CheckBox != nil:
		return refusedf("%w: check_box: only a check_box read names an attempt by its key", ErrInvalid)
	case q.ChecksRunning != nil:
		return refusedf("%w: checks_running: only a checks_running read names the repository whose running "+
			"checks it reads", ErrInvalid)
	case q.MirrorPush != nil:
		return refusedf("%w: mirror_push: only a mirror_push read names the repository whose push mirror "+
			"it reads", ErrInvalid)
	case q.DefaultTips != nil:
		return refusedf("%w: default_tips: only a default_tips read names the repository whose tip it reads",
			ErrInvalid)
	case q.Target != "" && q.Kind != ReadQueue && q.Kind != ReadLandings && q.Kind != ReadGreen:
		return refusedf("%w: target: only a queue, a landings or a green read names a branch's queue",
			ErrInvalid)
	case q.ExpungeTips != nil:
		return refusedf("%w: expunge_tips: only an expunge_tips read names a repository's ref drops", ErrInvalid)
	case !q.ScannedBefore.IsZero():
		return refusedf("%w: scanned_before: only an expunge_due read lists the repositories last judged "+
			"before an instant", ErrInvalid)
	case q.BranchChanges != nil:
		return refusedf("%w: branch_changes: only a branch_changes read names the branches whose live "+
			"changes it answers", ErrInvalid)
	case q.MinPacks != 0:
		return refusedf("%w: min_packs: only a compaction_due read names a floor of live packs", ErrInvalid)
	case q.Prefix != "" && q.Kind != ReadRefs && q.Kind != ReadRepos:
		return refusedf("%w: prefix: only a refs listing and a repositories listing name a prefix", ErrInvalid)
	case q.Prefix != "" && q.Kind == ReadRefs && !validRefPrefix(q.Prefix):
		return refusedf("%w: prefix: %q is not refs/heads/ or refs/tags/ or a directory of refs under "+
			"either, ending in /", ErrInvalid, q.Prefix)
	case q.Prefix != "" && q.Kind == ReadRepos && !validRepoPrefix(q.Prefix):
		return refusedf("%w: prefix: no repository name begins %q: a name is lowercase letters, digits and "+
			"hyphens, beginning with a letter or a digit, and a seat's own holds one dot", ErrInvalid, q.Prefix)
	case q.Brief && q.Kind != ReadPushes && q.Kind != ReadChange:
		return refusedf("%w: brief: only a push history and a change's first page are read in brief",
			ErrInvalid)
	case q.Brief && q.Kind == ReadChange && (q.Part != "" || q.Cursor != ""):
		return refusedf("%w: brief: only a change's first page of its revisions, or its row alone with "+
			"detail_only, is read in brief — named by no part or cursor", ErrInvalid)
	case q.Push != "" && q.Kind != ReadPushes:
		return refusedf("%w: push: only a push history is narrowed to one push", ErrInvalid)
	case q.PathOwners && q.Kind != ReadRepo:
		return refusedf("%w: path_owners: only a read of one repository answers its path owners", ErrInvalid)
	case q.Gates && q.Kind != ReadRepo && (q.Kind != ReadRefs || q.Cursor != ""):
		return refusedf("%w: gates: only a read answering one repository's detail — a read of one "+
			"repository, or a refs listing's first page — answers its rules' merge gates", ErrInvalid)
	case q.Push != "" && !ValidPushID(q.Push):
		return refusedf("%w: push: %q is not a push's id, a UUIDv7 in its canonical lowercase spelling",
			ErrInvalid, q.Push)
	case q.Push != "" && (q.Cursor != "" || q.Limit != 0 || q.Brief):
		return refusedf("%w: push: one push is read whole, never paged, cut or read in brief", ErrInvalid)
	case q.Change != 0 && q.Kind != ReadChange && q.Kind != ReadChangeRouting:
		return refusedf("%w: change: only a change read and a change's routing name a change's number",
			ErrInvalid)
	case q.State != "" && q.Kind != ReadChanges:
		return refusedf("%w: state: only a listing of changes is narrowed to a change's state", ErrInvalid)
	case q.Owner != "" && q.Kind != ReadChanges:
		return refusedf("%w: owner: only a listing of changes is narrowed to a change's owner", ErrInvalid)
	case q.Work != "" && q.Kind != ReadChanges:
		return refusedf("%w: work: only a listing of changes is narrowed to a work item", ErrInvalid)
	case q.Reviewer != "" && q.Kind != ReadChanges && q.Kind != ReadReviews:
		return refusedf("%w: reviewer: only a listing of changes or of a reviewer's reviews names a reviewer",
			ErrInvalid)
	case q.Needs != "" && q.Kind != ReadChanges:
		return refusedf("%w: needs: only a listing of changes is narrowed to what its changes wait on",
			ErrInvalid)
	case q.Window != "" && q.Kind != ReadChanges:
		return refusedf("%w: window: only a listing of changes is narrowed to the changes that moved "+
			"within a window", ErrInvalid)
	case q.Part != "" && q.Kind != ReadChange:
		return refusedf("%w: part: only a change read names a part of the change", ErrInvalid)
	case q.Part != "" && !q.Part.Valid():
		return refusedf("%w: part: %q is not one of %s", ErrInvalid, q.Part, words.Or(ChangeParts()))
	case q.DetailOnly && q.Kind != ReadChange:
		return refusedf("%w: detail_only: only a change read answers a change's detail", ErrInvalid)
	case q.DetailOnly && (q.Part != "" || q.Cursor != "" || q.Limit != 0):
		return refusedf("%w: detail_only: a change's detail alone pages no part — named by no part, cursor "+
			"or limit", ErrInvalid)
	case len(q.FindingIDs) > 0 && (q.Kind != ReadChange || q.Part != PartFindings):
		return refusedf("%w: finding_ids: only a change read's findings part is narrowed to the findings it "+
			"names", ErrInvalid)
	case len(q.FindingIDs) > 0 && q.Limit != 0:
		return refusedf("%w: finding_ids: the findings named are the read's rows, which no limit cuts",
			ErrInvalid)
	case q.FindingPaths && (q.Part != "" || q.DetailOnly || q.Brief || len(q.FindingIDs) > 0):
		return refusedf("%w: finding_paths: a change's findings by path are a page of their own, "+
			"named by no part, detail_only, brief or finding_ids", ErrInvalid)
	case q.Viewed && (q.Part != "" || q.DetailOnly || q.Brief || len(q.FindingIDs) > 0 || q.FindingPaths):
		return refusedf("%w: viewed: a change's viewed files are a page of their own, named by no part, "+
			"detail_only, brief, finding_ids or finding_paths", ErrInvalid)
	}
	switch {
	case q.Kind == ReadInbox && q.Repo != "":
		return refusedf("%w: repo: an inbox is read across every repository the asker may read", ErrInvalid)
	case q.Kind == ReadNoticeCards && q.Repo != "":
		return refusedf("%w: repo: an inbox page's cards are read across every repository its changes are on",
			ErrInvalid)
	case q.Kind == ReadReviews && q.Repo != "":
		return refusedf("%w: repo: a reviewer's reviews are listed across every repository the asker may "+
			"read, and a change's own reviews are a change read's part", ErrInvalid)
	case q.Kind == ReadRepos, q.Kind == ReadChanges && q.Repo == "", q.Kind == ReadChecks && q.Repo == "",
		q.Kind == ReadQueue && q.Repo == "", q.Kind == ReadReviews, q.Kind == ReadInbox,
		q.Kind == ReadNoticeCards, q.Kind == ReadCounts && q.Repo == "":
	case !ValidName(q.Repo):
		return refusedf("%w: repo: %q is not a repository name", ErrInvalid, q.Repo)
	}
	if q.Kind != ReadRepos && len(q.Status) > 0 {
		return refusedf("%w: status: only a listing of repositories filters by status", ErrInvalid)
	}
	for _, s := range q.Status {
		if !s.Valid() {
			return refusedf("%w: status: %q is not one of %s", ErrInvalid, s, words.Or(RepoStatuses()))
		}
	}
	switch q.Kind {
	case ReadRepos:
		if err := q.validateRepos(); err != nil {
			return err
		}
	case ReadRefs:
		if q.RefListing != nil {
			if err := q.validateRefListing(); err != nil {
				return err
			}
		}
	case ReadChanges:
		if err := q.validateChanges(); err != nil {
			return err
		}
	case ReadChange, ReadChangeRouting:
		if q.Change < 1 {
			return refusedf("%w: change: %d is no change's number: a repository numbers its changes "+
				"from 1", ErrInvalid, q.Change)
		}
		if err := validateFindingIDs(q.FindingIDs); err != nil {
			return err
		}
	case ReadReviews:
		if err := q.validateReviews(); err != nil {
			return err
		}
	case ReadCommit:
		if err := q.validateCommit(); err != nil {
			return err
		}
	case ReadInbox:
		if err := q.validateInbox(); err != nil {
			return err
		}
	case ReadNoticeCards:
		if err := q.validateNoticeCards(); err != nil {
			return err
		}
	case ReadCommitMarks:
		if err := q.validateCommitMarks(); err != nil {
			return err
		}
	case ReadCounts:
		if err := q.validateCounts(); err != nil {
			return err
		}
	case ReadChecks, ReadCheck:
		if err := q.validateChecks(); err != nil {
			return err
		}
	case ReadQueue, ReadLandings, ReadGreen:
		if err := q.validateQueueRead(); err != nil {
			return err
		}
	}
	if q.DayBounds != nil {
		if err := q.validateDayBounds(); err != nil {
			return err
		}
	}
	if q.PushFilter != nil {
		if err := q.validatePushFilter(); err != nil {
			return err
		}
	}
	switch bound := q.PageMax(); {
	case q.Limit < 0:
		return refusedf("%w: limit: %d", ErrInvalid, q.Limit)
	case q.Kind == ReadRepo && q.Limit != 0:
		return refusedf("%w: limit: one repository is not paged", ErrInvalid)
	case q.Limit > bound:
		return refusedf("%w: limit: %d is past the %d one page of %s carries", ErrInvalid,
			q.Limit, bound, q.Kind)
	}
	return nil
}

// kindOwnedField refuses a field only one principal's kind reads on a read of
// any other kind, the engine's own reads included ([ReadQuery.validate]).
func (q ReadQuery) kindOwnedField() error {
	switch {
	case q.Protected && q.Kind != ReadRepos:
		return refusedf("%w: protected: only a listing of repositories is narrowed to the ones a rule "+
			"protects", ErrInvalid)
	case q.Commit != "" && q.Kind != ReadCommit:
		return refusedf("%w: commit: only a commit read names a commit", ErrInvalid)
	case q.Cards && q.Kind != ReadChanges:
		return refusedf("%w: cards: only a listing of changes carries each change's card", ErrInvalid)
	case q.FindingPaths && q.Kind != ReadChange:
		return refusedf("%w: finding_paths: only a change read counts its findings by path", ErrInvalid)
	case q.Viewed && q.Kind != ReadChange:
		return refusedf("%w: viewed: only a change read answers the files its asker marked viewed", ErrInvalid)
	case q.Inbox != nil && q.Kind != ReadInbox:
		return refusedf("%w: inbox: only an inbox read names whose inbox it reads", ErrInvalid)
	case len(q.NoticeCards) > 0 && q.Kind != ReadNoticeCards:
		return refusedf("%w: notice_cards: only a notice_cards read names the changes whose cards it reads",
			ErrInvalid)
	case q.ChangeSort != "" && q.Kind != ReadChanges:
		return refusedf("%w: change_sort: only a listing of changes is ordered by when its changes were "+
			"opened or last moved", ErrInvalid)
	case q.Readiness != "" && q.Kind != ReadChanges:
		return refusedf("%w: readiness: only a listing of changes is narrowed to where their merge stands",
			ErrInvalid)
	case q.Text != "" && q.Kind != ReadChanges:
		return refusedf("%w: text: only a listing of changes is narrowed by its changes' words", ErrInvalid)
	case q.RefListing != nil && q.Kind != ReadRefs:
		return refusedf("%w: ref_listing: only a refs listing is ordered, narrowed or read in detail",
			ErrInvalid)
	case len(q.CommitMarks) > 0 && q.Kind != ReadCommitMarks:
		return refusedf("%w: commit_marks: only a commit_marks read names the commits whose marks it reads",
			ErrInvalid)
	case q.Counts != nil && q.Kind != ReadCounts:
		return refusedf("%w: counts: only a counts read names the parts it counts", ErrInvalid)
	case q.PushFilter != nil && q.Kind != ReadPushes:
		return refusedf("%w: push_filter: only a push history is narrowed to one pusher or a window", ErrInvalid)
	case len(q.DayBounds) > 0 && q.Kind != ReadRepos && q.Kind != ReadRepo && q.Kind != ReadPushes:
		return refusedf("%w: day_bounds: only a listing of repositories, a read of one and a push history "+
			"count pushes by the asker's days", ErrInvalid)
	case q.RepoFacts && q.Kind != ReadRepos && q.Kind != ReadRepo:
		return refusedf("%w: repo_facts: only a listing of repositories and a read of one answer a "+
			"repository's facts", ErrInvalid)
	case q.RepoKind != "" && q.Kind != ReadRepos:
		return refusedf("%w: repo_kind: only a listing of repositories is narrowed to one kind", ErrInvalid)
	case q.RepoSort != "" && q.Kind != ReadRepos:
		return refusedf("%w: repo_sort: only a listing of repositories is ordered by name or last push",
			ErrInvalid)
	case q.Trains && q.Kind != ReadQueue:
		return refusedf("%w: trains: only a queue read answers its trains and ejections", ErrInvalid)
	}
	return nil
}

// validateIntroduced is [ReadQuery.validate] for an `introduced` read: the
// engine's own, so it names NO asker (one would look held to a grant that
// judges nothing), and its repository by id in a range running one way:
// (After, Through] ascending, or below Before descending.
func (q ReadQuery) validateIntroduced() error {
	in := q.Introduced
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	onLog := func(p statelog.Position) bool { return p.IsZero() || p.Stream == topics.CodeLogStream }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "an introduced read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "an introduced read is the engine's own, which no seat makes for another")
	case in == nil:
		return refused("introduced", "an introduced read names its repository and its range")
	case in.RepoID == "":
		return refused("introduced.repo_id", "an introduced read names its repository by id")
	case q.Repo != "" || len(q.Status) > 0 || q.Prefix != "":
		return refused("repo", "an introduced read names its repository in its range and filters nothing")
	case q.Brief:
		return refused("brief", "an introduced read condenses every push in its range its own way")
	case q.Push != "":
		return refused("push", "an introduced read names its pushes by its range alone")
	case q.PathOwners:
		return refused("path_owners", "only a read of one repository answers its path owners")
	case q.Gates:
		return refused("gates", "only a read answering one repository's detail answers its rules' merge gates")
	case q.DetailOnly:
		return refused("detail_only", "only a change read answers a change's detail")
	case len(q.FindingIDs) > 0:
		return refused("finding_ids", "only a change read's findings part is narrowed to the findings it names")
	case q.MinPacks != 0:
		return refused("min_packs", "only a compaction_due read names a floor of live packs")
	case q.Change != 0:
		return refused("change", "only a change read and a change's routing name a change's number")
	case q.State != "":
		return refused("state", "only a listing of changes is narrowed to a change's state")
	case q.Owner != "":
		return refused("owner", "only a listing of changes is narrowed to a change's owner")
	case q.Work != "":
		return refused("work", "only a listing of changes is narrowed to a work item")
	case q.RunPushes != nil:
		return refused("run_pushes", "only a run_pushes read names an executor")
	case q.ChangesDue != nil:
		return refused("changes_due", "only a changes_due read names the repository its home owes change work "+
			"in")
	case q.ChecksDue != nil:
		return refused("checks_due", "only a checks_due read names the repository its home owes check work in")
	case q.Checks != nil:
		return refused("checks", "only a checks or a check read names a change's, a commit's or an attempt's "+
			"checks")
	case q.CheckBox != nil:
		return refused("check_box", "only a check_box read names an attempt by its key")
	case q.ChecksRunning != nil:
		return refused("checks_running", "only a checks_running read names the repository whose running "+
			"checks it reads")
	case q.MirrorPush != nil:
		return refused("mirror_push", "only a mirror_push read names the repository whose push mirror it reads")
	case q.DefaultTips != nil:
		return refused("default_tips", "only a default_tips read names the repository whose tip it reads")
	case q.ExpungeTips != nil:
		return refused("expunge_tips", "only an expunge_tips read names a repository's ref drops")
	case !q.ScannedBefore.IsZero():
		return refused("scanned_before", "only an expunge_due read lists the repositories last judged "+
			"before an instant")
	case q.Target != "":
		return refused("target", "only a queue, a landings or a green read names a branch's queue")
	case q.Reviewer != "":
		return refused("reviewer", "only a listing of changes or of a reviewer's reviews names a reviewer")
	case q.Needs != "":
		return refused("needs", "only a listing of changes is narrowed to what its changes wait on")
	case q.Part != "":
		return refused("part", "only a change read names a part of the change")
	case !onLog(in.After) || !onLog(in.Through) || !onLog(in.Before):
		return refused("introduced", "every position it names is on the code log")
	case in.Descending && in.Before.IsZero():
		return refused("introduced.before", "a descending read names the position it reads below")
	case in.Descending && (!in.After.IsZero() || !in.Through.IsZero()):
		return refused("introduced", "a descending read names Before alone")
	case !in.Descending && !in.Before.IsZero():
		return refused("introduced.before", "an ascending read names After and Through")
	case !in.Through.IsZero() && in.Through.Packed() < in.After.Packed():
		return refused("introduced.through", "the range ends below where it begins")
	case q.Limit < 0:
		return refused("limit", strconv.Itoa(q.Limit))
	case q.Limit > MaxSnapshotItems:
		return refused("limit", fmt.Sprintf("%d is past the %d one page of %s carries", q.Limit,
			MaxSnapshotItems, q.Kind))
	}
	return nil
}

// validateRunPushes is [ReadQuery.validate] for a `run_pushes` read, the
// engine's own when a run is collected: NO asker or ceiling seat (an
// `introduced` read's reason); the run's launch id; its repositories by id,
// one to MaxRunRepos, each once; and its launch position on the code log,
// never zero, which would scan whole histories for pushes no run made. A
// page's limit is at most MaxSnapshotItems ([Reader.runPushes]).
func (q ReadQuery) validateRunPushes() error {
	in := q.RunPushes
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a run_pushes read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a run_pushes read is the engine's own, which no seat makes for another")
	case in == nil:
		return refused("run_pushes", "a run_pushes read names its executor, the run's repositories and the "+
			"position it was launched at")
	case !canonicalUUID(in.Run):
		return refused("run_pushes.run", fmt.Sprintf("%q is not a UUID in its canonical lowercase spelling — "+
			"an executor is read by the launch id its pushes name", in.Run))
	case len(in.Repos) == 0 || len(in.Repos) > MaxRunRepos:
		return refused("run_pushes.repos", fmt.Sprintf("%d repositories; an executor is given one to %d "+
			"(MaxRunRepos)", len(in.Repos), MaxRunRepos))
	case in.After.IsZero():
		return refused("run_pushes.after", "a run_pushes read names the position its run was launched at: "+
			"nothing the run pushed applied below it")
	case in.After.Stream != topics.CodeLogStream:
		return refused("run_pushes.after", fmt.Sprintf("%s is a position on another log than the code log (%s)",
			in.After, topics.CodeLogStream))
	case in.After.Valid() != nil:
		return refused("run_pushes.after", in.After.Valid().Error())
	case q.Limit < 0:
		return refused("limit", strconv.Itoa(q.Limit))
	case q.Limit > MaxSnapshotItems:
		return refused("limit", fmt.Sprintf("%d is past the %d pushes one page of %s reads", q.Limit,
			MaxSnapshotItems, q.Kind))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a run_pushes read names its repositories in its run and filters nothing")
		}
	}
	seen := make(map[string]bool, len(in.Repos))
	for i, id := range in.Repos {
		switch {
		case !canonicalUUID(id):
			return refused(fmt.Sprintf("run_pushes.repos[%d]", i), fmt.Sprintf("%q is not a repository's id "+
				"in its canonical lowercase spelling", id))
		case seen[id]:
			return refused(fmt.Sprintf("run_pushes.repos[%d]", i), fmt.Sprintf("%s is named twice — a run's "+
				"repositories are read once each", id))
		}
		seen[id] = true
	}
	return nil
}

// validateCompactionDue is [ReadQuery.validate] for a `compaction_due` read,
// the compactor's own poll: NO asker (a principal's read of a duty listing
// would be held to nothing), its floor of live packs, at least one, and
// nothing else; a page is MaxSnapshotItems repositories.
func (q ReadQuery) validateCompactionDue() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", "a compaction_due read is the engine's own, which no principal makes")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a compaction_due read lists every repository at its floor of live "+
				"packs, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	if q.MinPacks < 1 {
		return refused("min_packs", fmt.Sprintf("%d; a compaction_due read names the fewest live packs a "+
			"repository it lists holds, at least 1", q.MinPacks))
	}
	return nil
}

// validatePublicationDue is [ReadQuery.validate] for a `publication_due` read,
// the publisher's own poll: NO asker (`compaction_due`'s reason) and nothing
// else, since its partial index holds exactly what it lists.
func (q ReadQuery) validatePublicationDue() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", "a publication_due read is the engine's own, which no principal makes")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a publication_due read lists every repository holding a pack no "+
				"published layer covers, a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateExpungeDue is [ReadQuery.validate] for an `expunge_due` read, the
// maintainer's own poll: a bare listing ([ReadQuery.validateBareListing]) but
// for the instant its weekly arm lists from ([ReadQuery.ScannedBefore]).
func (q ReadQuery) validateExpungeDue() error {
	bare := q
	bare.ScannedBefore = time.Time{}
	return bare.validateBareListing("an expunge_due read lists every repository with an expunge open or a " +
		"purge requested, then every one last judged before its scanned_before")
}

// validateBareListing is [ReadQuery.validate] for a duty listing naming
// nothing but its cursor: the engine's own, so NO asker, and no limit, since
// its partial index holds exactly what it lists and a page is MaxSnapshotItems.
// lists is the sentence a refusal says what it lists in.
func (q ReadQuery) validateBareListing(lists string) error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	if q.Asker != (Principal{}) {
		return refused("asker", fmt.Sprintf("a %s read is the engine's own, which no principal makes", q.Kind))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, lists+", a page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateDefaultTips is [ReadQuery.validate] for a `default_tips` read: a
// bare listing, or ONE repository by its id ([DefaultTipsQuery]).
func (q ReadQuery) validateDefaultTips() error {
	one := q.DefaultTips
	q.DefaultTips = nil
	if err := q.validateBareListing("a default_tips read lists every active project repository"); err != nil {
		return err
	}
	if one == nil {
		return nil
	}
	if q.Cursor != "" {
		return refusedf("%w: cursor: a default_tips read of one repository answers one row", ErrInvalid)
	}
	if parsed, err := uuid.Parse(one.RepoID); err != nil || parsed.String() != one.RepoID {
		return refusedf("%w: default_tips.repo_id: not a repository's id", ErrInvalid)
	}
	return nil
}

// validateIndexed is [ReadQuery.validate] for an `indexed` read, the
// indexer's own listing: held to an `expunge_due` read's rule.
func (q ReadQuery) validateIndexed() error {
	if err := q.validateExpungeDue(); err != nil {
		return fmt.Errorf("%w (an indexed read is held to the same: it lists the repositories searched "+
			"by index, a page of MaxSnapshotItems at a time, after its cursor)", err)
	}
	return nil
}

// validateExpungeTips is [ReadQuery.validate] for an `expunge_tips` read, the
// maintainer's own: NO asker, its repository by id and the instant its drops
// are listed from, never zero (the scan's less the grace kept), and nothing
// else; a page is MaxSnapshotItems drops.
func (q ReadQuery) validateExpungeTips() error {
	in := q.ExpungeTips
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "an expunge_tips read is the engine's own, which no principal makes")
	case in == nil:
		return refused("expunge_tips", "an expunge_tips read names its repository and the instant its drops are "+
			"listed from")
	case !canonicalUUID(in.RepoID):
		return refused("expunge_tips.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", in.RepoID))
	case in.Since.IsZero():
		return refused("expunge_tips.since", "an expunge_tips read names the instant its drops are listed from: "+
			"the scan's, less the grace the expunge keeps")
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "an expunge_tips read lists one repository's ref drops since an instant, a "+
				"page of MaxSnapshotItems at a time, and names nothing else")
		}
	}
	return nil
}

// validateMirrorPush is [ReadQuery.validate] for a `mirror_push` read, the
// home's own: NO asker, its repository by id and nothing else, no cursor or
// limit, since a repository holds one push mirror and one green row a
// protected branch.
func (q ReadQuery) validateMirrorPush() error {
	in := q.MirrorPush
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a mirror_push read is the engine's own, which no principal makes")
	case in == nil:
		return refused("mirror_push", "a mirror_push read names the repository whose push mirror it reads")
	case !canonicalUUID(in.RepoID):
		return refused("mirror_push.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", in.RepoID))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil}, {"cursor", q.Cursor != ""},
		{"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"default_tips", q.DefaultTips != nil},
		{"branch_changes", q.BranchChanges != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
	} {
		if f.set {
			return refused(f.field, "a mirror_push read answers one repository's push mirror and green rows, "+
				"and names nothing else")
		}
	}
	return nil
}

// validateBranchChanges is [ReadQuery.validate] for a `branch_changes` read,
// the engine's own for a collected run's delivered refs: NO asker; one to
// [MaxRunRepos] repositories by id, once each, each with branches under
// refs/heads/ once each, [MaxRefsPerQuery] in all; and nothing else, since one
// page answers every branch, each one change at most.
func (q ReadQuery) validateBranchChanges() error {
	in := q.BranchChanges
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a branch_changes read is the engine's own, which no principal makes")
	case in == nil:
		return refused("branch_changes", "a branch_changes read names its repositories and their branches")
	case len(in.Repos) == 0 || len(in.Repos) > MaxRunRepos:
		return refused("branch_changes.repos", fmt.Sprintf("%d repositories; a branch_changes read names one "+
			"to %d (MaxRunRepos)", len(in.Repos), MaxRunRepos))
	}
	repos := make(map[string]bool, len(in.Repos))
	branches := 0
	for i, repo := range in.Repos {
		field := fmt.Sprintf("branch_changes.repos[%d]", i)
		switch {
		case !canonicalUUID(repo.RepoID):
			return refused(field+".repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
				"lowercase spelling", repo.RepoID))
		case repos[repo.RepoID]:
			return refused(field+".repo_id", fmt.Sprintf("%s is named twice — a repository's branches are "+
				"named together", repo.RepoID))
		case len(repo.Refs) == 0:
			return refused(field+".refs", "a repository is named with the branches whose changes the read answers")
		}
		repos[repo.RepoID] = true
		if branches += len(repo.Refs); branches > MaxRefsPerQuery {
			return refused(field+".refs", fmt.Sprintf("past %d branches; a branch_changes read names at most %d "+
				"in all (MaxRefsPerQuery)", MaxRefsPerQuery, MaxRefsPerQuery))
		}
		named := make(map[string]bool, len(repo.Refs))
		for j, ref := range repo.Refs {
			if why := branchProblem(ref); why != "" {
				return refused(fmt.Sprintf("%s.refs[%d]", field, j), why)
			}
			if named[ref] {
				return refused(fmt.Sprintf("%s.refs[%d]", field, j), fmt.Sprintf("%s is named twice", ref))
			}
			named[ref] = true
		}
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"ceiling_seat", q.CeilingSeat != ""}, {"repo", q.Repo != ""}, {"status", len(q.Status) > 0},
		{"prefix", q.Prefix != ""}, {"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil},
		{"expunge_tips", q.ExpungeTips != nil}, {"min_packs", q.MinPacks != 0}, {"limit", q.Limit != 0},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"cursor", q.Cursor != ""}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil}, {"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a branch_changes read answers its repositories' branches' live changes "+
				"in one page, and names nothing else")
		}
	}
	return nil
}

// freshness is the level a read is served at: any level [checkFreshness]
// admits, so a screen reads its own writes back at `session` from the
// position its write answered (the dashboard's SESSION_QUERIES). Where none is
// named, a principal's read and a duty listing are read stale, the REST path's
// level; the engine's own `introduced`, `run_pushes`, `expunge_tips` and
// `branch_changes` are refused, since their caller must name the level its
// "nothing" may be said at: read stale, a run's pushes would answer none for a
// push another node already applied.
func (q ReadQuery) freshness() (statelog.Freshness, error) {
	fresh := q.Fresh
	if fresh.Level == "" && q.Kind != ReadIntroduced && q.Kind != ReadRunPushes && q.Kind != ReadExpungeTips &&
		q.Kind != ReadBranchChanges {
		fresh.Level = statelog.ReadStale
	}
	return fresh, checkFreshness(fresh)
}

// validRefPrefix reports a prefix a refs listing may name: refs/heads/ or
// refs/tags/, or a directory of refs under either — a seat's own namespace —
// ending in "/". A prefix that no ref could begin with is refused rather than
// answered empty.
func validRefPrefix(prefix string) bool {
	return (strings.HasPrefix(prefix, branchPrefix) || strings.HasPrefix(prefix, tagPrefix)) &&
		strings.HasSuffix(prefix, "/") && ValidRef(prefix+"x")
}

// validRepoPrefix reports a prefix a repositories listing may name: a name
// whole, or the opening of one, which a "0" completes since a digit may end a
// label and follow a hyphen or dot. Any other is refused, not answered empty.
func validRepoPrefix(prefix string) bool {
	return ValidName(prefix) || ValidName(prefix+"0")
}

// prefixEnd is the first string past every one prefix begins: prefix with its
// last byte one higher, so [prefix, prefixEnd) is one range of an index by
// name. A refs listing's prefix ends in "/" and a repository name is ASCII
// throughout, so the last byte is never 0xff and the step never carries.
func prefixEnd(prefix string) string {
	return prefix[:len(prefix)-1] + string([]byte{prefix[len(prefix)-1] + 1})
}

// limitOr is a limit, or its kind's default.
func limitOr(limit, fallback int) int {
	if limit == 0 {
		return fallback
	}
	return limit
}
