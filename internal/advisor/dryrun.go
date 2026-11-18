package advisor

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/config"
)

// CheckChange applies ch to a copy of c as the dashboard sends it (merge, then set) and refuses
// it where the copy breaks an admission rule c keeps: an analyst's fix passes it before it is
// listed, and the entity route checks again when a person previews it.
func CheckChange(c *config.Company, ch Change) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	copied := &config.Company{}
	if err := json.Unmarshal(raw, copied); err != nil {
		return err
	}
	if ch.Entity != EntityRoles {
		return fmt.Errorf("advisor: a change to %s is not checked here", ch.Entity)
	}
	var role *config.Role
	for r := range copied.EachRole() {
		if r.Seat().Handle() == ch.ID {
			role = r
		}
	}
	if role == nil {
		return fmt.Errorf("advisor: no seat %q to change", ch.ID)
	}
	if err := applyTo(role, ch); err != nil {
		return err
	}
	before := len(config.Problems(c.ValidateAdmission()))
	if err := copied.ValidateAdmission(); len(config.Problems(err)) > before {
		return fmt.Errorf("advisor: the change breaks a rule: %w", err)
	}
	return nil
}

// applyTo merges ch.Merge into entity key by key, then replaces ch.Set's fields.
func applyTo(entity any, ch Change) error {
	raw, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	merge, err := asJSON(ch.Merge)
	if err != nil {
		return err
	}
	set, err := asJSON(ch.Set)
	if err != nil {
		return err
	}
	fields = mergeFields(fields, merge)
	maps.Copy(fields, set)
	if raw, err = json.Marshal(fields); err != nil {
		return err
	}
	return json.Unmarshal(raw, entity)
}

func asJSON(v map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	return out, json.Unmarshal(raw, &out)
}

// mergeFields merges from into into, nested objects key by key.
func mergeFields(into, from map[string]any) map[string]any {
	out := maps.Clone(into)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range from {
		had, hadObject := out[k].(map[string]any)
		if next, isObject := v.(map[string]any); isObject && hadObject {
			out[k] = mergeFields(had, next)
			continue
		}
		out[k] = v
	}
	return out
}
