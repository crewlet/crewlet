package configapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// Sections are the company document's top-level keys, which `GET /config?section=`
// reads one of: config.Company's JSON member names.
var Sections = sync.OnceValue(func() []string {
	var out []string
	t := reflect.TypeFor[config.Company]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" && t.Field(i).IsExported() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
})

// writeSection answers one top-level section of the redacted document: the
// section's own value, as `GET /config/{kind}/{id}` answers the entity itself.
func (s *Service) writeSection(w http.ResponseWriter, r *http.Request, company *config.Company,
	section string) {

	if !slices.Contains(Sections(), section) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "section " + section + " is not a section of the " +
				"company document; it is one of " + words.Or(Sections()) + "."})
		return
	}
	encoded, err := json.Marshal(company)
	if err != nil {
		s.fail(w, r, "encode the config", err)
		return
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		s.fail(w, r, "decode the config", err)
		return
	}
	value, set := document[section]
	if !set {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{"hint": "The active document sets no " + section + " section."})
		return
	}
	if r.URL.Query().Get("format") != "yaml" {
		writeJSON(w, http.StatusOK, value)
		return
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		s.fail(w, r, "decode the section", err)
		return
	}
	body, err := yaml.Marshal(decoded)
	if err != nil {
		s.fail(w, r, "encode the section as yaml", err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
