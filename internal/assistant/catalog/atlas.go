package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/authz"
)

// THE ATLAS'S PROPOSALS: every change a person makes to the company's atlas,
// prepared as a proposal they approve (ADR-0051) and sent as the operator tool
// of the same verb, so a change Ask Crewlet prepared and one the person made on
// the screen are one code path. Each preview reads the entries it changes as
// they are now through the turn's grant (`GET /atlas/{id}`), refuses before
// any card a change the tool would refuse (an entry the person cannot read, a
// fact the settings declare), and binds the proposal to the entry as the card
// shows it ([proposal.RevisionAtlasEntry]).
//
// A VALUE'S PURGE IS NEVER PROPOSED. A proposal keeps the exact request its
// approval sends, so a purge of a value would keep, in the proposal's record,
// the value it erases. An entry's purge names the entry by its id; a value is
// purged from the screen.

// atlasEntryRoute is the read every atlas preview reads an entry through.
const atlasEntryRoute = "GET /atlas/{id}"

// The atlas proposals' names.
const (
	addAtlasEntryTool        = "add_atlas_entry"
	correctAtlasFactTool     = "correct_atlas_fact"
	confirmAtlasEntryTool    = "confirm_atlas_entry"
	relateAtlasEntriesTool   = "relate_atlas_entries"
	unrelateAtlasEntriesTool = "unrelate_atlas_entries"
	setAtlasOwnerTool        = "set_atlas_owner"
	retireAtlasEntryTool     = "retire_atlas_entry"
	restoreAtlasEntryTool    = "restore_atlas_entry"
	mergeAtlasEntriesTool    = "merge_atlas_entries"
	splitAtlasEntriesTool    = "split_atlas_entries"
	purgeAtlasEntryTool      = "purge_atlas_entry"
)

// atlasWrites are the atlas proposals, in the order a model meets them.
func atlasWrites() []Tool {
	entry := text("The entry's id, as the atlas's read answers it (GET /atlas).", nameBytes)
	other := text("The other entry's id.", nameBytes)
	relation := oneOf("The relation, from the entry that relies to the one relied on.", atlasRelations()...)
	fact := oneOf("The fact, one the entry's kind carries.", atlasFactNames()...)
	return []Tool{
		write(addAtlasEntryTool, authz.ActionAtlasAddEntry, false, "add atlas entries",
			"Propose adding an entry no source reports to the company's atlas: a service, a database, "+
				"a queue, a bucket, a server or anything else the company runs on, by its kind and key, "+
				"with a name where the key is not one. A vendor the atlas does not know is a "+
				"vendor_account keyed other/<id>.",
			object([]string{"kind", "key"}, map[string]*schema{
				"kind": oneOf("The entry's kind.", atlasKindNames()...),
				"key":  text("Its key in its kind's grammar: a service's slug (checkout-api), a host name.", nameBytes),
				"name": text("A name to show, plain text.", lineBytes),
			})),
		write(correctAtlasFactTool, authz.ActionAtlasCorrect, true, "correct the atlas",
			"Propose setting one fact of an atlas entry to the value the person knows, which then "+
				"outranks what integrations and agents report; with wrong, propose that the value "+
				"shown is wrong. Read the entry first. Facts the settings declare are changed there.",
			object([]string{"atlas_entry", "fact"}, map[string]*schema{
				"atlas_entry": entry, "fact": fact,
				"value": text("The value, in the fact's grammar; with wrong, the value that is wrong.", lineBytes),
				"wrong": flag("Say the value is wrong rather than set it."),
			})),
		write(confirmAtlasEntryTool, authz.ActionAtlasConfirm, false, "confirm atlas entries",
			"Propose confirming what the atlas shows as the person's own: the entry, one of its facts "+
				"(fact, and value to pick one a source reports), or one relation (relation and to).",
			object([]string{"atlas_entry"}, map[string]*schema{
				"atlas_entry": entry, "fact": fact,
				"value":    text("The value confirmed, as the atlas shows it.", lineBytes),
				"relation": relation, "to": text("With relation, the entry it points at.", nameBytes),
			})),
		write(relateAtlasEntriesTool, authz.ActionAtlasRelate, false, "relate atlas entries",
			"Propose drawing one relation between two atlas entries: a service runs_on a server, "+
				"depends_on a database.",
			object([]string{"atlas_entry", "relation", "to"}, map[string]*schema{
				"atlas_entry": entry, "relation": relation, "to": text("The entry relied on.", nameBytes),
			})),
		write(unrelateAtlasEntriesTool, authz.ActionAtlasUnrelate, true, "remove atlas relations",
			"Propose recording that a relation the atlas shows between two entries does not hold.",
			object([]string{"atlas_entry", "relation", "to"}, map[string]*schema{
				"atlas_entry": entry, "relation": relation, "to": text("The entry it points at.", nameBytes),
			})),
		write(setAtlasOwnerTool, authz.ActionAtlasSetOwner, true, "set atlas owners",
			"Propose who owns an atlas entry: one seat or one unit of the chart, in place of any owner "+
				"a person named before; clear removes it.",
			object([]string{"atlas_entry"}, map[string]*schema{
				"atlas_entry": entry,
				"seat":        text("The owning seat's handle or name.", nameBytes),
				"unit":        text("The owning unit's key or name.", nameBytes),
				"clear":       flag("Remove the owner a person named."),
			})),
		write(retireAtlasEntryTool, authz.ActionAtlasRetire, true, "retire atlas entries",
			"Propose retiring an atlas entry the company no longer runs: it leaves the map and stays in "+
				"its history.",
			object([]string{"atlas_entry"}, map[string]*schema{"atlas_entry": entry})),
		write(restoreAtlasEntryTool, authz.ActionAtlasRestore, false, "restore atlas entries",
			"Propose bringing back an atlas entry somebody retired.",
			object([]string{"atlas_entry"}, map[string]*schema{"atlas_entry": entry})),
		write(mergeAtlasEntriesTool, authz.ActionAtlasMerge, false, "merge atlas entries",
			"Propose that two atlas entries of one kind are the same thing, shown as one.",
			object([]string{"atlas_entry", "other"}, map[string]*schema{"atlas_entry": entry, "other": other})),
		write(splitAtlasEntriesTool, authz.ActionAtlasSplit, false, "split atlas entries",
			"Propose that two atlas entries are not the same thing, whatever key they share.",
			object([]string{"atlas_entry", "other"}, map[string]*schema{"atlas_entry": entry, "other": other})),
		write(purgeAtlasEntryTool, authz.ActionAtlasPurge, true, "purge atlas entries",
			"Propose erasing an atlas entry everywhere, its history included, and keeping it from being "+
				"recorded again. Nothing undoes it; an administrator approves it after a fresh sign-in.",
			object([]string{"atlas_entry"}, map[string]*schema{"atlas_entry": entry})),
	}
}

func atlasKindNames() []string { return wordsOf(atlas.Kinds) }
func atlasFactNames() []string { return wordsOf(atlas.Facts) }

// atlasRelations are the relations between two entries a proposal draws: an
// owner is set by its own proposal.
func atlasRelations() []string {
	return wordsOf(slices.DeleteFunc(slices.Clone(atlas.Relations), func(r atlas.Relation) bool {
		return r == atlas.RelationOwnedBy || r == atlas.RelationDocumentedIn
	}))
}

func wordsOf[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, s := range set {
		out[i] = string(s)
	}
	return out
}

// shownEntry is one entry as the atlas's read answers it.
type shownEntry struct {
	id, label, state string
	doc              any
}

// readAtlasEntry reads the entry id names through the grant: a read the route
// refuses (one the person cannot read, or none at all) refuses the step.
func readAtlasEntry(ctx context.Context, read proposal.Read, id string) (shownEntry, error) {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return shownEntry{}, fmt.Errorf("%w: an atlas entry is named by its id, as the atlas's read "+
			"answers it", ErrInvalidArguments)
	}
	doc, err := readGet(ctx, read, atlasEntryRoute, map[string]string{"id": id}, nil)
	if err != nil {
		return shownEntry{}, err
	}
	e := cards.Lookup(doc, "entry")
	label := value(e, "name") + " (" + strings.ReplaceAll(value(e, "ref.kind"), "_", " ") + ")"
	return shownEntry{id: value(e, "id"), label: label, state: value(e, "state"), doc: e}, nil
}

// facts is the entry's facts named f, each a decoded fact view.
func (s shownEntry) facts(f string) []any {
	all, _ := cards.Lookup(s.doc, "facts").([]any)
	return slices.DeleteFunc(slices.Clone(all), func(v any) bool { return value(v, "fact") != f })
}

// relations is the entry's relations named r.
func (s shownEntry) relations(r string) []any {
	all, _ := cards.Lookup(s.doc, "relations").([]any)
	return slices.DeleteFunc(slices.Clone(all), func(v any) bool { return value(v, "relation") != r })
}

// declaredBy is where a fact or relation the settings or the engine's records
// hold is changed, or "" for one a person may change in the atlas.
func declaredBy(view any) string {
	sources := []any{cards.Lookup(view, "source")}
	if others, ok := cards.Lookup(view, "others").([]any); ok {
		for _, o := range others {
			sources = append(sources, cards.Lookup(o, "source"))
		}
	}
	for _, src := range sources {
		switch value(src, "kind") {
		case string(atlas.SourceSettings):
			return "the company's settings"
		case string(atlas.SourceRecords):
			if value(src, "name") == string(atlas.RecordGitlet) {
				return "Gitlet"
			}
			return "the engine's own records"
		}
	}
	return ""
}

// shownValue is a fact view's value with the word its source is read as.
func shownValue(view any) string {
	return value(view, "value.text") + " (" + value(view, "word") + ", from " + value(view, "source.kind") + ")"
}

// atlasRevision is the entry as the card shows it: its state, and each fact and
// relation with the source holding it and its standing. Never an instant, which
// a new sighting of the same value moves.
func atlasRevision(s shownEntry) string {
	type shown struct {
		Fact, Relation, To, External, Name, Value, Source, Of, Standing string
	}
	var parts []shown
	for _, list := range []string{"facts", "relations"} {
		views, _ := cards.Lookup(s.doc, list).([]any)
		for _, v := range views {
			parts = append(parts, shown{Fact: value(v, "fact"), Relation: value(v, "relation"),
				To: value(v, "to"), External: value(v, "external"), Name: value(v, "name"),
				Value: value(v, "value.text"), Source: value(v, "source.kind"), Of: value(v, "source.name"),
				Standing: value(v, "standing")})
		}
	}
	body, _ := json.Marshal(struct {
		State string
		Parts []shown
	}{s.state, parts})
	sum := sha256.Sum256(body)
	return proposal.Revision{Kind: proposal.RevisionAtlasEntry, ID: s.id,
		Version: hex.EncodeToString(sum[:16])}.String()
}

// atlasEntryRevision re-reads an entry's revision for an approval.
func atlasEntryRevision(ctx context.Context, read proposal.Read, r proposal.Revision) (string, error) {
	s, err := readAtlasEntry(ctx, read, r.ID)
	if err != nil {
		return "", err
	}
	return atlasRevision(s), nil
}

// refusedDeclared is a change on what the settings or the engine's records
// declare, which the tool would refuse: no card.
func refusedDeclared(what, where string) error {
	return fmt.Errorf("%w: %s declare %s, and it is changed there, never in the atlas",
		ErrInvalidArguments, where, what)
}

func addAtlasPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		ref, err := atlas.RecordedRef(atlas.Kind(a.String("kind")), a.String("key"))
		if err != nil {
			return proposal.Preview{}, fmt.Errorf("%w: key: %w", ErrInvalidArguments, err)
		}
		if src, named := ref.EngineNamed(); named {
			where := "the company's settings"
			if src.Kind == atlas.SourceRecords {
				where = "Gitlet"
			}
			return proposal.Preview{}, fmt.Errorf("%w: a %s keyed %s is one %s declare, and enters the "+
				"atlas from there", ErrInvalidArguments, ref.Kind, ref.Key, where)
		}
		probe, err := get(atlasEntryRoute, map[string]string{"id": ref.ID().String()}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		resp, err := read(ctx, probe)
		switch {
		case err != nil:
			return proposal.Preview{}, fmt.Errorf("%w: %w", ErrPreviewRead, err)
		case resp.Status == http.StatusOK:
			return proposal.Preview{}, fmt.Errorf("%w: %s is in the atlas already, as entry %s; a retired "+
				"one is brought back with %s", ErrInvalidArguments, ref, ref.ID(), restoreAtlasEntryTool)
		case resp.Status != http.StatusNotFound:
			return proposal.Preview{}, refusedRead(resp)
		}
		var c changes
		c.fact("kind", "Kind", strings.ReplaceAll(string(ref.Kind), "_", " "))
		c.add("key", "Key", ref.Key)
		c.add("name", "Name", a.String("name"))
		return proposal.Preview{Title: "Add atlas entry", Target: orDefault(a.String("name"), ref.Key),
			Note:         "The entry is shown as confirmed by you, and a source that reports it later joins it.",
			ApproveLabel: "Approve and add", Changes: c.rows(), Request: req}, nil
	}
}

func correctAtlasPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		s, err := readAtlasEntry(ctx, read, a.String("atlas_entry"))
		if err != nil {
			return proposal.Preview{}, err
		}
		f := atlas.Fact(a.String("fact"))
		if !f.AppliesTo(atlas.Kind(value(s.doc, "ref.kind"))) {
			return proposal.Preview{}, fmt.Errorf("%w: a %s carries no %s", ErrInvalidArguments,
				value(s.doc, "ref.kind"), f)
		}
		var now any
		for _, v := range s.facts(string(f)) {
			if !f.Spec().Many || value(v, "value.text") == a.String("value") {
				now = v
			}
		}
		if now != nil {
			if where := declaredBy(now); where != "" {
				return proposal.Preview{}, refusedDeclared(s.label+"'s "+string(f), where)
			}
		}
		var c changes
		c.fact("entry", "Entry", s.label)
		label := strings.ReplaceAll(string(f), "_", " ")
		title, approve, note := "Correct atlas fact", "Approve and correct",
			"Your value is shown over what any integration or agent reports, until one reports a "+
				"change, when both are shown for review."
		switch {
		case a.Bool("wrong"):
			if now == nil {
				return proposal.Preview{}, fmt.Errorf("%w: the atlas shows no %s on %s to call wrong",
					ErrInvalidArguments, label, s.label)
			}
			c = append(c, proposal.Removed(string(f), label, shownValue(now)))
			title, approve, note = "Mark atlas fact wrong", "Approve and mark wrong",
				"The value is recorded as wrong in your name and leaves the entry."
		case now != nil:
			c.move(string(f), label, value(now, "value.text"), a.String("value"))
		default:
			c.add(string(f), label, a.String("value"))
		}
		return proposal.Preview{Title: title, Target: s.label, Note: note, ApproveLabel: approve,
			Changes: c.rows(), Request: req, SourceRevision: atlasRevision(s)}, nil
	}
}

func confirmAtlasPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		s, err := readAtlasEntry(ctx, read, a.String("atlas_entry"))
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("entry", "Entry", s.label)
		switch {
		case a.String("fact") != "":
			var now any
			for _, v := range s.facts(a.String("fact")) {
				if a.String("value") == "" || value(v, "value.text") == a.String("value") {
					now = v
				}
			}
			if now == nil {
				return proposal.Preview{}, fmt.Errorf("%w: the atlas shows no %s on %s with that value to "+
					"confirm", ErrInvalidArguments, a.String("fact"), s.label)
			}
			c = append(c, proposal.Changed(a.String("fact"), strings.ReplaceAll(a.String("fact"), "_", " "),
				shownValue(now), value(now, "value.text")+" (confirmed by you)"))
		case a.String("relation") != "":
			to, err := readAtlasEntry(ctx, read, a.String("to"))
			if err != nil {
				return proposal.Preview{}, err
			}
			i := slices.IndexFunc(s.relations(a.String("relation")), func(v any) bool { return value(v, "to") == to.id })
			if i < 0 {
				return proposal.Preview{}, fmt.Errorf("%w: the atlas shows no %s from %s to %s to confirm",
					ErrInvalidArguments, a.String("relation"), s.label, to.label)
			}
			c.fact("relation", strings.ReplaceAll(a.String("relation"), "_", " "), to.label+" (confirmed by you)")
		default:
			c = append(c, proposal.Changed("state", "State", s.state, "confirmed by you"))
		}
		return proposal.Preview{Title: "Confirm atlas entry", Target: s.label,
			Note:         "What a source reported is shown as confirmed by you.",
			ApproveLabel: "Approve and confirm", Changes: c.rows(), Request: req,
			SourceRevision: atlasRevision(s)}, nil
	}
}

func relateAtlasPreview(remove bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			a := Args(args)
			s, err := readAtlasEntry(ctx, read, a.String("atlas_entry"))
			if err != nil {
				return proposal.Preview{}, err
			}
			to, err := readAtlasEntry(ctx, read, a.String("to"))
			if err != nil {
				return proposal.Preview{}, err
			}
			r := atlas.Relation(a.String("relation"))
			if !r.Joins(atlas.Kind(value(s.doc, "ref.kind")), atlas.Kind(value(to.doc, "ref.kind"))) {
				return proposal.Preview{}, fmt.Errorf("%w: %s does not run from %s to %s",
					ErrInvalidArguments, r, s.label, to.label)
			}
			var c changes
			c.fact("entry", "Entry", s.label)
			label := strings.ReplaceAll(string(r), "_", " ")
			shown := slices.IndexFunc(s.relations(string(r)), func(v any) bool { return value(v, "to") == to.id })
			if !remove {
				c.add("relation", label, to.label)
				return proposal.Preview{Title: "Relate atlas entries", Target: s.label,
					Note: "The map draws the relation, as confirmed by you.", ApproveLabel: "Approve and relate",
					Changes: c.rows(), Request: req, SourceRevision: atlasRevision(s)}, nil
			}
			if shown < 0 {
				return proposal.Preview{}, fmt.Errorf("%w: the atlas shows no %s from %s to %s",
					ErrInvalidArguments, label, s.label, to.label)
			}
			if where := declaredBy(s.relations(string(r))[shown]); where != "" {
				return proposal.Preview{}, refusedDeclared("that "+s.label+" "+label+" "+to.label, where)
			}
			c = append(c, proposal.Removed("relation", label, to.label))
			return proposal.Preview{Title: "Remove atlas relation", Target: s.label,
				Note:         "It is recorded in your name that the relation does not hold.",
				ApproveLabel: "Approve and remove", Changes: c.rows(), Request: req,
				SourceRevision: atlasRevision(s)}, nil
		}
	}
}

func ownerAtlasPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		s, err := readAtlasEntry(ctx, read, a.String("atlas_entry"))
		if err != nil {
			return proposal.Preview{}, err
		}
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		var owners []string
		for _, v := range s.relations(string(atlas.RelationOwnedBy)) {
			if where := declaredBy(v); where != "" {
				return proposal.Preview{}, refusedDeclared("who owns "+s.label, where)
			}
			owners = append(owners, ownerName(chart, value(v, "external"), value(v, "name")))
		}
		to := ""
		switch {
		case a.String("seat") != "":
			to = ownerName(chart, string(atlas.ExternalSeat), a.String("seat"))
		case a.String("unit") != "":
			to = "unit " + a.String("unit")
		}
		var c changes
		c.fact("entry", "Entry", s.label)
		c.move("owner", "Owner", strings.Join(owners, ", "), to)
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: %s is owned that way already", ErrInvalidArguments, s.label)
		}
		return proposal.Preview{Title: "Set atlas owner", Target: s.label,
			Note: "The owner is shown as named by you.", ApproveLabel: "Approve and set",
			Changes: c.rows(), Request: req, SourceRevision: atlasRevision(s)}, nil
	}
}

// ownerName is an owner as a card names it: a seat by its name, a unit by its
// key.
func ownerName(c company, external, name string) string {
	if external == string(atlas.ExternalSeat) {
		return c.seatName(name)
	}
	return "unit " + name
}

func retireAtlasPreview(retire bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			s, err := readAtlasEntry(ctx, read, Args(args).String("atlas_entry"))
			if err != nil {
				return proposal.Preview{}, err
			}
			retired := s.state == string(atlas.StateRetired)
			if retired == retire {
				return proposal.Preview{}, fmt.Errorf("%w: %s is %s already", ErrInvalidArguments, s.label, s.state)
			}
			if sources, _ := cards.Lookup(s.doc, "sources").([]any); retire && slices.ContainsFunc(sources,
				func(src any) bool { return atlas.SourceKind(value(src, "kind")).Computed() }) {
				return proposal.Preview{}, refusedDeclared(s.label, "the company's settings or the engine's records")
			}
			var c changes
			c.fact("entry", "Entry", s.label)
			title, label, to, note := "Restore atlas entry", "Approve and restore", "restored",
				"The entry is on the map again."
			if retire {
				title, label, to = "Retire atlas entry", "Approve and retire", string(atlas.StateRetired)
				note = "It leaves the map and stays in its history; restoring it brings it back."
			}
			c = append(c, proposal.Changed("state", "State", s.state, to))
			return proposal.Preview{Title: title, Target: s.label, Note: note, ApproveLabel: label,
				Changes: c.rows(), Request: req, SourceRevision: atlasRevision(s)}, nil
		}
	}
}

func pairAtlasPreview(merge bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			a := Args(args)
			s, err := readAtlasEntry(ctx, read, a.String("atlas_entry"))
			if err != nil {
				return proposal.Preview{}, err
			}
			o, err := readAtlasEntry(ctx, read, a.String("other"))
			if err != nil {
				return proposal.Preview{}, err
			}
			if value(s.doc, "ref.kind") != value(o.doc, "ref.kind") {
				return proposal.Preview{}, fmt.Errorf("%w: %s and %s are of two kinds and never one entry",
					ErrInvalidArguments, s.label, o.label)
			}
			one := s.id == o.id
			var c changes
			c.fact("entry", "Entry", s.label)
			c.fact("other", "Other", o.label)
			if merge {
				if one {
					return proposal.Preview{}, fmt.Errorf("%w: they are one entry already", ErrInvalidArguments)
				}
				c = append(c, proposal.Changed("shown", "Shown as", "two entries", "one entry"))
				return proposal.Preview{Title: "Merge atlas entries", Target: s.label,
					Note:         "A \"not the same\" somebody recorded between them is withdrawn first.",
					ApproveLabel: "Approve and merge", Changes: c.rows(), Request: req,
					SourceRevision: atlasRevision(s)}, nil
			}
			from := "two entries"
			if one {
				from = "one entry"
			}
			c = append(c, proposal.Changed("shown", "Shown as", from, "two entries, never merged"))
			return proposal.Preview{Title: "Split atlas entries", Target: s.label,
				Note:         "A person's \"not the same\" always wins over a key they share.",
				ApproveLabel: "Approve and split", Changes: c.rows(), Request: req,
				SourceRevision: atlasRevision(s)}, nil
		}
	}
}

func purgeAtlasPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		s, err := readAtlasEntry(ctx, read, Args(args).String("atlas_entry"))
		if err != nil {
			return proposal.Preview{}, err
		}
		if sources, _ := cards.Lookup(s.doc, "sources").([]any); slices.ContainsFunc(sources,
			func(src any) bool { return atlas.SourceKind(value(src, "kind")).Computed() }) {
			return proposal.Preview{}, refusedDeclared(s.label, "the company's settings or the engine's records")
		}
		recency, _ := authz.RecencyOf(authz.ActionAtlasPurge)
		var c changes
		c.fact("entry", "Entry", s.label)
		c = append(c, proposal.Removed("entry", "Entry and its history", s.label))
		return proposal.Preview{Title: "Purge atlas entry", Target: s.label,
			Summary: "Erases " + s.label + " and everything recorded about it, history included, and keeps " +
				"it from being recorded again.",
			Note: "Nothing undoes it.", Risk: wire.RiskDestructive, ApproveLabel: "Approve and purge",
			Changes: c.rows(), Request: req, StepUp: recency.Demands(),
			SourceRevision: atlasRevision(s)}, nil
	}
}
