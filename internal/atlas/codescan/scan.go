package codescan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Name is the worker's name in a repository's home loop, and who its records
// on the atlas log are attributed to.
const Name = "atlas_code"

// MaxReadsPerPass is how many files one pass reads. A pass runs inside one
// turn of the home loop, a minute at the most, and each read is one git
// process on the repository's Gitlet node: fifty leave a turn most of its
// minute on a loaded node, and a monorepo at the list's bound is read in
// four passes. Files past it wait for the next pass; nothing is published
// until none waits.
const MaxReadsPerPass = 50

// Code is Gitlet as the scan reads it, through the estate router's code
// seam: its rows, and a repository's git on whichever node serves it.
type Code interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
}

// Store is where each repository's standing is kept ([CoordStore]).
type Store interface {
	Load(ctx context.Context) (map[string]Status, error)
	Save(ctx context.Context, st Status) error
	Forget(ctx context.Context, repo string) error
}

// Options is what a scanner is built from.
type Options struct {
	Code  Code
	Log   collect.Log
	Store Store
	// Admits reports whether this node records on the atlas log now; nil
	// admits. A node that does not lists nothing and reads nothing, so a
	// repository homed on it is read once it does.
	Admits func() bool
	// Now is the clock; nil is the wall clock in UTC.
	Now func() time.Time
}

// Scanner is the home loop's worker (gitlethome.Worker) that reads each
// repository's deploy and dependency files when its default branch moves.
type Scanner struct {
	code   Code
	log    collect.Log
	store  Store
	admits func() bool
	now    func() time.Time

	mu sync.Mutex
	// standings is the last standing this node read or wrote for each
	// repository: what lets a nudge that moved no tip cost one row read.
	standings map[string]Status
	// listed is the repositories the listing in flight has named.
	listed   map[string]bool
	readings *readings
}

// New builds the scanner.
func New(o Options) (*Scanner, error) {
	switch {
	case o.Code == nil:
		return nil, errors.New("codescan: Gitlet's reads are required: the files are read through them")
	case o.Log == nil:
		return nil, errors.New("codescan: the atlas log is required: what the files declare is recorded there")
	case o.Store == nil:
		return nil, errors.New("codescan: a store is required: every repository's standing is recorded there")
	}
	s := &Scanner{code: o.Code, log: o.Log, store: o.Store, admits: o.Admits, now: o.Now, standings: map[string]Status{},
		listed: map[string]bool{}, readings: newReadings(MaxReadingBytes)}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// Name is the worker's name.
func (s *Scanner) Name() string { return Name }

func freshAt(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadStale}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}

// scanned reports a repository the scan reads: one the whole company may
// read. One only its unit may read is never scanned (the package doc).
func scanned(t gitlet.DefaultTip) bool { return t.Visibility == gitlet.VisibilityCompany }

// Due is one page of the repositories that owe a pass: every scanned one
// whose default branch is not at the tip last read whole, and every one with
// a standing that is no longer scanned, whose claims are to be withdrawn.
func (s *Scanner) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	if s.admits != nil && !s.admits() {
		return nil, "", nil
	}
	page, err := s.code.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadDefaultTips, Cursor: cursor, Fresh: freshAt(floor)})
	if err != nil {
		return nil, "", fmt.Errorf("codescan: list the repositories' default tips: %w", err)
	}
	standings, err := s.store.Load(ctx)
	if err != nil {
		return nil, "", err
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standings = standings
	if cursor == "" {
		s.listed = map[string]bool{}
	}
	var due []string
	for _, tip := range page.DefaultTips {
		s.listed[tip.ID] = true
		st, found := standings[tip.ID]
		switch {
		case !scanned(tip):
			if found {
				due = append(due, tip.ID)
			}
		case !found || st.Name != tip.Name || st.due(tip.Head, now):
			due = append(due, tip.ID)
		}
	}
	if page.Next == "" {
		for id := range standings {
			if !s.listed[id] {
				due = append(due, id)
			}
		}
	}
	return due, page.Next, nil
}

// Work is one repository's pass: nothing where its default branch has not
// moved since it was read whole, a withdrawal of everything its files said
// where it is no longer scanned, and otherwise a read of what changed.
func (s *Scanner) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	if s.admits != nil && !s.admits() {
		return nil
	}
	ans, err := s.code.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadDefaultTips, Fresh: freshAt(floor),
		DefaultTips: &gitlet.DefaultTipsQuery{RepoID: repoID}})
	if err != nil {
		return fmt.Errorf("codescan: read the default tip of %s: %w", repoID, err)
	}
	var tip *gitlet.DefaultTip
	if len(ans.DefaultTips) == 1 && ans.DefaultTips[0].ID == repoID && scanned(ans.DefaultTips[0]) {
		tip = &ans.DefaultTips[0]
	}
	now := s.now().UTC()
	// THE COMMON NUDGE MOVED NO TIP (a check finished, a branch was pushed):
	// answered from the standing this node last read, with no store read.
	s.mu.Lock()
	st, found := s.standings[repoID]
	s.mu.Unlock()
	if tip != nil && found && st.Name == tip.Name && !st.due(tip.Head, now) {
		return nil
	}
	standings, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	st, found = standings[repoID]
	switch {
	case tip == nil && !found:
		return nil
	case tip == nil:
		return s.release(ctx, st, now)
	case found && st.Name != tip.Name:
		// RENAMED: its claims were made in the old name's voice, and a
		// service is keyed by the name. The old voice is withdrawn whole.
		if err := s.release(ctx, st, now); err != nil {
			return err
		}
		st = Status{}
	case found && !st.due(tip.Head, now):
		s.remember(st)
		return nil
	}
	return s.scan(ctx, *tip, st, floor, now)
}

// scan reads what changed at the tip, and once every listed file is read
// publishes what differs from the log and records the standing.
func (s *Scanner) scan(ctx context.Context, tip gitlet.DefaultTip, st Status, floor statelog.Position,
	now time.Time) error {

	if st.Tip != tip.Head || st.Rules != Rules {
		st = Status{Tip: st.Tip, Rules: st.Rules, LastCompleteAt: st.LastCompleteAt}
	}
	st.Repo, st.Name = tip.ID, tip.Name
	st.LastAttemptAt, st.Completed, st.LastError = now, false, ""
	st.Calls, st.Reads, st.Pending, st.Recorded, st.Purged = 0, 0, 0, 0, 0

	var files []gitlet.CodeFile
	if tip.Head != "" {
		a, err := s.code.GitRead(ctx, gitlet.GitQuery{Kind: gitlet.GitCodeFiles, RepoID: tip.ID, Fresh: freshAt(floor),
			CodeFiles: &gitlet.CodeFilesQuery{Commit: tip.Head}})
		st.Calls++
		switch {
		case err != nil:
			return s.fault(ctx, st, fmt.Errorf("codescan: list the files of %s at %s: %w", tip.Name, tip.Head, err))
		case a.CodeFiles == nil:
			return s.fault(ctx, st, fmt.Errorf("codescan: the listing of %s answered no files part", tip.Name))
		}
		files, st.Unlisted = a.CodeFiles.Files, a.CodeFiles.More
	}
	held := s.readings.of(tip.ID, tip.Name)
	current := make(map[string]reading, len(files))
	for _, f := range files {
		format, ok := deployfiles.Classify(f.Path)
		if !ok {
			// A NODE OF ANOTHER BUILD LISTED IT: this build's list is the
			// one this build reads by.
			continue
		}
		if r, ok := held[f.Path]; ok && r.blob == f.OID {
			current[f.Path] = r
			continue
		}
		r := reading{blob: f.OID}
		switch {
		case !format.Parsed():
			r.parsed = Parse(tip.Name, f.Path, format, nil, now)
		case f.Size > deployfiles.MaxFileBytes:
			r.oversize = true
		case st.Reads == MaxReadsPerPass:
			st.Pending++
			continue
		default:
			raw, err := s.read(ctx, tip, f, floor, &st)
			if err != nil {
				s.readings.keep(tip.ID, tip.Name, current)
				return s.fault(ctx, st, err)
			}
			r.parsed = Parse(tip.Name, f.Path, format, raw, now)
		}
		current[f.Path] = r
	}
	st.Listed = len(files)
	if st.Pending > 0 {
		// HALF A COMMIT IS NEVER PUBLISHED: what was read is kept for the
		// next pass, which the backstop asks for since the tip is not read.
		for path, r := range held {
			if _, ok := current[path]; !ok {
				current[path] = r
			}
		}
		s.readings.keep(tip.ID, tip.Name, current)
		return s.save(ctx, st)
	}
	s.readings.keep(tip.ID, tip.Name, current)

	var want []atlas.Claim
	st.Oversize, st.Unparsed, st.Withheld, st.Truncated = 0, 0, 0, 0
	entries := map[atlas.Ref]bool{}
	for _, f := range files {
		r, ok := current[f.Path]
		if !ok {
			continue
		}
		want = append(want, r.parsed.Claims...)
		st.Withheld += r.parsed.Withheld
		st.Truncated += r.parsed.Truncated
		if r.oversize {
			st.Oversize++
		}
		if r.parsed.Unparsed {
			st.Unparsed++
		}
		for _, c := range r.parsed.Claims {
			entries[c.Subject] = true
		}
	}
	st.Entries = len(entries)
	if err := s.publish(ctx, tip.Name, want, now, &st); err != nil {
		return s.fault(ctx, st, err)
	}
	st.Tip, st.Rules, st.Completed, st.LastCompleteAt = tip.Head, Rules, true, now
	log.InfoContext(ctx, "atlas_code_read", "repo", tip.Name, "tip", tip.Head, "listed", st.Listed,
		"unlisted", st.Unlisted, "reads", st.Reads, "entries", st.Entries, "withheld", st.Withheld,
		"oversize", st.Oversize, "unparsed", st.Unparsed, "recorded", st.Recorded, "purged", st.Purged)
	return s.save(ctx, st)
}

// read is one listed file's bytes at the tip, nil for an empty file.
func (s *Scanner) read(ctx context.Context, tip gitlet.DefaultTip, f gitlet.CodeFile, floor statelog.Position,
	st *Status) ([]byte, error) {

	if f.Size == 0 {
		return nil, nil
	}
	a, err := s.code.GitRead(ctx, gitlet.GitQuery{Kind: gitlet.GitCodeFiles, RepoID: tip.ID, Fresh: freshAt(floor),
		CodeFiles: &gitlet.CodeFilesQuery{Commit: tip.Head, File: f.Path}})
	st.Calls++
	st.Reads++
	switch {
	case err != nil:
		return nil, fmt.Errorf("codescan: read a listed file of %s at %s: %w", tip.Name, tip.Head, err)
	case a.CodeFiles == nil || a.CodeFiles.File == nil || a.CodeFiles.File.OID != f.OID:
		// THE SAME COMMIT HOLDS THE SAME BLOB at a path: another answer is
		// no reading of the file that was listed.
		return nil, fmt.Errorf("codescan: a file of %s at %s was answered as another object than the one listed",
			tip.Name, tip.Head)
	}
	return a.CodeFiles.Raw, nil
}

// release withdraws everything the repository's files said, in the voice of
// the name they said it under, and forgets its standing and its readings.
func (s *Scanner) release(ctx context.Context, st Status, now time.Time) error {
	if st.Name != "" {
		if err := s.publish(ctx, st.Name, nil, now, &st); err != nil {
			return fmt.Errorf("codescan: withdraw what the files of %s declared: %w", st.Name, err)
		}
	}
	if err := s.store.Forget(ctx, st.Repo); err != nil {
		return err
	}
	s.readings.drop(st.Repo)
	s.mu.Lock()
	delete(s.standings, st.Repo)
	s.mu.Unlock()
	log.InfoContext(ctx, "atlas_code_released", "repo", st.Name, "recorded", st.Recorded,
		"detail", "the repository is no longer scanned, so what its files declared is withdrawn")
	return nil
}

// actor is who the scan's records are attributed to: the engine's own kind,
// named for the worker, and no credential, since none made the write.
var actor = iam.Actor{Name: Name, Kind: iam.ActorSystem}

// publish makes the voice of the repository named name hold exactly want:
// one record per entry that differs from the log, in a fixed order, stopping
// at the first fault. A record the log refuses because an administrator
// purged what it names is counted and the rest go on.
func (s *Scanner) publish(ctx context.Context, name string, want []atlas.Claim, now time.Time, st *Status) error {
	src := atlas.CodeSource(name)
	live, err := s.log.LiveOf(ctx, src, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return err
	}
	st.Recorded, st.Purged = 0, 0
	for _, rec := range collect.Replace(live.Claims, want) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.log.Read(ctx, atlasdomain.Call{By: actor, OpID: statelog.NewOpID(now, Name)},
			atlasdomain.ReadInput{Source: src, Entry: rec.Entry, Claims: rec.Claims, Withdrawn: rec.Withdrawn,
				ObservedAt: now})
		switch {
		case errors.Is(err, atlasdomain.ErrPurged):
			st.Purged++
		case err != nil:
			return err
		default:
			st.Recorded++
		}
	}
	return nil
}

// fault records a pass that did not finish, and answers its error.
func (s *Scanner) fault(ctx context.Context, st Status, err error) error {
	st.LastError = textcut.Within(err.Error(), collect.MaxErrorLength)
	if saveErr := s.save(context.WithoutCancel(ctx), st); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return err
}

func (s *Scanner) save(ctx context.Context, st Status) error {
	if err := s.store.Save(ctx, st); err != nil {
		return err
	}
	s.remember(st)
	return nil
}

func (s *Scanner) remember(st Status) {
	s.mu.Lock()
	s.standings[st.Repo] = st
	s.mu.Unlock()
}
