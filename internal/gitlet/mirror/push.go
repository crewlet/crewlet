package mirror

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// pushBatch is the most refspecs one `git push` carries: a thousand refspecs
// are some hundred kilobytes of argv, far inside every platform's argument
// limit, where a repository's whole tag set would not be.
const pushBatch = gitlet.MaxImportRefs

// DefaultRefs are the refs a push mirror that names none pushes: the
// repository's default branch, read when it pushes so a rename moves with it,
// and every tag.
func DefaultRefs(defaultBranch string) []string {
	return []string{"refs/heads/" + defaultBranch, "refs/tags/*"}
}

// pushed is what one push attempt moved at the other host, and what it held
// back.
type pushed struct {
	updated, deleted, waiting []string
}

// level is what a push mirror's last attempt left the other host level with,
// as a digest ([coverage]), and when it read or pushed it so
// ([Mirrors.stillLevel]). A digest, not the refs: gitlet.MaxRefsPerRepo refs
// would keep megabytes here.
type level struct {
	digest [sha256.Size]byte
	at     time.Time
}

// coverage is the digest of what a push mirror holds the other host to: its
// URL, its patterns (an added pattern can delete refs there with nothing of
// the copy's moving), every covered ref with its commit, and every branch
// held back.
func coverage(url string, patterns []string, want map[string]string, waiting []string) [sha256.Size]byte {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", url)
	for _, p := range patterns {
		fmt.Fprintf(h, "%s\n", p)
	}
	h.Write([]byte{0})
	for _, ref := range slices.Sorted(maps.Keys(want)) {
		fmt.Fprintf(h, "%s %s\n", ref, want[ref])
	}
	h.Write([]byte{0})
	for _, ref := range waiting {
		fmt.Fprintf(h, "%s\n", ref)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// stillLevel reports whether key's push mirror covers what its last attempt
// left the other host level with (sum), read so within an interval of now,
// so a nudge that moved nothing it covers dials nobody. The interval bound
// undoes a writer there that is not the mirror within gitlet.MirrorInterval.
func (m *Mirrors) stillLevel(key mirrorKey, sum [sha256.Size]byte, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.levels[key]
	return ok && l.digest == sum && now.Sub(l.at) < gitlet.MirrorInterval
}

// leveled records that the other host was read, or pushed, level with sum at
// read, before its listing ran.
func (m *Mirrors) leveled(key mirrorKey, sum [sha256.Size]byte, read time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.levels[key] = level{digest: sum, at: read}
}

// unlevel forgets what key's push mirror last left the other host level with.
func (m *Mirrors) unlevel(key mirrorKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.levels, key)
}

// push is one attempt of repoID's PUSH MIRROR (M6-D16; package doc, "The push
// mirror follows green"). A nil error is a mirror the other host now holds
// level, or found or was left level; the push's account is published
// (code_mirror_pushed) only where it moved anything.
func (m *Mirrors) push(ctx context.Context, repoID string, l listed, floor statelog.Position) (err error) {
	outcome := "current"
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// What the other host holds after a failure is not known here,
			// so the next attempt dials it whatever it covers.
			m.unlevel(mirrorKey{repo: repoID, out: true})
			outcome = "failed"
			m.log.Warn("code_mirror_attempt_failed", "repo", repoID, "attempt", "push", "error", err,
				"detail", "the other host holds what the last push left there; the next attempt reads the "+
					"rows again")
		}
		m.count(metrics.CodeMirrorRuns, metrics.Attrs{"dir": "push", "outcome": outcome})
	}()
	ans, err := m.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadMirrorPush,
		MirrorPush: &gitlet.MirrorPushQuery{RepoID: repoID}, Fresh: syncFresh(floor, l.at)})
	if err != nil {
		return fmt.Errorf("mirror: read %s's push mirror: %w", repoID, err)
	}
	if len(ans.Mirrors) == 0 || ans.Mirrors[0].To == nil {
		// Removed, or its repository: nothing is owed or remembered.
		m.forget(mirrorKey{repo: repoID, out: true})
		return nil
	}
	to := *ans.Mirrors[0].To
	// Every green commit the read names landed at or before its position, so
	// a copy synced there holds it.
	s, err := m.o.Cache.Sync(ctx, repoID, statelog.FloorAt(statelog.ReadSession, ans.Position))
	if err != nil {
		return fmt.Errorf("mirror: sync %s: %w", repoID, err)
	}
	defer s.Release()
	if s.Repo.Status != gitlet.RepoStatusActive && s.Repo.Status != gitlet.RepoStatusArchived {
		// Importing, or deleted since: nothing of it is the company's to
		// publish yet, or any more.
		return nil
	}
	roster := m.o.Roster()
	noGitlet, err := m.personWithoutGitlet(ctx, to.As, roster)
	if err != nil {
		return fmt.Errorf("mirror: %s's push mirror: %w", s.Repo.Name, err)
	}
	if why := gitlet.MirrorSeatRefusal(to.As, s.Repo, roster, noGitlet); why != "" {
		return fmt.Errorf("mirror: %s's push mirror pushes as nobody it may: %s", s.Repo.Name, why)
	}
	patterns := to.Refs
	if len(patterns) == 0 {
		patterns = DefaultRefs(s.Repo.DefaultBranch)
	}
	want, waiting, err := m.covered(ctx, s, patterns, ans.Green)
	if err != nil {
		return err
	}
	key, sum, read := mirrorKey{repo: repoID, out: true}, coverage(to.URL, patterns, want, waiting), m.now()
	if m.stillLevel(key, sum, read) {
		// Nothing it covers moved since the other host was last level with
		// it, an interval ago at most.
		return nil
	}
	remote, err := m.o.Dial(ctx, to.URL, to.Credential)
	if err != nil {
		return fmt.Errorf("mirror: dial %s's push mirror: %w", s.Repo.Name, err)
	}
	have, err := m.remoteRefs(ctx, remote, patterns)
	if err != nil {
		return err
	}
	specs, out := diffRefs(want, waiting, have)
	if len(specs) == 0 {
		m.leveled(key, sum, read)
		return nil
	}
	sc, err := m.o.Cache.NewScratch(ctx, s, statelog.NewOpID(m.now(), ""))
	if err != nil {
		return fmt.Errorf("mirror: a scratch for %s: %w", s.Repo.Name, err)
	}
	defer sc.Remove()
	for batch := range slices.Chunk(specs, pushBatch) {
		if err := m.pushBatch(ctx, s, sc, remote, batch); err != nil {
			return fmt.Errorf("mirror: push %s to %s: %w", s.Repo.Name, remote, err)
		}
	}
	m.leveled(key, sum, read)
	outcome = "pushed"
	m.announcePushed(ctx, types.CodeMirrorPushed{RepoID: repoID, RepoName: s.Repo.Name, URL: to.URL,
		As: to.As}.WithRefs(out.updated, out.deleted, out.waiting))
	return nil
}

// covered is the refs of the copy s that patterns cover, each with the commit
// the other host is to hold: a protected branch at its GREEN row's commit, or
// waiting where it has none; every other ref at the copy's tip (package doc,
// "The push mirror follows green"). Protection alone decides: an unprotected
// branch moves with no landing, so a green row it kept from a protection
// since removed is never followed.
func (m *Mirrors) covered(ctx context.Context, s cache.Synced, patterns []string, green []gitlet.GreenRow) (
	want map[string]string, waiting []string, err error) {

	held, err := m.refs(ctx, s.Dir, "")
	if err != nil {
		return nil, nil, err
	}
	greens := make(map[string]string, len(green))
	for _, g := range green {
		greens[g.TargetRef] = g.CommitOID
	}
	want = map[string]string{}
	for ref, oid := range held {
		if !matchesAny(patterns, ref) {
			continue
		}
		if strings.HasPrefix(ref, "refs/heads/") && protected(s.Repo.Protections, ref) {
			g, ok := greens[ref]
			if !ok {
				waiting = append(waiting, ref)
				continue
			}
			oid = g
		}
		want[ref] = oid
	}
	slices.Sort(waiting)
	return want, waiting, nil
}

// matchesAny reports whether ref matches one of patterns (gitref's protection
// grammar, which a push mirror's refs are written in).
func matchesAny(patterns []string, ref string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return gitref.MatchPattern(p, ref) })
}

// protected reports whether a rule of rules protects ref.
func protected(rules []gitlet.PushRule, ref string) bool {
	return slices.ContainsFunc(rules, func(r gitlet.PushRule) bool { return gitref.MatchPattern(r.Pattern, ref) })
}

// remoteRefs is every ref the other host holds that patterns cover, with the
// object each names: ONE `ls-remote --refs` (peeled tags left out, so a tag is
// its ref's object, as the copy's for-each-ref lists it) of the arms the
// patterns reach, `--branches` and `--tags` (config's validateMirrorRefs
// allows no others), which protocol v2 sends as ref prefixes, so a forge's
// refs/pull/* is never advertised. A
// pattern argument would not do it: ls-remote matches it on the client, after
// the whole advertisement. STREAMED (gitproc.Lines), never captured: the list
// grows with the other host, past gitproc.CaptureMax.
func (m *Mirrors) remoteRefs(ctx context.Context, remote gitproc.Remote, patterns []string) (map[string]string,
	error) {

	var arms []string
	for _, arm := range []struct{ prefix, flag string }{{"refs/heads/", "--branches"}, {"refs/tags/", "--tags"}} {
		if slices.ContainsFunc(patterns, func(p string) bool { return strings.HasPrefix(p, arm.prefix) }) {
			arms = append(arms, arm.flag)
		}
	}
	out := map[string]string{}
	if len(arms) == 0 {
		// No pattern reaches a branch or a tag, so the mirror covers nothing
		// there — and ls-remote with neither flag would list every ref.
		return out, nil
	}
	lines := gitproc.NewLines(func(line []byte) error {
		oid, ref, ok := strings.Cut(string(line), "\t")
		if !ok || !gitref.ValidOID(oid) {
			return fmt.Errorf("mirror: ls-remote printed %q", line)
		}
		if matchesAny(patterns, ref) {
			out[ref] = oid
		}
		return nil
	})
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Args:   append([]string{"ls-remote", "--refs"}, append(arms, remote.URL())...),
		Remote: &remote, Stdout: lines,
		Class: gitproc.ClassTransfer, Share: share, Deadline: gitproc.RemoteDeadline(0, 0),
	})
	if err == nil {
		err = lines.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("mirror: list the refs of %s: %w\n%s", remote, err, res.Stderr)
	}
	return out, nil
}

// diffRefs is the refspecs, in ref order, that bring the other host from have
// level with want — `<oid>:<ref>` for each ref it holds at another object or
// not at all, `:<ref>` for each it holds that want lacks, a waiting branch
// never deleted (it is held back) — and the account of what they move.
func diffRefs(want map[string]string, waiting []string, have map[string]string) ([]string, pushed) {
	var (
		specs []string
		out   = pushed{waiting: waiting}
	)
	for _, ref := range slices.Sorted(maps.Keys(want)) {
		if have[ref] != want[ref] {
			specs = append(specs, want[ref]+":"+ref)
			out.updated = append(out.updated, ref+" "+want[ref])
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(have)) {
		if _, ok := want[ref]; ok || slices.Contains(waiting, ref) {
			continue
		}
		specs = append(specs, ":"+ref)
		out.deleted = append(out.deleted, ref)
	}
	return specs, out
}

// pushBatch pushes specs, forced, to remote from the scratch sc over the copy
// s, never from the copy, which only a sync writes. `--porcelain` names each
// ref's fate, so a ref the other host rejected (a protection, a hook) is named
// in the error rather than read from a sentence.
func (m *Mirrors) pushBatch(ctx context.Context, s cache.Synced, sc *cache.Scratch, remote gitproc.Remote,
	specs []string) error {

	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassTransfer, Share: share, Remote: &remote,
		Args: append([]string{"push", "--force", "--porcelain", "--no-verify", "--quiet", remote.URL()},
			specs...),
		// Exit 1 is a push some ref of which the other host rejected:
		// --porcelain says which, below.
		Answers:  []int{1},
		Deadline: gitproc.RemoteDeadline(s.Repo.LiveBytes, s.Repo.LiveObjects),
	})
	if err != nil {
		return fmt.Errorf("%w\n%s", err, res.Stderr)
	}
	var rejected []string
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		line := lines.Text()
		if !strings.HasPrefix(line, "!\t") {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		what := f[1]
		if len(f) == 3 {
			what += " (" + f[2] + ")"
		}
		rejected = append(rejected, what)
	}
	if err := lines.Err(); err != nil {
		return err
	}
	switch {
	case len(rejected) > 0:
		return fmt.Errorf("the other host rejected %s", strings.Join(rejected, ", "))
	case res.Exit != 0:
		return errors.New("git push exited 1 naming no rejected ref: " + strings.TrimSpace(res.Stderr))
	}
	return nil
}

// announcePushed publishes the push's account (code_mirror_pushed); a stream
// that does not take it is logged, and the push stands.
func (m *Mirrors) announcePushed(ctx context.Context, ev types.CodeMirrorPushed) {
	m.announce(ctx, ev.RepoID, events.New(ev, events.TraceContext{}), "code_mirror_pushed_unpublished",
		"the event log does not hold this push; the other host holds what it moved")
}

// personWithoutGitlet is whether the seat a push mirror pushes as is a
// person's whom the identity directory says uses no Gitlet (held by nobody,
// by one who may not act, or by one not given the app), read before every
// push (gitlet.MirrorSeatRefusal, ADR-0110); false for an agent's seat or a
// handle the chart lacks, which the chart alone judges. A directory that
// cannot answer fails the attempt: never a push published as somebody nobody
// could say may read it.
func (m *Mirrors) personWithoutGitlet(ctx context.Context, as string, roster gitlet.Roster) (bool, error) {
	if roster == nil {
		return false, nil
	}
	if seat, ok := roster.Seat(as); !ok || !seat.Human {
		return false, nil
	}
	p, err := m.o.People.Requester(ctx, as)
	if err != nil {
		return false, fmt.Errorf("the identity directory could not say whether the person holding %q uses "+
			"Gitlet, which a push mirror pushing as their seat is judged by: %w", as, err)
	}
	return !p.Can(iam.GrantGitlet), nil
}
