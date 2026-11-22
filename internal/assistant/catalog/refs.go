package catalog

import (
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A RECORD A CARD NAMES IS A [wire.Ref], never its name in the words: its kind,
// id, shown name and page from the link table, so the dashboard draws it as
// itself and no reader parses a name back out of a sentence.

// refScreen is the link table's screen for a record of kind, and the
// parameter its address takes; false for a kind no screen opens (a team).
func refScreen(kind wire.ResourceKind) (screen, param string, ok bool) {
	switch kind {
	case wire.ResourceContainer:
		return "container", "key", true
	case wire.ResourceTurn:
		return "turn", "id", true
	}
	return BriefScreenOf(kind)
}

// recordRef is the record of kind id, shown as name (its id where it has
// none), with its page where the link table builds one for id.
func recordRef(kind wire.ResourceKind, id, name string) wire.Ref {
	if name == "" {
		name = id
	}
	r := wire.Ref{Kind: kind, ID: id, Name: name}
	if screen, param, ok := refScreen(kind); ok {
		if link, err := cards.Link(screen, map[string]string{param: id}, name); err == nil {
			r.Open = &link
		}
	}
	return r
}

// naming is the phrase that is the record of kind id, shown as name; the
// empty phrase for no id.
func naming(kind wire.ResourceKind, id, name string) wire.Phrase {
	if id == "" {
		return wire.Plain(name)
	}
	return wire.Naming(recordRef(kind, id, name))
}

// taskRef is a task by its key, or its id where the key is unknown.
func taskRef(key string) wire.Phrase { return naming(wire.ResourceTask, key, key) }

// taskLine is a task as a fact row shows it: its key, then its title.
func taskLine(key, title string) wire.Phrase {
	if key == "" {
		return wire.Plain(title)
	}
	if title == "" {
		return taskRef(key)
	}
	return taskRef(key).Plain(" " + title)
}

// artifactRef is an artifact read through its route ([cards.Lookup] paths of
// `GET /artifacts/{id}`): its id, title and kind.
func artifactRef(doc any) wire.Phrase {
	id := value(doc, "id")
	if id == "" {
		return wire.Plain(value(doc, "title"))
	}
	r := recordRef(wire.ResourceArtifact, id, value(doc, "title"))
	r.ArtifactKind = value(doc, "kind")
	return wire.Naming(r)
}

// seat is the seat handle as a card names it: by its name on the chart, its
// handle for one the chart does not hold; the empty phrase for no handle.
func (c company) seat(handle string) wire.Phrase {
	if handle == "" {
		return nil
	}
	name := c.seats[handle].name
	if name == "" {
		name = handle
	}
	return naming(wire.ResourceSeat, handle, name)
}

// unit is the team id as a card names it, by its name on the chart.
func (c company) unit(id string) wire.Phrase {
	if id == "" {
		return nil
	}
	return naming(wire.ResourceUnit, id, orDefault(c.units[id], id))
}

// seatsOf are handles as a card names them, one phrase each.
func (c company) seatsOf(handles []string) []wire.Phrase {
	out := make([]wire.Phrase, 0, len(handles))
	for _, h := range handles {
		out = append(out, c.seat(h))
	}
	return out
}

// Named is the record of kind id, shown as name, as a card's phrase names it:
// what a receipt names outside a preview.
func Named(kind wire.ResourceKind, id, name string) wire.Phrase { return naming(kind, id, name) }
