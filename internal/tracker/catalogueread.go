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

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
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
		Level:       q.Level,
		Scope:       catalogueScope(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		Set:         true,
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
