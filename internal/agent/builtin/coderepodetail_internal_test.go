package builtin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// CODE_GET_REPO CARRIES EVERY FIELD OF A REPOSITORY'S DETAIL, OR SAYS WHY NOT.
// Its detail is a copy of gitlet.RepoDetail made field by field, and nothing
// failed when the detail grew a field the copy did not take: the purge a
// person asked for (expunge) and an import's end (remotes) were read with the
// rest of the row and dropped, so the purge's own answer sent its asker to
// read what code_get_repo never answered. Every field the detail declares
// beside its summary — the row every listing answers, which repoRowOf copies
// — is either one this answer carries, by its JSON name, or one named here
// with the reason it is left out.
//
// Mutation: drop Remotes from codeRepoDetail → red.
func TestCodeGetRepoCarriesEveryDetailFieldOrSaysWhy(t *testing.T) {
	t.Parallel()
	leftOut := map[string]string{
		"tombstones": "the deleted refs a copy syncing a delta learns of; nothing a reader of one " +
			"repository acts on",
		"limits": "the serving node's epoch's, what an import chunks to: the company's code limits are " +
			"its document's",
		"path_owners": "answered only to a read that asks for them, and code_get_repo asks for its " +
			"rules' gates alone",
		"gates": "answered beside the detail, whole, each with the rule it holds a merge to (protections)",
		"rule_sources": "answered beside the detail, whole, each on the rule it says who last set and " +
			"when (protections)",
	}
	jsonName := func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		return name
	}
	carried := map[string]bool{}
	var collect func(rt reflect.Type)
	collect = func(rt reflect.Type) {
		for i := range rt.NumField() {
			if f := rt.Field(i); f.Anonymous {
				collect(f.Type)
			} else {
				carried[jsonName(f)] = true
			}
		}
	}
	collect(reflect.TypeFor[codeRepoDetail]())
	detail := reflect.TypeFor[gitlet.RepoDetail]()
	declared := map[string]bool{}
	for i := range detail.NumField() {
		f := detail.Field(i)
		if f.Anonymous {
			continue
		}
		name := jsonName(f)
		declared[name] = true
		_, out := leftOut[name]
		switch {
		case carried[name] && out:
			t.Errorf("gitlet.RepoDetail.%s (%q) is carried and named left out too: take it off the list", f.Name,
				name)
		case !carried[name] && !out:
			t.Errorf("gitlet.RepoDetail.%s (%q) is neither carried by code_get_repo's detail nor named left "+
				"out with why: carry it, or say why a reader has no use for it", f.Name, name)
		}
	}
	for name, why := range leftOut {
		if !declared[name] {
			t.Errorf("%q (%s) is named left out, and gitlet.RepoDetail has no such field: take it off the list",
				name, why)
		}
	}
}
