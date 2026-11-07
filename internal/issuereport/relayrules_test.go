package issuereport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// exportedCatalogue is the generated export, as a relay reads it.
type exportedCatalogue struct {
	Schema   int                 `json:"schema"`
	Enums    map[string][]string `json:"enums"`
	Patterns map[string]string   `json:"patterns"`
	Limits   map[string]int      `json:"limits"`
	Classes  []exportedClass     `json:"classes"`
}

type exportedClass struct {
	ID         string `json:"id"`
	Component  string `json:"component"`
	Site       string `json:"site"`
	SiteVaries bool   `json:"site_varies"`
	SiteKind   string `json:"site_kind"`
	Frames     bool   `json:"frames"`
	Harness    bool   `json:"harness"`
	Facts      []struct {
		Key      string   `json:"key"`
		Type     string   `json:"type"`
		Enum     []string `json:"enum"`
		Max      int64    `json:"max"`
		Optional bool     `json:"optional"`
	} `json:"facts"`
	Identity []string `json:"identity"`
}

// relayRefusal is why a relay would refuse this report, nil if it would take it.
// It reads only the export and the bytes, never this package's types, so a wired
// site that omits what the export requires fails here as it would at the relay.
func relayRefusal(export, body []byte) error {
	var catalogue exportedCatalogue
	if err := json.Unmarshal(export, &catalogue); err != nil {
		return fmt.Errorf("the export: %w", err)
	}
	var report map[string]any
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&report); err != nil {
		return fmt.Errorf("the report: %w", err)
	}
	if report["schema"] != float64(catalogue.Schema) {
		return fmt.Errorf("schema %v is not the export's %d", report["schema"], catalogue.Schema)
	}
	id, _ := report["class"].(string)
	i := slices.IndexFunc(catalogue.Classes, func(c exportedClass) bool { return c.ID == id })
	if i < 0 {
		return fmt.Errorf("class %q is not in the export", id)
	}
	class := catalogue.Classes[i]

	allowed := []string{"schema", "class", "fingerprint", "component", "site", "engine", "topology", "occurrence", "facts"}
	optional := []string{}
	site, _ := report["site"].(string)
	migration := class.SiteKind == "migration" && site != class.Site
	if site != class.Site {
		if !class.SiteVaries {
			return fmt.Errorf("site %q on a class fixed at %q", site, class.Site)
		}
		if !migration {
			allowed = append(allowed, "site_file")
		}
	}
	if class.Harness {
		allowed = append(allowed, "harness")
	}
	if class.Frames {
		optional = append(optional, "frames")
	}
	if err := exactly(report, "report", allowed, optional); err != nil {
		return err
	}
	pattern := func(name string, value any) error {
		text, _ := value.(string)
		if !regexp.MustCompile(catalogue.Patterns[name]).MatchString(text) {
			return fmt.Errorf("%q is not of the pattern %s", text, name)
		}
		return nil
	}
	sitePattern := "site"
	if migration {
		sitePattern = "site.migration"
	}
	if err := pattern(sitePattern, site); err != nil {
		return err
	}
	component, _ := report["component"].(string)
	wantComponent := class.Component
	if site != class.Site && !migration {
		wantComponent, _, _ = strings.Cut(site, ".")
		if err := pattern("source.file", report["site_file"]); err != nil {
			return err
		}
		if file, _ := report["site_file"].(string); !strings.HasPrefix(file, wantComponent+"/") ||
			strings.Contains(strings.TrimPrefix(file, wantComponent+"/"), "/") {
			return fmt.Errorf("site_file %q is not in the package of %q", file, site)
		}
	}
	if component != wantComponent {
		return fmt.Errorf("component %q, want %q", component, wantComponent)
	}
	if want, err := recomputed(class, component, site, report); err != nil {
		return err
	} else if report["fingerprint"] != want {
		return fmt.Errorf("fingerprint %v does not recompute to %s", report["fingerprint"], want)
	}

	engine, _ := report["engine"].(map[string]any)
	if err := exactly(engine, "engine", []string{"version", "go", "os", "arch"}, []string{"commit"}); err != nil {
		return err
	}
	for field, name := range map[string]string{"version": "engine.version", "go": "engine.go"} {
		if err := pattern(name, engine[field]); err != nil {
			return err
		}
	}
	version, _ := engine["version"].(string)
	if commit, stated := engine["commit"]; stated {
		if err := pattern("engine.commit", commit); err != nil {
			return err
		}
	} else if strings.HasPrefix(version, "main-") {
		return errors.New("the image of main states its commit")
	}
	inSet := func(name string, value any) error {
		text, _ := value.(string)
		if !slices.Contains(catalogue.Enums[name], text) {
			return fmt.Errorf("%q is not in %s", text, name)
		}
		return nil
	}
	if err := errors.Join(inSet("engine.os", engine["os"]), inSet("engine.arch", engine["arch"])); err != nil {
		return err
	}

	topology, _ := report["topology"].(map[string]any)
	if err := exactly(topology, "topology", []string{"nodes", "stream", "coordination", "sandbox"}, nil); err != nil {
		return err
	}
	for _, field := range []string{"nodes", "stream", "coordination", "sandbox"} {
		if err := inSet("topology."+field, topology[field]); err != nil {
			return err
		}
	}

	harness, carried := report["harness"].(map[string]any)
	if class.Harness != carried {
		return fmt.Errorf("class %s harness flag %v, carried %v", id, class.Harness, carried)
	}
	if carried {
		if err := errors.Join(exactly(harness, "harness", []string{"name", "version"}, nil),
			inSet("harness.name", harness["name"]), pattern("harness.version", harness["version"])); err != nil {
			return err
		}
	}

	occurrence, _ := report["occurrence"].(map[string]any)
	if err := exactly(occurrence, "occurrence", []string{"count", "first_seen", "last_seen"}, nil); err != nil {
		return err
	}
	if count, _ := occurrence["count"].(float64); count < 1 || count > float64(catalogue.Limits["occurrence.count"]) {
		return fmt.Errorf("occurrence.count %v is out of range", count)
	}

	if frames, ok := report["frames"].([]any); ok {
		if len(frames) == 0 || len(frames) > catalogue.Limits["frames.count"] {
			return fmt.Errorf("%d frames", len(frames))
		}
		for n, raw := range frames {
			frame, _ := raw.(map[string]any)
			if err := errors.Join(exactly(frame, "frame", []string{"function", "file", "line"}, nil),
				pattern("site", frame["function"]), pattern("source.file", frame["file"])); err != nil {
				return err
			}
			if line, _ := frame["line"].(float64); line < 1 || line > float64(catalogue.Limits["frame.line"]) {
				return fmt.Errorf("frame line %v is out of range", line)
			}
			if n == 0 && frame["function"] != site {
				return fmt.Errorf("frames start at %v, not the site %q", frame["function"], site)
			}
		}
	}

	facts, ok := report["facts"].(map[string]any)
	if !ok {
		return errors.New("facts is not an object")
	}
	declared := make(map[string]bool, len(class.Facts))
	for _, fact := range class.Facts {
		declared[fact.Key] = true
		value, present := facts[fact.Key]
		if !present {
			if fact.Optional {
				continue
			}
			return fmt.Errorf("fact %q is required", fact.Key)
		}
		switch fact.Type {
		case "boolean":
			if _, isBool := value.(bool); !isBool {
				return fmt.Errorf("fact %q is not a boolean", fact.Key)
			}
		case "integer":
			if number, isNumber := value.(float64); !isNumber || number < 0 || number > float64(fact.Max) {
				return fmt.Errorf("fact %q is not an integer in range", fact.Key)
			}
		case "enum":
			if text, _ := value.(string); !slices.Contains(fact.Enum, text) {
				return fmt.Errorf("fact %q is not in its set", fact.Key)
			}
		default:
			return fmt.Errorf("fact %q is of type %q", fact.Key, fact.Type)
		}
	}
	for key := range facts {
		if !declared[key] {
			return fmt.Errorf("fact %q is not declared", key)
		}
	}
	return nil
}

// recomputed is the fingerprint as a relay computes it, from the export's identity
// list and the report's bytes, sharing nothing with [FingerprintOf] so that a change
// to either side shows.
func recomputed(class exportedClass, component, site string, report map[string]any) (string, error) {
	input := "crewlet-report/v1\x00" + class.ID + "\x00" + component + "\x00" + site
	for _, path := range class.Identity {
		var value any
		if path == "harness.name" {
			harness, _ := report["harness"].(map[string]any)
			value = harness["name"]
		} else {
			facts, _ := report["facts"].(map[string]any)
			value = facts[path]
		}
		switch v := value.(type) {
		case string:
			input += "\x00" + path + "=" + v
		case bool:
			input += "\x00" + path + "=" + strconv.FormatBool(v)
		default:
			return "", fmt.Errorf("identity %q holds %v", path, value)
		}
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:]), nil
}

// exactly holds an object to the keys it must have and the keys it may have.
func exactly(object map[string]any, what string, required, optional []string) error {
	if object == nil {
		return fmt.Errorf("%s is missing", what)
	}
	for _, key := range required {
		if _, present := object[key]; !present {
			return fmt.Errorf("%s has no %q", what, key)
		}
	}
	for key := range object {
		if !slices.Contains(required, key) && !slices.Contains(optional, key) {
			return fmt.Errorf("%s has a field the schema does not allow: %q", what, key)
		}
	}
	return nil
}
