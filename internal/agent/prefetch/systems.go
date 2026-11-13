package prefetch

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// THE COMPANY'S SYSTEMS THE WORK NAMES. A seat asked about `orders-db` or
// `api.acme.example` would otherwise probe a server or a vendor for what the
// company already knows about it. So a turn whose work item names entries of
// the atlas, or whose ask names one exactly, is told, in a few lines, what
// each is, how sure the atlas is of it and what it relies on, and to read the
// rest with the atlas tools. The entries the work item names come first: a
// person or an agent linked them on purpose, and a name in the ask is a match.
// There is no constant section: a turn that names nothing is told nothing,
// and the block is bounded however much it names.

// The block's bounds, each its reason beside it.
const (
	// MaxSystems is the most entries the block lists: eight, the most one
	// piece of work names before it is about the company at large, which the
	// atlas tools read better than a prompt does.
	MaxSystems = 8
	// MaxSystemsBytes is the most the listed lines take, about three hundred
	// tokens: well under the knowledge block beside it, so a turn whose work
	// names many systems does not crowd out what it was asked.
	MaxSystemsBytes = 1200
)

// Systems reads the company's atlas as a turn's seat may read it within whom
// the turn works for (ADR-0109), its private addresses only within the seat's
// own reach. It answers false where the turn may read none of it.
type Systems interface {
	Systems(ctx context.Context, r Request) (atlas.View, bool, error)
}

// SystemsLead opens the block: what the entries are, and what to do next.
var SystemsLead = "Your work item, or what you were asked, names these entries of the company's atlas. Each line is what " +
	"the atlas holds: the kind, how sure it is, and what the entry relies on. Read an entry in full with `" +
	toolname.AtlasGet + "`, and what depends on it with `" + toolname.AtlasTrace + "`, before probing a " +
	"server or a vendor for what the company already knows."

// systemsBlock is the block and how many entries it lists.
type systemsBlock struct {
	text  string
	named int
}

// systems renders the entries the ask names, or nothing where it names none,
// the turn may read none, or the atlas could not be read (logged).
func (f *Fetcher) systems(ctx context.Context, r Request) systemsBlock {
	asked := r.Ask.Text()
	if f.src.Systems == nil || r.Seat == nil || !r.ReadsAtlas ||
		(strings.TrimSpace(asked) == "" && len(r.Linked) == 0) {
		return systemsBlock{}
	}
	view, readable, err := f.src.Systems.Systems(ctx, r)
	if err != nil {
		log.WarnContext(ctx, "prefetch_systems_unread", "seat", r.Seat.Handle(), "error", err)
		return systemsBlock{}
	}
	if !readable {
		return systemsBlock{}
	}
	index := view.Index()
	named := systemsOf(view, index, r.Linked, asked)
	var lines []string
	var listed []atlas.Entry
	size := 0
	for _, e := range named {
		if len(lines) == MaxSystems {
			break
		}
		line := systemLine(view, index, e)
		if size+len(line)+1 > MaxSystemsBytes {
			break
		}
		lines, listed, size = append(lines, line), append(listed, e), size+len(line)+1
	}
	if len(lines) == 0 {
		return systemsBlock{}
	}
	writers, vendorListed := atlas.Writers(listed)
	label := provenance.LabelForHosted(r.Seat.Handle(), provenance.SourceAtlas, writers, true)
	if vendorListed {
		label.Trust = provenance.TrustFor(r.Seat.Handle(), nil)
	}
	body := strings.Join(lines, "\n")
	if more := len(named) - len(lines); more > 0 {
		body += fmt.Sprintf("\n(%d more the work names; find them with `%s`.)", more, toolname.AtlasSearch)
	}
	fenced := provenance.Present(r.Boundary, &label, body)
	return systemsBlock{text: SystemsLead + "\n\n" + fenced, named: len(lines)}
}

// systemsOf is the entries the block may list, each once: the live ones the
// work item names, in its order and through whichever entry each id merged
// into, then the ones the ask names. An id the seat's atlas does not hold
// (retired since, purged, or outside what the seat reads) names nothing.
func systemsOf(view atlas.View, index map[uuid.UUID]int, linked []uuid.UUID, asked string) []atlas.Entry {
	var out []atlas.Entry
	listed := map[uuid.UUID]bool{}
	add := func(e atlas.Entry) {
		if !listed[e.ID] && e.State != atlas.StateRetired {
			listed[e.ID] = true
			out = append(out, e)
		}
	}
	for _, id := range linked {
		if at, held := index[id]; held {
			add(view.Entries[at])
		}
	}
	for _, e := range view.Named(asked) {
		add(e)
	}
	return out
}

// systemLine is one entry in a line: its name, kind and key, how sure the
// atlas is of it, its state and health where they say something, and what it
// relies on through the relations a request travels.
func systemLine(v atlas.View, index map[uuid.UUID]int, e atlas.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (%s", e.Name, e.Ref.Kind)
	if e.Ref.Key != e.Name {
		fmt.Fprintf(&b, ", %s", e.Ref.Key)
	}
	fmt.Fprintf(&b, "): %s", e.Word)
	if e.State != atlas.StateConfirmed {
		fmt.Fprintf(&b, ", %s", e.State)
	}
	if h := e.Health(); h != atlas.HealthQuiet {
		fmt.Fprintf(&b, ", %s", h)
	}
	if e.NotUsedRecently {
		b.WriteString(", not used recently")
	}
	var relies []string
	for _, ed := range e.Relations {
		if !ed.Relation.Structural() || ed.External != "" {
			continue
		}
		if i, ok := index[ed.To]; ok {
			relies = append(relies, string(ed.Relation)+" "+v.Entries[i].Name)
		}
	}
	if len(relies) > 0 {
		fmt.Fprintf(&b, "; %s", strings.Join(relies, ", "))
	}
	return b.String()
}
