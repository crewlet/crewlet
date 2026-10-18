package artifactdataapi_test

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD SPEAKS THE DATASTORE'S VOCABULARY, IN THE ENGINE'S ORDER.
//
// The schema editor offers the column types, read rules, presets and write
// conditions a collection may declare; the native sheet and the page bridge
// write the ops a write may carry and read back the verdicts the applier
// writes; and the data tab leaves the per-viewer state collection out of the
// list. Each is a closed set the engine owns: a value the engine grew that the
// dashboard lacks is a column nobody can declare or a verdict a sheet cannot
// say, and one the dashboard keeps that the engine dropped is a choice every
// write refuses.
func TestTheDashboardSpeaksTheDatastoreVocabulary(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"COLUMN_TYPES":       names(artifacts.ColumnTypes),
		"READ_RULES":         names(artifacts.ReadRules),
		"COLLECTION_PRESETS": names(artifacts.Presets),
		"FIELD_CONDITIONS":   names(artifacts.FieldConditions),
		"STATUS_CATEGORIES":  artifactdata.StatusCategories,
		"DATA_OP_KINDS":      names(artifacts.DataOpKinds),
		"DATA_VERDICTS":      names(artifacts.DataVerdicts),
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		client := clientsource.Strings(body)
		if len(client) == 0 {
			t.Fatalf("%s names nothing, so this gate certifies nothing", name)
		}
		if !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q, in that order: "+
				"change contract/artifacts.ts to the engine's", name, client, engine)
		}
	}
	state, err := clientsource.Scalar(tree, "STATE_COLLECTION")
	if err != nil {
		t.Fatal(err)
	}
	if state != artifactdata.StateCollection {
		t.Errorf("the dashboard leaves %q out of a store's collections and the engine keeps "+
			"per-viewer state in %q: the data tab would list every viewer's private state "+
			"collection as one of the author's", state, artifactdata.StateCollection)
	}
}

// THE DASHBOARD BOUNDS A WRITE AND A SCHEMA AT THE ENGINE'S CAPS.
//
// A paste that spans more cells than one write carries is split by the
// native sheet at this bound, and the schema editor stops adding collections
// and columns at these, so a person is told in place rather than refused
// whole after Save.
func TestTheDashboardBoundsTheDatastoreAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"DATA_BATCH_MAX_OPS":   artifacts.MaxBatchOps,
		"DATA_COLLECTIONS_MAX": artifacts.MaxCollections,
		"DATA_COLUMNS_MAX":     artifacts.MaxColumns,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d and the engine at %d: change "+
				"contract/artifacts.ts to the engine's figure", name, client, engine)
		}
	}
}

// THE DASHBOARD NAMES A COLLECTION AND A FIELD BY THE ENGINE'S GRAMMAR.
//
// HELD BY BEHAVIOUR, not by spelling, as the login grammars are: the schema
// editor checks a new collection's name and a new column's key against these
// before it sends the schema, and a name the two answer differently is either
// refused after Save with nothing on the field to say why, or refused at the
// field when the engine would have taken it. So both patterns are compiled
// here and a corpus is run through them, the reserved list, and the engine's
// own checks.
func TestTheDashboardNamesCollectionsAndFieldsByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	pattern := func(name string) *regexp.Regexp {
		source, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		re, err := regexp.Compile(source)
		if err != nil {
			t.Fatalf("%s does not compile: %v", name, err)
		}
		return re
	}
	collection, field := pattern("COLLECTION_NAME_PATTERN"), pattern("FIELD_NAME_PATTERN")
	body, err := clientsource.Literal(tree, "RESERVED_COLLECTIONS")
	if err != nil {
		t.Fatal(err)
	}
	reserved := clientsource.Strings(body)
	if len(reserved) == 0 {
		t.Fatal("RESERVED_COLLECTIONS names nothing, so the corpus below checks half a rule")
	}
	collections := []string{
		"vendors", "reviews", "a", "a1", "audit_log", "x_", "rows2024",
		"schema", "history", "state", "writes", "undo", "export", "query", "data",
		"_state", "_private", "Vendors", "1vendors", "vendors-list", "vendor list", "vendors.x",
		"", "é", strings.Repeat("a", artifacts.MaxCollectionNameBytes),
		strings.Repeat("a", artifacts.MaxCollectionNameBytes+1),
	}
	for _, name := range collections {
		dashboard := collection.MatchString(name) && !slices.Contains(reserved, name)
		if engine := artifactdata.ValidCollectionName(name); dashboard != engine {
			t.Errorf("collection %q: the dashboard says %v and the engine %v", name, dashboard, engine)
		}
	}
	for _, name := range reserved {
		if !collection.MatchString(name) {
			t.Errorf("%q is listed as reserved and does not match the grammar either, so the "+
				"list says nothing the pattern does not", name)
		}
	}
	fields := []string{
		"score", "Score", "a", "a_b", "vendor2", "approvedBy",
		"_id", "_rank", "1score", "score-x", "score x", "score.x", "", "é",
		strings.Repeat("f", artifacts.MaxFieldNameBytes),
		strings.Repeat("f", artifacts.MaxFieldNameBytes+1),
	}
	for _, name := range fields {
		if dashboard, engine := field.MatchString(name), query.ValidField(name); dashboard != engine {
			t.Errorf("field %q: the dashboard says %v and the engine %v", name, dashboard, engine)
		}
	}
}

// names is a closed set's values as the strings the dashboard declares.
func names[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}
