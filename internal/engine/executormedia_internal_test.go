package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executordomain"
)

// A TAKE KEEPS ITS MEDIA APART ONLY ONCE EVERY NODE APPLYING THE EXECUTOR LOG
// READS A CAPTURE THAT NAMES MEDIA: a rolling upgrade's older node would
// retain such a capture, its estate naming none of the media, and its
// collector take them. So while one counted node reads below
// executordomain.MediaVersion, or has not said what it reads, or nobody is
// counted, the policy keeps them inline; a node not applying the log holds
// nothing back.
//
// Mutation: read the version off any one node, count a node that runs no
// executor log, or answer true for an empty set, and a case goes red.
func TestATakeKeepsMediaApartOnlyOnceEveryReaderReadsIt(t *testing.T) {
	t.Parallel()
	log := executordomain.Domain{}.Name()
	at := func(node string, version int) coord.NodePositions {
		return coord.NodePositions{NodeID: node, Domains: map[string]coord.DomainPosition{
			log: {RecordVersion: version}}}
	}
	for name, tc := range map[string]struct {
		rows  []coord.NodePositions
		apart bool
	}{
		"every reader at the media version or past it": {rows: []coord.NodePositions{
			at("data-a", executordomain.MediaVersion), at("data-b", executordomain.MediaVersion+3)}, apart: true},
		"one reader below it": {rows: []coord.NodePositions{
			at("data-a", executordomain.MediaVersion), at("data-b", executordomain.MediaVersion-1)}},
		"a reader that has not said": {rows: []coord.NodePositions{
			at("data-a", executordomain.MediaVersion), at("data-b", 0)}},
		"no reader": {},
		"a node that applies no executor log": {rows: []coord.NodePositions{
			at("data-a", executordomain.MediaVersion), {NodeID: "seats-a",
				Domains: map[string]coord.DomainPosition{"tracker": {RecordVersion: 1}}}}, apart: true},
	} {
		if got := unseenNode(t, nil, tc.rows...).executorMediaRead(t.Context()); got != tc.apart {
			t.Errorf("%s: media kept apart %v, want %v", name, got, tc.apart)
		}
	}
}
