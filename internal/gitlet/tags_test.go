package gitlet_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A TAGS QUERY VALIDATES its limit, sub-query, stamp and cursor length.
//
// Mutation: add GitTags to Unstampable → red.
func TestATagsQueryValidates(t *testing.T) {
	t.Parallel()
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	q := func(limit int) gitlet.GitQuery {
		return gitlet.GitQuery{Kind: gitlet.GitTags, RepoID: repo, Asker: asker, Tags: &gitlet.TagsQuery{Limit: limit}}
	}
	for _, limit := range []int{-1, gitlet.MaxTagsLimit + 1} {
		if err := q(limit).Validate(); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "tags.limit") {
			t.Errorf("a limit of %d: Validate = %v, want ErrInvalid naming tags.limit", limit, err)
		}
	}
	for _, limit := range []int{0, 1, gitlet.MaxTagsLimit} {
		if err := q(limit).Validate(); err != nil {
			t.Errorf("a limit of %d: Validate = %v, want a read", limit, err)
		}
	}
	if got := (&gitlet.TagsQuery{}).PageLimit(); got != gitlet.DefaultTagsLimit {
		t.Errorf("a page of no limit lists %d, want %d", got, gitlet.DefaultTagsLimit)
	}
	none := q(0)
	none.Tags = nil
	tips := q(0)
	tips.Tips = &gitlet.TipsQuery{OIDs: []string{strings.Repeat("a", 40)}}
	unstamped := q(0)
	unstamped.Unstamped = true
	for name, bad := range map[string]gitlet.GitQuery{"no tags query": none, "a tips query beside it": tips,
		"unstamped": unstamped} {
		if err := bad.Validate(); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	widest := q(0)
	widest.Cursor = strings.Repeat("d", 349)
	if err := widest.Validate(); err != nil {
		t.Errorf("a cursor of 349 bytes: Validate = %v, want it handed to the Gitlet node", err)
	}
	widest.Cursor = strings.Repeat("d", 385)
	if err := widest.Validate(); !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a cursor of 385 bytes: Validate = %v, want ErrBadCursor", err)
	}
}

// AN OBJECT TYPE IS ONE GIT NAMES.
//
// Mutation: accept the empty type → red.
func TestAnObjectTypeIsOneGitNames(t *testing.T) {
	t.Parallel()
	for _, typ := range []gitlet.ObjectType{gitlet.ObjectCommit, gitlet.ObjectTree, gitlet.ObjectBlob, gitlet.ObjectTag} {
		if !typ.Valid() {
			t.Errorf("%q is not valid", typ)
		}
	}
	for _, typ := range []gitlet.ObjectType{"", "TAG", "submodule"} {
		if typ.Valid() {
			t.Errorf("%q is valid", typ)
		}
	}
}

// LAST JSON SECOND IS THE LAST ENCODING/JSON WRITES.
//
// Mutation: 253402300800 → red.
func TestLastJSONSecondIsTheLastEncodingJSONWrites(t *testing.T) {
	t.Parallel()
	if _, err := json.Marshal(time.Unix(gitlet.LastJSONSecond, 0).UTC()); err != nil {
		t.Errorf("encoding LastJSONSecond: %v", err)
	}
	if _, err := json.Marshal(time.Unix(gitlet.LastJSONSecond+1, 0).UTC()); err == nil {
		t.Error("encoding the second after LastJSONSecond succeeded: it is not the last")
	}
}
