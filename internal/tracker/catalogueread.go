package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading the workspace catalogues.
//
// One question rather than two, because every caller wants both: a screen
// drawing the catalogue draws types and fields together, and a model told
// which types exist and not which fields are required would file work that is
// refused on the next breath.

// CatalogueAnswer is what a company may file, and what it must carry.
type CatalogueAnswer struct {
	// Types is the EFFECTIVE set — the builtins this build ships, plus
	// what the company declared, with a declaration replacing a builtin of
	// the same slug. See [EffectiveTypes].
	Types []TaskType `json:"types"`

	// Fields are the WORKSPACE's declarations. A project declares its own
	// beside them, and a task's effective set is the union — which is why
	// this answer names its scope rather than claiming to be complete.
	Fields []FieldDef `json:"fields"`

	// OptionsTotal is how many options the returned fields declare between
	// them, and OptionsShown how many this answer carries.
	//
	// THE TWO TOGETHER, because a reader's question is "am I seeing all the
	// choices" and a single number cannot answer it. At the caps this
	// engine allows — 64 fields carrying 128 options each — the option
	// lists ALONE are the largest thing a catalogue answer holds, and an
	// answer that quietly carried some of them would have a model choosing
	// from a list it believed was complete.
	OptionsTotal int `json:"options_total"`
	OptionsShown int `json:"options_shown"`

	// PolicyVersion is what a task's policy stamp records having been
	// validated against.
	PolicyVersion int `json:"policy_version"`

	TypesVersion  uint64 `json:"types_version"`
	FieldsVersion uint64 `json:"fields_version"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// CatalogueQuery asks for them.
type CatalogueQuery struct {
	// Archived includes the archived types and fields; absent excludes
	// them, which is what a form offering choices means.
	Archived bool

	// Options is how many option rows this answer may carry. Zero takes
	// [MaxCatalogueOptions].
	Options int

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Catalogue answers what a task may be and what it may carry.
func (r *Reader) Catalogue(ctx context.Context, q CatalogueQuery) (CatalogueAnswer, error) {
	if q.Level == "" {
		return CatalogueAnswer{}, fmt.Errorf("tracker: this catalogue read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	var out CatalogueAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level:           q.Level,
		Scope:           catalogueScope(),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		types, _, err := readTypeCatalogue(ctx, tx)
		if err != nil {
			return err
		}
		fields, _, err := readFieldCatalogue(ctx, tx)
		if err != nil {
			return err
		}
		out.Types = EffectiveTypes(types.Types)
		out.Fields = fields.Fields
		out.PolicyVersion = fields.PolicyVersion
		out.TypesVersion, out.FieldsVersion = types.Version, fields.Version
		if !q.Archived {
			out.Types = liveTypes(out.Types)
			out.Fields = liveFields(out.Fields)
		}
		// THE OPTION COUNTS, before the fields are handed on: they are
		// what a reader checks to know the choices they were offered are
		// all of them, and at this engine's own caps — 64 fields of 128
		// options — the lists are the largest thing this answer holds.
		out.OptionsTotal = countOptions(out.Fields)
		out.Fields, out.OptionsShown = pageOptions(out.Fields, q.Options)
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return CatalogueAnswer{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// catalogueScope is the catalogue FAMILY, which is where both documents'
// subjects resolve — see [subjectPath].
func catalogueScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path(),
	}}.Normalised()
}

func liveTypes(in []TaskType) []TaskType {
	out := make([]TaskType, 0, len(in))
	for _, t := range in {
		if !t.Archived {
			out = append(out, t)
		}
	}
	return out
}

func liveFields(in []FieldDef) []FieldDef {
	out := make([]FieldDef, 0, len(in))
	for _, f := range in {
		if !f.Archived {
			out = append(out, f)
		}
	}
	return out
}

// MaxCatalogueOptions is how many option rows ONE catalogue answer carries.
//
// The engine's own caps allow 64 fields of 128 options — 8 192 rows, which
// encodes at nearly a megabyte and is fourteen times the ceiling on one tool
// answer. A catalogue that large is a company with a genuinely deep
// vocabulary, and the answer for it is a page rather than a refusal: a model
// choosing a value needs the options of the ONE field it is setting, and
// `options_total` beside `options_shown` is what tells it whether it is
// looking at all of them.
//
// 256, which is TWO maximal option lists, or every option of a company with an
// ordinary catalogue. The figure is chosen against what sits BESIDE the
// options in the same answer rather than on its own: at their own caps the 64
// type declarations and 64 field declarations are already ≈ 37 KiB, so the
// options have ≈ 27 KiB of the ceiling to fit in, and 256 rows encode at
// ≈ 20 KiB. `TestEveryToolAnswerFitsToolAnswerBytes` is what re-measures that
// when any of the three caps moves.
const MaxCatalogueOptions = 256

// pageOptions cuts the option lists to the answer's budget, FIELD BY FIELD, and
// says how many rows survived.
//
// WHOLE FIELDS RATHER THAN A FLAT CUT: a model setting `severity` needs every
// option of `severity`, and half a list is worse than none — it would choose
// from what it was shown and believe that was the set. So a field whose list
// does not fit is returned with NO options rather than some, and the counts say
// what is missing.
func pageOptions(fields []FieldDef, budget int) ([]FieldDef, int) {
	if budget <= 0 {
		budget = MaxCatalogueOptions
	}
	out := make([]FieldDef, 0, len(fields))
	shown := 0
	for _, f := range fields {
		if n := len(f.Config.Options); n > 0 && shown+n > budget {
			f.Config.Options = nil
			out = append(out, f)
			continue
		}
		shown += len(f.Config.Options)
		out = append(out, f)
	}
	return out, shown
}

// countOptions is how many choices a set of fields declares between them.
func countOptions(fields []FieldDef) int {
	total := 0
	for _, f := range fields {
		total += len(f.Config.Options)
	}
	return total
}
