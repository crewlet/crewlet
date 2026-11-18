package query

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitref"
)

// THE COPY'S TAGS (gitlet.GitTags): a page newest first from the tag index,
// with no git, its cursor the last tag's key; and the tags naming each commit
// a history's page lists, costed as its push is (package doc, "The copy's
// tags").

// tagsRefPrefix is the namespace every tag's ref is under.
const tagsRefPrefix = "refs/tags/"

// tagsCursorEncoding is a tags cursor's name: unpadded base64url, strict.
var tagsCursorEncoding = base64.RawURLEncoding.Strict()

// parseTagsCursor reads a tags cursor, "d:<seconds>:<name>": the last row's
// date and its name after refs/tags/ in unpadded base64url, canonical as every
// cursor is; "" is nil.
func parseTagsCursor(raw string) (*cache.TagKey, error) {
	if raw == "" {
		return nil, nil
	}
	bad := errBadCursor(raw, gitlet.GitTags, "d:<seconds>:<name>")
	rest, ok := strings.CutPrefix(raw, "d:")
	if !ok {
		return nil, bad
	}
	secs, enc, ok := strings.Cut(rest, ":")
	if !ok {
		return nil, bad
	}
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != secs {
		return nil, bad
	}
	name, err := tagsCursorEncoding.DecodeString(enc)
	if err != nil || len(name) == 0 || !gitref.ValidRef(tagsRefPrefix+string(name)) {
		return nil, bad
	}
	return &cache.TagKey{Date: n, Ref: tagsRefPrefix + string(name)}, nil
}

// formatTagsCursor is k as a Next.
func formatTagsCursor(k cache.TagKey) string {
	return "d:" + strconv.FormatInt(k.Date, 10) + ":" +
		tagsCursorEncoding.EncodeToString([]byte(strings.TrimPrefix(k.Ref, tagsRefPrefix)))
}

// widestTagsCursor is a tags cursor at its longest: 349 bytes.
var widestTagsCursor = formatTagsCursor(cache.TagKey{Date: 1<<63 - 1,
	Ref: tagsRefPrefix + strings.Repeat("x", gitref.MaxRefNameBytes-len(tagsRefPrefix))})

// tagsRead answers a tags read from the copy's tag index, with no git: a page
// newest first from the cursor, cut by the limit and the answer's bytes.
func (r *read) tagsRead(ctx context.Context) (gitlet.GitAnswer, error) {
	// THE GRANT, judged at the log's head before a refusal is concluded from
	// a copy that may be behind it (read.objects).
	for {
		refused := r.grant()
		if refused == nil {
			break
		}
		if r.linear {
			return gitlet.GitAnswer{}, refused
		}
		if spent := r.resync(); spent != nil {
			_, _, principal, scope := r.floorWords()
			return gitlet.GitAnswer{}, fmt.Errorf("%w %s", refused,
				gitlet.NotRejudged(spent, principal, scope, time.Now()))
		}
		if err := r.lookAtHead(ctx); err != nil {
			return gitlet.GitAnswer{}, err
		}
	}
	x, ok := r.s.o.Cache.Tags(r.synced)
	if !ok {
		return gitlet.GitAnswer{}, r.notIndexed()
	}
	defer func() { _ = x.Close() }()        // read-only
	after, _ := parseTagsCursor(r.q.Cursor) // cursorOf held it to the grammar
	limit := r.q.Tags.PageLimit()
	left := r.room(gitlet.GitAnswer{Kind: gitlet.GitTags, Next: widestTagsCursor,
		Tags: &gitlet.TagsAnswer{Tags: []gitlet.TagRow{}, Total: widest}})
	page := &gitlet.TagsAnswer{Tags: []gitlet.TagRow{}, Total: x.Len()}
	var next string
	var cut error
	err := x.Rows(after, func(e cache.TagEntry) bool {
		row := tagRow(e)
		cost := jsonLen(row)
		if len(page.Tags) > 0 {
			cost++ // the comma before it
		}
		if len(page.Tags) == limit || cost > left {
			if len(page.Tags) == 0 {
				cut = fmt.Errorf("%w: tag %s encodes to %d bytes, past the %d an empty page leaves",
					errUnparsed, e.Ref, cost, left)
				return false
			}
			last := page.Tags[len(page.Tags)-1]
			next = formatTagsCursor(cache.TagKey{Date: tagDate(last), Ref: last.Ref})
			return false
		}
		page.Tags = append(page.Tags, row)
		left -= cost
		return true
	})
	if err != nil {
		// The index has set itself aside and asked a round to build it again.
		return gitlet.GitAnswer{}, r.notIndexed()
	}
	if cut != nil {
		return gitlet.GitAnswer{}, r.unparsed(ctx, "tags", cut)
	}
	return gitlet.GitAnswer{Kind: gitlet.GitTags, Next: next, Tags: page}, nil
}

// notIndexed is a tags read on a copy keeping no tag index it can read now.
func (r *read) notIndexed() error {
	return r.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s keeps no tag index of repository %s it can read "+
		"now and is building one; another Gitlet node answers meanwhile", r.s.o.NodeID, r.q.RepoID))
}

// tagRow is e as a page lists it: its date where git dates it, one past
// gitlet.LastJSONSecond, which encoding/json cannot write, in unix seconds
// (DateUnix).
func tagRow(e cache.TagEntry) gitlet.TagRow {
	row := gitlet.TagRow{Ref: e.Ref, OID: e.OID, Type: e.Type, Peeled: e.Peeled, PeeledType: e.PeeledType}
	switch {
	case !e.Dated:
	case e.Date <= gitlet.LastJSONSecond:
		d := time.Unix(e.Date, 0).UTC()
		row.Date = &d
	default:
		row.DateUnix = e.Date
	}
	return row
}

// tagDate is a row's key date: 0 where it is undated.
func tagDate(row gitlet.TagRow) int64 {
	switch {
	case row.Date != nil:
		return row.Date.Unix()
	default:
		return row.DateUnix
	}
}

// openTags opens the tag index of the read's pin for a stamped history read,
// once a pin: a read pinned anew since (lookAtHead) opens the new pin's.
func (r *read) openTags() {
	if r.q.Kind != gitlet.GitLog || r.q.Unstamped {
		return
	}
	if r.tagsTried && r.tagsDir == r.synced.Dir && r.tagsAt == r.synced.Position {
		return
	}
	r.closeTags()
	r.tagIndex, _ = r.s.o.Cache.Tags(r.synced)
	r.tagsDir, r.tagsAt, r.tagsTried, r.tagsOf = r.synced.Dir, r.synced.Position, true, nil
}

// closeTags closes the tag index the read opened, if any.
func (r *read) closeTags() {
	if r.tagIndex != nil {
		_ = r.tagIndex.Close() // read-only
		r.tagIndex = nil
	}
}

// tagsOfCommit is commit id's tags, looked up once a read; false where the
// index failed, which drops the read's tag decoration.
func (r *read) tagsOfCommit(id string) (gitlet.CommitTags, bool) {
	if t, ok := r.tagsOf[id]; ok {
		return t, true
	}
	refs, more, err := r.tagIndex.Commit(id, gitlet.MaxCommitTags)
	if err != nil {
		// The index has set itself aside: the read names no tag at all.
		r.closeTags()
		r.tagged = nil
		return gitlet.CommitTags{}, false
	}
	t := gitlet.CommitTags{Refs: refs, More: more}
	if r.tagsOf == nil {
		r.tagsOf = map[string]gitlet.CommitTags{}
	}
	r.tagsOf[id] = t
	return t, true
}

// tagCost is what naming commit id's tags adds to a page's stamps, as
// pushCost is for its push.
func (r *read) tagCost(id string) int {
	if r.tagIndex == nil || r.tagged == nil || r.isNamed(id) {
		return 0
	}
	t, ok := r.tagsOfCommit(id)
	if !ok || len(t.Refs) == 0 {
		return 0
	}
	entry := map[string]gitlet.CommitTags{id: t}
	if len(r.tagged.Commits) == 0 {
		// The first entry brings the field itself.
		return jsonLen(gitlet.TagMarks{Commits: entry}) - jsonLen(gitlet.TagMarks{})
	}
	return jsonLen(entry) - len("{}") + len(",")
}

// markTags names commit id's tags in the read's decoration.
func (r *read) markTags(id string) {
	if r.tagIndex == nil || r.tagged == nil {
		return
	}
	t, ok := r.tagsOfCommit(id)
	if !ok || len(t.Refs) == 0 {
		return
	}
	if r.tagged.Commits == nil {
		r.tagged.Commits = map[string]gitlet.CommitTags{}
	}
	r.tagged.Commits[id] = t
}

// isNamed reports whether commit id is decorated already.
func (r *read) isNamed(id string) bool {
	for _, named := range r.named {
		if named == id {
			return true
		}
	}
	return false
}
