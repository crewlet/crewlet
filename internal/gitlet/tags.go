package gitlet

import (
	"fmt"
	"slices"
	"time"
)

// MaxTagsLimit is the most tags one page lists: a screen of them, the page
// cut by its bytes too.
const MaxTagsLimit = 100

// DefaultTagsLimit is a page's tags where the read names no limit.
const DefaultTagsLimit = 25

// MaxCommitTags is the most tags an answer names one commit with, newest
// first: a history row draws a few chips, and CommitTags.More counts the rest.
const MaxCommitTags = 8

// LastJSONSecond is 9999-12-31T23:59:59Z in unix seconds: the last instant
// encoding/json writes, refusing every later year.
const LastJSONSecond = 253402300799

// ObjectType is a git object's type as git names it.
type ObjectType string

const (
	ObjectCommit ObjectType = "commit"
	ObjectTree   ObjectType = "tree"
	ObjectBlob   ObjectType = "blob"
	ObjectTag    ObjectType = "tag"
)

// Valid reports one of git's four object types.
func (t ObjectType) Valid() bool {
	return slices.Contains([]ObjectType{ObjectCommit, ObjectTree, ObjectBlob, ObjectTag}, t)
}

// TagsQuery is a page of a repository's tags, newest first (GitTags); its
// cursor is the query's.
type TagsQuery struct {
	// Limit is the most tags the page lists: 1 to MaxTagsLimit, 0 for DefaultTagsLimit.
	Limit int `json:"limit,omitempty"`
}

func (q *TagsQuery) validate() error {
	if q.Limit < 0 || q.Limit > MaxTagsLimit {
		return fmt.Errorf("%w: tags.limit: %d is outside 0 to %d (0 is %d)", ErrInvalid, q.Limit, MaxTagsLimit,
			DefaultTagsLimit)
	}
	return nil
}

// PageLimit is the most tags a page of q lists.
func (q *TagsQuery) PageLimit() int {
	if q.Limit == 0 {
		return DefaultTagsLimit
	}
	return q.Limit
}

// TagsAnswer is one page of the copy's tags, newest first by their own date.
type TagsAnswer struct {
	Tags []TagRow `json:"tags"`
	// Total is how many tags the copy's refs hold, every page.
	Total int `json:"total"`
}

// TagRow is one tag: its object, what that peels to, and git's creatordate.
type TagRow struct {
	// Ref is the whole ref; OID the object it names and Type that object's
	// type, tag for an annotated tag.
	Ref  string     `json:"ref"`
	OID  string     `json:"oid"`
	Type ObjectType `json:"type"`
	// Peeled is what it names once every tag is peeled, and PeeledType its type.
	Peeled     string     `json:"peeled"`
	PeeledType ObjectType `json:"peeled_type"`
	// Date is git's creatordate to the second: an annotated tag's tagger date, a
	// lightweight tag's commit's committer date; absent where git's is empty.
	Date *time.Time `json:"date,omitempty"`
	// DateUnix carries a date past LastJSONSecond, which Date cannot: such a
	// tag is still listed where git lists it, newest.
	DateUnix int64 `json:"date_unix,omitempty"`
}

// TagMarks is the tags naming each commit an answer names, by the commit's
// whole id; a commit absent from Commits is named by no tag.
type TagMarks struct {
	Commits map[string]CommitTags `json:"commits,omitempty"`
}

// CommitTags is the tags naming one commit: at most MaxCommitTags whole refs,
// newest first, and how many more there are.
type CommitTags struct {
	Refs []string `json:"refs"`
	More int      `json:"more,omitempty"`
}
