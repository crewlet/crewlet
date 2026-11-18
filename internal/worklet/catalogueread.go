package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading the workspace catalogues: types and fields in one read, because a
// model told which types exist and not which fields are required files work
// that is refused on the next breath.

// CatalogueAnswer is what a company may file, and what it must carry.
type CatalogueAnswer struct {
	// Types is the EFFECTIVE set — the builtins this build ships, plus
	// what the company declared, with a declaration replacing a builtin of
	// the same slug. See [EffectiveTypes].
	Types []TaskType `json:"types"`

	// DeclaredTypes is the company's OWN declarations, archived ones
	// included whatever the query asked: what a write of `types` replaces.
	// Sent back, the effective set would copy every builtin into the
	// company's words.
	DeclaredTypes []TaskType `json:"declared_types"`

	// Fields are the WORKSPACE's declarations only: a project declares its
	// own beside them, and a task's effective set is the union.
	Fields []FieldDef `json:"fields"`

	// OptionsTotal is how many options the returned fields declare between
	// them, and OptionsShown how many this answer carries: together they
	// tell a reader whether it sees all the choices, since the lists are
	// paged ([MaxCatalogueOptions]) and a model must not choose from a
	// list it wrongly believes complete.
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

	Level   statelog.ReadLevel
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound in RECORDS, which is what the broker
	// answers; MaxLag is derived from it through this node's drain rate.
	// The read refuses past whichever set bound is reached first.
	MaxLagSeq uint64
}

// Catalogue answers what a task may be and what it may carry.
func (r *Reader) Catalogue(ctx context.Context, q CatalogueQuery) (CatalogueAnswer, error) {
	if q.Level == "" {
		return CatalogueAnswer{}, fmt.Errorf("worklet: this catalogue read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	var out CatalogueAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     catalogueScope(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
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
		out.DeclaredTypes = append([]TaskType{}, types.Types...)
		out.Fields = fields.Fields
		out.PolicyVersion = fields.PolicyVersion
		out.TypesVersion, out.FieldsVersion = types.Version, fields.Version
		if !q.Archived {
			out.Types = liveTypes(out.Types)
			out.Fields = liveFields(out.Fields)
		}
		// Counted BEFORE pageOptions cuts the lists, so OptionsTotal says
		// what the page left out.
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
// The caps allow 64 fields of 128 options, 8 192 rows: nearly a megabyte,
// fourteen times one tool answer's ceiling. So a deep catalogue is paged, not
// refused: a model needs only the options of the ONE field it is setting.
// 256 is two maximal lists, or every option of an ordinary catalogue, sized
// against what sits beside them: at their caps the 64 types and 64 fields are
// ≈ 37 KiB, leaving the options ≈ 27 KiB, and 256 rows encode at ≈ 20 KiB.
// `TestEveryToolAnswerFitsToolAnswerBytes` re-measures that when a cap moves.
const MaxCatalogueOptions = 256

// pageOptions cuts the option lists to the answer's budget WHOLE FIELD BY
// WHOLE FIELD, and says how many rows survived. Half a list is worse than
// none, since a model would choose from it believing it the set, so a field
// whose list does not fit is returned with NO options and the counts say what
// is missing.
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
