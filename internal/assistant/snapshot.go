package assistant

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// snapshotPart is one message, step or card of a snapshot, by its place in
// its list.
type snapshotPart struct{ list, index int }

// The lists of a snapshot, as snapshotPart.list names them.
const (
	partMessage = iota
	partStep
	partCard
)

// fitSnapshot is f, a snapshot frame, held to [wire.FrameMaxBytes]: whole
// where it fits, else with the parts [wire.SnapshotEvent.Cut] names left out
// in that order, until it fits, and marked cut.
//
// ONE FRAME LIKE ANY OTHER, though no frame log admits it: a turn's records
// (forty rounds of answers and steps, any number of 256 KiB cards) can pass
// the bound, and a frame past it is refused by the stream's encoder and,
// sealed into a peer's replay answer, by the broker. What a reader loses
// least goes first: a step is a line of progress, a card a read they can ask
// again, an answer and a proposal what they came for. The question is never
// cut, and alone it is far within the bound (assistantdomain.MessageMaxBytes).
func fitSnapshot(f wire.Frame) (wire.Frame, error) {
	whole, err := json.Marshal(f)
	if err != nil {
		return wire.Frame{}, fmt.Errorf("assistant: encode turn %s's snapshot: %w", f.TurnID, err)
	}
	if len(whole) <= wire.FrameMaxBytes {
		return f, nil
	}
	snap := f.Snapshot
	head := f
	head.Snapshot = &wire.SnapshotEvent{Messages: []wire.Message{}, Steps: []wire.StepEvent{},
		Cards: []wire.CardEvent{}, Cut: true}
	empty, err := json.Marshal(head)
	if err != nil {
		return wire.Frame{}, fmt.Errorf("assistant: encode turn %s's snapshot: %w", f.TurnID, err)
	}
	// MEASURED BY ENCODING each part once: a list encodes as its parts
	// joined by commas, so the frame's size is the empty frame's, every part
	// kept, and a comma between each two of one list.
	sizes := [3][]int{}
	kept, sum := [3]int{}, 0
	for list, parts := range [3][]any{each(snap.Messages), each(snap.Steps), each(snap.Cards)} {
		for _, v := range parts {
			var b []byte
			if b, err = json.Marshal(v); err != nil {
				return wire.Frame{}, fmt.Errorf("assistant: encode turn %s's snapshot: %w",
					f.TurnID, err)
			}
			sizes[list] = append(sizes[list], len(b))
			kept[list]++
			sum += len(b)
		}
	}
	size := func() int {
		n := len(empty) + sum
		for _, k := range kept {
			n += max(k-1, 0)
		}
		return n
	}
	dropped := [3]map[int]bool{{}, {}, {}}
	for _, p := range cutOrder(snap) {
		if size() <= wire.FrameMaxBytes {
			break
		}
		dropped[p.list][p.index] = true
		kept[p.list]--
		sum -= sizes[p.list][p.index]
	}
	cut := *head.Snapshot
	cut.Messages = keep(snap.Messages, dropped[partMessage])
	cut.Steps = keep(snap.Steps, dropped[partStep])
	cut.Cards = keep(snap.Cards, dropped[partCard])
	head.Snapshot = &cut
	encoded, err := json.Marshal(head)
	if err != nil {
		return wire.Frame{}, fmt.Errorf("assistant: encode turn %s's snapshot: %w", f.TurnID, err)
	}
	if len(encoded) > wire.FrameMaxBytes {
		return wire.Frame{}, fmt.Errorf("assistant: turn %s's snapshot encodes to %d bytes "+
			"with everything but its question cut, over %d", f.TurnID, len(encoded),
			wire.FrameMaxBytes)
	}
	return head, nil
}

// cutOrder is every part of snap a cut may leave out, the first to go first
// ([wire.SnapshotEvent.Cut]): steps, cards that propose no change, answers,
// proposals, each oldest first. The person's question is not among them.
func cutOrder(snap *wire.SnapshotEvent) []snapshotPart {
	var order, proposals []snapshotPart
	for i := range snap.Steps {
		order = append(order, snapshotPart{partStep, i})
	}
	for i, c := range snap.Cards {
		if c.Card.Type == wire.CardProposal {
			proposals = append(proposals, snapshotPart{partCard, i})
			continue
		}
		order = append(order, snapshotPart{partCard, i})
	}
	for i, m := range snap.Messages {
		if m.Role != wire.RoleUser {
			order = append(order, snapshotPart{partMessage, i})
		}
	}
	return append(order, proposals...)
}

// each is list as values to encode one at a time.
func each[T any](list []T) []any {
	out := make([]any, len(list))
	for i := range list {
		out[i] = list[i]
	}
	return out
}

// keep is list without the places dropped names, never nil.
func keep[T any](list []T, dropped map[int]bool) []T {
	out := make([]T, 0, len(list)-len(dropped))
	for i, v := range list {
		if !dropped[i] {
			out = append(out, v)
		}
	}
	return out
}
