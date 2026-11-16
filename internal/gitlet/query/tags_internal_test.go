package query

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// pinsAsked is a node's cache keeping no tag index, recording the pin of
// each read that asks for one.
type pinsAsked struct {
	fakeCache
	asked *[]string
}

func (p pinsAsked) Tags(s cache.Synced) (*cache.TagIndex, bool) {
	*p.asked = append(*p.asked, s.Dir)
	return nil, false
}

// A READ PINNED ANEW OPENS THE TAG INDEX AGAIN, once a pin.
//
// Mutation: open once a read → the second pin's index is never asked, red.
func TestAReadPinnedAnewOpensTheTagIndexAgain(t *testing.T) {
	t.Parallel()
	var asked []string
	r := &read{s: &Server{o: Options{Cache: pinsAsked{asked: &asked}}, pageBytes: gitlet.MaxGitAnswerBytes},
		q: gitlet.GitQuery{Kind: gitlet.GitLog, Log: &gitlet.LogQuery{}}}
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
	}
	r.synced = cache.Synced{Dir: "/copy.1", Position: at(3)}
	r.index(nil)
	r.index(nil)
	r.synced = cache.Synced{Dir: "/copy.1", Position: at(5)}
	r.index(nil)
	r.synced = cache.Synced{Dir: "/copy.2", Position: at(5)}
	r.index(nil)
	if want := []string{"/copy.1", "/copy.1", "/copy.2"}; !slices.Equal(asked, want) {
		t.Errorf("the reads asked for the tag index of %v, want once a pin: %v", asked, want)
	}
	r.closeTags()
}
