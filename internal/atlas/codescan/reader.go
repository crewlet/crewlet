package codescan

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reader is the one reading of a repository's listed files, whoever holds
// the repository: Gitlet's scan and a forge's pass both read through it, so
// which file is read, which is only counted and what is published are
// decided once.
type reader struct {
	log      collect.Log
	readings *readings
}

// listedFile is one file of a tree as its host listed it. Size is negative
// where the host's listing does not say it.
type listedFile struct {
	path string
	oid  string
	size int64
}

// readFile reads one listed file whole: its bytes, or oversize for a file
// past [deployfiles.MaxFileBytes], of which it answers nothing.
type readFile func(ctx context.Context, f listedFile) (raw []byte, oversize bool, err error)

// budget is what one pass may still read: files, and bytes of them. A zero
// bound is no bound.
type budget struct {
	reads, maxReads int
	bytes, maxBytes int
}

// spent reports a pass that starts no further file.
func (b *budget) spent() bool {
	return (b.maxReads > 0 && b.reads >= b.maxReads) || (b.maxBytes > 0 && b.bytes >= b.maxBytes)
}

// settle reads what changed among one tree's files and, once every listed
// file is read, publishes what differs from the log. It answers false where
// files still wait for a later pass, and counts what it did on st.
//
// THE CLOSED LIST IS HELD HERE, for every host: a file that is not on it is
// skipped whoever listed it, and a format that is listed by its presence
// (a Dockerfile, a Procfile) is never read.
func (r *reader) settle(ctx context.Context, id string, repo Repo, files []listedFile, read readFile, b *budget,
	st *Status, now time.Time) (bool, error) {

	held := r.readings.of(id, repo.Voice)
	current := make(map[string]reading, len(files))
	for _, f := range files {
		format, ok := deployfiles.Classify(f.path)
		if !ok {
			// A NODE OF ANOTHER BUILD, OR A FORGE, LISTED IT: this build's
			// list is the one this build reads by.
			continue
		}
		if rd, ok := held[f.path]; ok && rd.blob == f.oid {
			current[f.path] = rd
			continue
		}
		rd := reading{blob: f.oid}
		switch {
		case !format.Parsed() || f.size == 0:
			rd.parsed = Parse(repo, f.path, format, nil, now)
		case f.size > deployfiles.MaxFileBytes:
			rd.oversize = true
		case b.spent():
			st.Pending++
			continue
		default:
			raw, oversize, err := read(ctx, f)
			st.Calls++
			st.Reads++
			st.Bytes += len(raw)
			b.reads++
			b.bytes += len(raw)
			if err != nil {
				r.readings.keep(id, repo.Voice, current)
				return false, err
			}
			if rd.oversize = oversize; !oversize {
				rd.parsed = Parse(repo, f.path, format, raw, now)
			}
		}
		current[f.path] = rd
	}
	st.Listed = len(files)
	if st.Pending > 0 {
		// HALF A COMMIT IS NEVER PUBLISHED: what was read is kept for the
		// next pass, which is owed since the tip is not read.
		for path, rd := range held {
			if _, ok := current[path]; !ok {
				current[path] = rd
			}
		}
		r.readings.keep(id, repo.Voice, current)
		return false, nil
	}
	r.readings.keep(id, repo.Voice, current)

	var want []atlas.Claim
	st.Oversize, st.Unparsed, st.Withheld, st.Truncated = 0, 0, 0, 0
	entries := map[atlas.Ref]bool{}
	for _, f := range files {
		rd, ok := current[f.path]
		if !ok {
			continue
		}
		want = append(want, rd.parsed.Claims...)
		st.Withheld += rd.parsed.Withheld
		st.Truncated += rd.parsed.Truncated
		if rd.oversize {
			st.Oversize++
		}
		if rd.parsed.Unparsed {
			st.Unparsed++
		}
		for _, c := range rd.parsed.Claims {
			entries[c.Subject] = true
		}
	}
	st.Entries = len(entries)
	if err := r.publish(ctx, repo.Voice, want, now, st); err != nil {
		return false, err
	}
	return true, nil
}

// actor is who the scan's records are attributed to: the engine's own kind,
// named for the worker, and no credential, since none made the write.
var actor = iam.Actor{Name: Name, Kind: iam.ActorSystem}

// publish makes the voice hold exactly want: one record per entry that
// differs from the log, in a fixed order, stopping at the first fault. A
// record the log refuses because an administrator purged what it names is
// counted and the rest go on.
func (r *reader) publish(ctx context.Context, voice string, want []atlas.Claim, now time.Time, st *Status) error {
	src := atlas.CodeSource(voice)
	live, err := r.log.LiveOf(ctx, src, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return err
	}
	st.Recorded, st.Purged = 0, 0
	for _, rec := range collect.Replace(live.Claims, want) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := r.log.Read(ctx, atlasdomain.Call{By: actor, OpID: statelog.NewOpID(now, Name)},
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
