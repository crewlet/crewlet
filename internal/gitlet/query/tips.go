package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// tipsMemoFormat names what a tips entry of this build holds (encodeTipFact,
// encodeSides) and is the first part of its key, as memoFormat is a page's:
// TestATipsEntrysShapeIsItsFormat holds it to a fingerprint of gitlet.Tip.
const tipsMemoFormat = "tips/2"

// tipFactKey keys an object's facts, and tipSidesKey the counts of a tip's
// commit against a base commit.
func tipFactKey(oid string) cache.MemoKey { return cache.NewMemoKey(tipsMemoFormat, "fact", oid) }

func tipSidesKey(base, tip string) cache.MemoKey {
	return cache.NewMemoKey(tipsMemoFormat, "sides", base, tip)
}

// tipsWarm is what recallTips found in the memo, which tipsRead does not
// compute again, and what tipsRead computed beside it.
type tipsWarm struct {
	facts map[string]gitlet.Tip // by oid: every field but the counts
	sides map[[2]string][2]int  // by (base commit, tip commit): behind, ahead
}

// recallTips is the memo's answer to a tips read, asked before any git runs:
// every fact and count of the tips from the cursor on, or none (false), with
// what it did find kept for tipsRead.
func (r *read) recallTips(ctx context.Context, at cursor) (gitlet.GitAnswer, bool, error) {
	q := r.q.Tips
	start := at.a
	if start >= len(q.OIDs) {
		return gitlet.GitAnswer{}, false, fmt.Errorf("%w: cursor t:%d is past the %d tips the read names",
			gitlet.ErrBadCursor, start, len(q.OIDs))
	}
	r.tips = &tipsWarm{facts: map[string]gitlet.Tip{}, sides: map[[2]string][2]int{}}
	if r.grant() != nil || (q.Base != "" && !gitref.ValidOID(q.Base)) {
		//nolint:nilerr // a refused grant is objects' to judge at the log's head, and an abbreviated base is expanded there.
		return gitlet.GitAnswer{}, false, nil
	}
	whole := true
	for _, id := range r.tipsNeeded(start, q.Base) {
		body, ok := r.s.o.Cache.MemoGet(r.synced, tipFactKey(id))
		if !ok {
			whole = false
			continue
		}
		tip, err := decodeTipFact(body)
		if err != nil || tip.OID != id {
			whole = false
			continue
		}
		r.tips.facts[id] = tip
	}
	if base, ok := r.tips.facts[q.Base]; ok && base.PeeledType == "commit" {
		for _, id := range q.OIDs[start:] {
			tip, known := r.tips.facts[id]
			if !known || tip.PeeledType != "commit" || tip.Peeled == base.Peeled {
				continue
			}
			pair := [2]string{base.Peeled, tip.Peeled}
			if _, held := r.tips.sides[pair]; held {
				continue
			}
			body, found := r.s.o.Cache.MemoGet(r.synced, tipSidesKey(pair[0], pair[1]))
			if behind, ahead, err := decodeSides(body); found && err == nil {
				r.tips.sides[pair] = [2]int{behind, ahead}
			} else {
				whole = false
			}
		}
	} else if q.Base != "" && ok {
		// A base that peels to no commit is refused by tipsRead.
		whole = false
	}
	if !whole {
		return gitlet.GitAnswer{}, false, nil
	}
	a, err := r.tipsPage(ctx, start, q.Base, nil, nil)
	if err != nil {
		return gitlet.GitAnswer{}, false, err
	}
	a.Memo = true
	return a, true, nil
}

// tipsNeeded is every object whose facts a page from start needs: the tips
// from start on, then the base.
func (r *read) tipsNeeded(start int, base string) []string {
	ids := r.q.Tips.OIDs[start:]
	if base != "" && !slices.Contains(ids, base) {
		ids = append(ids[:len(ids):len(ids)], base)
	}
	return ids
}

// tipsRead answers a tips read under the share: the facts the memo did not
// hold, from one tag batch and one log, then the page, counting each tip the
// memo did not.
func (r *read) tipsRead(ctx context.Context, objs found, at cursor) (gitlet.GitAnswer, error) {
	q := r.q.Tips
	start := at.a
	if r.tips == nil {
		r.tips = &tipsWarm{facts: map[string]gitlet.Tip{}, sides: map[[2]string][2]int{}}
	}
	base := ""
	if q.Base != "" {
		o := objs.revs[q.Base]
		if o.peeledKind != "commit" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: tips.base: %s names a %s; tips are counted against a commit",
				gitlet.ErrInvalid, q.Base, o.peeledKind)
		}
		base = o.id
	}
	b := r.s.o.Cache.Memo(r.synced)
	defer b.Discard()

	byID := make(map[string]object, len(objs.revs))
	for _, o := range objs.revs {
		byID[o.id] = o
	}
	var missing []string
	for _, id := range r.tipsNeeded(start, base) {
		if _, ok := r.tips.facts[id]; !ok {
			missing = append(missing, id)
		}
	}
	var tags, commits []string
	for _, id := range missing {
		o := byID[id]
		if o.kind == "tag" {
			tags = append(tags, id)
		}
		if o.peeledKind == "commit" && !slices.Contains(commits, o.peeled) {
			commits = append(commits, o.peeled)
		}
	}
	tagged, err := r.readTags(ctx, tags)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	listed, err := r.readCommits(ctx, "log of the tips' commits", commits)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	for _, id := range missing {
		o := byID[id]
		tip := gitlet.Tip{OID: id, Type: o.kind, Peeled: o.peeled, PeeledType: o.peeledKind, Tag: tagged[id]}
		if o.peeledKind == "commit" {
			c := listed[o.peeled]
			tip.Commit = &c
		}
		r.tips.facts[id] = tip
		b.Put(tipFactKey(id), encodeTipFact(tip))
	}
	a, err := r.tipsPage(ctx, start, base, r.sides, b)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	b.Place(func(int) []byte { return nil })
	return a, nil
}

// tipsPage cuts the page of tips from start on, every fact in r.tips: each
// count the memo did not hold asked of count and put in b, nil on a warm read
// that holds them all.
func (r *read) tipsPage(ctx context.Context, start int, baseID string,
	count func(ctx context.Context, base, tip string) (int, int, error), b *cache.MemoBatch) (gitlet.GitAnswer, error) {

	q := r.q.Tips
	baseCommit := ""
	var named []string
	if baseID != "" {
		baseCommit = r.tips.facts[baseID].Peeled
		named = []string{baseCommit}
	}
	// Each tip's push is named as the page takes it (pushCost), so the page
	// carries the pushes of its own commits and the base's alone.
	r.index(named)
	envelope := gitlet.GitAnswer{Kind: gitlet.GitTips, Next: widestCursor('t', 1),
		Tips: &gitlet.TipsAnswer{Base: baseCommit, Tips: []gitlet.Tip{}}}
	left := r.room(envelope)
	a := gitlet.GitAnswer{Kind: gitlet.GitTips, Tips: &gitlet.TipsAnswer{Base: baseCommit, Tips: []gitlet.Tip{}}}
	for i := start; i < len(q.OIDs); i++ {
		tip := r.tips.facts[q.OIDs[i]]
		outOfTime := false
		if baseCommit != "" && tip.PeeledType == "commit" {
			pair := [2]string{baseCommit, tip.Peeled}
			sides, held := r.tips.sides[pair]
			switch {
			case tip.Peeled == baseCommit:
				held = true
			case held:
			case count == nil:
				return gitlet.GitAnswer{}, fmt.Errorf("%w: the counts of tip %s were not recalled", errUnparsed,
					tip.OID)
			default:
				behind, ahead, err := count(ctx, pair[0], pair[1])
				switch {
				case errors.Is(err, gitlet.ErrSliceTooLarge) && i > start:
					// Out of time: the next page counts it first, with a whole budget.
					a.Next = formatCursor(cursor{letter: 't', a: i})
					return a, nil
				case errors.Is(err, gitlet.ErrSliceTooLarge):
					outOfTime = true
				case err != nil:
					return gitlet.GitAnswer{}, err
				default:
					sides, held = [2]int{behind, ahead}, true
					r.tips.sides[pair] = sides
					b.Put(tipSidesKey(pair[0], pair[1]), encodeSides(behind, ahead))
				}
			}
			if held {
				tip.Behind, tip.Ahead = &sides[0], &sides[1]
			}
			tip.CountsOutOfTime = outOfTime
		}
		cost := jsonLen(tip)
		if len(a.Tips.Tips) > 0 {
			cost++ // the comma before it
		}
		pushed, name := 0, func() {}
		if tip.Commit != nil {
			pushed, name = r.pushCost(tip.Peeled)
		}
		if cost+pushed > left {
			if len(a.Tips.Tips) == 0 {
				return gitlet.GitAnswer{}, fmt.Errorf("%w: tip %s encodes to %d bytes, past the %d an empty page "+
					"leaves", errUnparsed, tip.OID, cost+pushed, left)
			}
			a.Next = formatCursor(cursor{letter: 't', a: i})
			return a, nil
		}
		name()
		a.Tips.Tips = append(a.Tips.Tips, tip)
		left -= cost + pushed
		if outOfTime {
			if i+1 < len(q.OIDs) {
				a.Next = formatCursor(cursor{letter: 't', a: i + 1})
			}
			return a, nil
		}
	}
	return a, nil
}

// readTags is each annotated tag of ids' own facts, from ONE cat-file batch.
func (r *read) readTags(ctx context.Context, ids []string) (map[string]*gitlet.TipTag, error) {
	out := make(map[string]*gitlet.TipTag, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	deadline, err := r.budget()
	if err != nil {
		return nil, err
	}
	var stdin bytes.Buffer
	for _, id := range ids {
		stdin.WriteString(id)
		stdin.WriteByte('\n')
	}
	stream := gitref.NewTagStream(gitref.TagCaps{Person: gitlet.MaxLogPersonBytes, Subject: gitlet.MaxLogSubjectBytes},
		func(t gitref.Tag) error {
			if i := len(out); i >= len(ids) || t.OID != ids[i] {
				return fmt.Errorf("%w: cat-file answered tag %s out of the order asked", errUnparsed, t.OID)
			}
			out[t.OID] = tipTag(t)
			return nil
		})
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", "--batch"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: stream, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if res.Damage == nil && (errors.Is(err, errUnparsed) || errors.Is(err, gitref.ErrNotATag)) {
		r.charge(res)
		return nil, r.unparsed(ctx, "tags", err)
	}
	if err = r.failed(ctx, "read of the tags the read names", res, err, nil); err != nil {
		return nil, err
	}
	if err = stream.Close(); err != nil {
		return nil, r.unparsed(ctx, "tags", err)
	}
	if len(out) != len(ids) {
		return nil, r.unparsed(ctx, "tags", fmt.Errorf("%w: cat-file answered %d of the %d tags asked",
			errUnparsed, len(out), len(ids)))
	}
	return out, nil
}

// tipTag is a tag object's facts as a tip carries them, each unbounded field
// described past its bound as a history's are.
func tipTag(t gitref.Tag) *gitlet.TipTag {
	held := func(h gitref.Held, bound int) (string, int) {
		text, size, _ := (&heldText{buf: h.Bytes, n: h.Len, cap: bound}).described(bound)
		return text, size
	}
	tag := &gitlet.TipTag{}
	tag.Tagger, tag.TaggerBytes = held(t.TaggerName, gitlet.MaxLogPersonBytes)
	tag.TaggerEmail, tag.TaggerEmailBytes = held(t.TaggerEmail, gitlet.MaxLogPersonBytes)
	tag.Subject, tag.SubjectBytes = held(t.Subject, gitlet.MaxLogSubjectBytes)
	if !t.TaggedAt.IsZero() {
		at := t.TaggedAt
		tag.TaggedAt = &at
	}
	return tag
}

// readCommits is each commit of ids as a history lists it, from ONE log that
// walks none of their parents; what names the log in an error. A commit dated
// past the year 9999 fails it, unparsed: no answer can carry such a date.
func (r *read) readCommits(ctx context.Context, what string, ids []string) (map[string]gitlet.LogCommit, error) {
	out := make(map[string]gitlet.LogCommit, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	deadline, err := r.budget()
	if err != nil {
		return nil, err
	}
	stream := newLogStream(nil, func(rec *logRecord) error {
		if !slices.Contains(ids, rec.oid) {
			return fmt.Errorf("%w: git log listed commit %s, which the read did not ask", errUnparsed, rec.oid)
		}
		if rec.authored.After(lastJSONTime) || rec.committed.After(lastJSONTime) {
			return fmt.Errorf("%w: commit %s is dated past the year 9999, which an answer cannot carry",
				errUnparsed, rec.oid)
		}
		out[rec.oid], _ = rec.commit()
		return nil
	})
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"log", "-z", "--no-walk=unsorted", "--no-mailmap", "--no-color", "--no-notes", logFormat}, ids...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: stream, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if res.Damage == nil && errors.Is(err, errUnparsed) {
		r.charge(res)
		return nil, r.unparsed(ctx, what, err)
	}
	if err = r.failed(ctx, what, res, err, nil); err != nil {
		return nil, err
	}
	if err = stream.close(); err != nil {
		return nil, r.unparsed(ctx, what, err)
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, r.unparsed(ctx, what, fmt.Errorf("%w: git log did not list commit %s", errUnparsed, id))
		}
	}
	return out, nil
}

// lastJSONTime is the last instant encoding/json writes a time.Time at.
var lastJSONTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// encodeTipFact is a tip's facts as a memo entry holds them, its counts no
// part of them; decodeTipFact reads them back.
func encodeTipFact(tip gitlet.Tip) []byte {
	var w pageWriter
	w.str(tip.OID)
	w.str(tip.Type)
	w.str(tip.Peeled)
	w.str(tip.PeeledType)
	w.flag(tip.Tag != nil)
	if t := tip.Tag; t != nil {
		w.str(t.Tagger)
		w.int(t.TaggerBytes)
		w.str(t.TaggerEmail)
		w.int(t.TaggerEmailBytes)
		w.flag(t.TaggedAt != nil)
		if t.TaggedAt != nil {
			w.time(*t.TaggedAt)
		}
		w.str(t.Subject)
		w.int(t.SubjectBytes)
	}
	w.flag(tip.Commit != nil)
	if tip.Commit != nil {
		appendLogCommit(&w, *tip.Commit)
	}
	return w
}

func decodeTipFact(body []byte) (gitlet.Tip, error) {
	r := &pageReader{s: string(body)}
	tip := gitlet.Tip{OID: r.str(), Type: r.str(), Peeled: r.str(), PeeledType: r.str()}
	if r.flag() {
		t := &gitlet.TipTag{Tagger: r.str(), TaggerBytes: r.int(), TaggerEmail: r.str(),
			TaggerEmailBytes: r.int()}
		if r.flag() {
			at := r.time()
			t.TaggedAt = &at
		}
		t.Subject, t.SubjectBytes = r.str(), r.int()
		tip.Tag = t
	}
	if r.flag() {
		c := readLogCommit(r)
		tip.Commit = &c
	}
	if r.err != nil || r.s != "" {
		return gitlet.Tip{}, errMemoPage
	}
	return tip, nil
}

// encodeSides is a tip's counts against a base as a memo entry holds them;
// decodeSides reads them back.
func encodeSides(behind, ahead int) []byte {
	var w pageWriter
	w.int(behind)
	w.int(ahead)
	return w
}

func decodeSides(body []byte) (behind, ahead int, err error) {
	r := &pageReader{s: string(body)}
	behind, ahead = r.int(), r.int()
	if r.err != nil || r.s != "" {
		return 0, 0, errMemoPage
	}
	return behind, ahead, nil
}
