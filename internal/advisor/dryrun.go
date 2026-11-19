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
	var entity any
	switch ch.Entity {
	case EntityRoles:
		for r := range copied.EachRole() {
			if r.Seat().Handle() == ch.ID {
				entity = r
			}
		}
	case EntityUnits:
		for u := range copied.EachUnit() {
			if u.ID == ch.ID {
				entity = u
			}
		}
	default:
		return fmt.Errorf("advisor: a change to %s is not checked here", ch.Entity)
	}
	if entity == nil {
		return fmt.Errorf("advisor: no %s %q to change", ch.Entity, ch.ID)
	}
	if err := applyTo(entity, ch); err != nil {
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
	fields, err := Changed(entity, ch)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, entity)
}

// asFields is v's JSON as a map of its fields.
func asFields(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	return fields, json.Unmarshal(raw, &fields)
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
