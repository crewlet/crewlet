package main

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// qwenFrontmatter is Qwen Code 0.28.4's own pattern for a rule's frontmatter,
// matched once a BOM is dropped and every line end is LF.
var qwenFrontmatter = regexp.MustCompile(`^---\n([\s\S]*?)\n---(?:\n|$)([\s\S]*)$`)

// qwenComment is an HTML comment, which Qwen Code drops from a rule's body.
var qwenComment = regexp.MustCompile(`<!--[\s\S]*?-->`)

// qwenRule is what Qwen Code 0.28.4 loads of a .qwen/rules file at a
// session's start, and whether it loads it (read in its source and measured,
// B2b's rules rows): the body after the frontmatter, HTML comments dropped,
// trimmed, none where that is empty, or where the first of s's keys present
// names a path ([qwenScopedAway]).
func qwenRule(raw []byte, s *treeScope) ([]byte, bool) {
	text := strings.TrimPrefix(string(raw), "\uFEFF")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	body := text
	if m := qwenFrontmatter.FindStringSubmatch(text); m != nil {
		if s != nil && qwenScopedAway(m[1], *s) {
			return nil, false
		}
		body = m[2]
	}
	for prev := ""; prev != body; {
		prev, body = body, qwenComment.ReplaceAllString(body, "")
	}
	body = strings.TrimSpace(strings.ReplaceAll(body, "<!--", ""))
	return []byte(body), body != ""
}

// qwenScopedAway reports whether front, a rule's frontmatter, names a path
// under the first of s's keys it holds, read as Qwen Code 0.28.4 reads it: as
// YAML, where it parses to a mapping with each key once, else by its line
// reader ([qwenLineValue]); a list's entries, nulls dropped, each as
// JavaScript's String gives it, one non-empty naming a path, unless an entry
// holds a mapping, on which Qwen Code's reading throws and loads the rule at
// start; a non-empty string naming one; anything else none.
func qwenScopedAway(front string, s treeScope) bool {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(front), &doc) != nil || len(doc.Content) == 0 ||
		doc.Content[0].Kind != yaml.MappingNode || duplicateKeys(doc.Content[0]) {
		return qwenLineScoped(front, s)
	}
	fields := doc.Content[0].Content
	for _, key := range s.Keys {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i].Value != key {
				continue
			}
			v := deref(fields[i+1])
			switch {
			case v.Kind == yaml.SequenceNode:
				named := false
				for _, e := range v.Content {
					text, kept, mapping := qwenEntry(e)
					if mapping {
						return false
					}
					named = named || (kept && text != "")
				}
				return named
			case v.Kind == yaml.ScalarNode && (v.Tag == "!!str" || v.Tag == "!!timestamp"):
				return v.Value != ""
			}
			return false
		}
	}
	return false
}

// qwenEntry is a list entry as JavaScript's String gives it once Qwen Code
// has dropped its nulls: whether it is kept (a null is not), and whether it
// holds a mapping. A list's entries join with `,`. Only whether the text is
// empty decides anything, so a scalar's text is its YAML source.
func qwenEntry(n *yaml.Node) (text string, kept, mapping bool) {
	n = deref(n)
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value, n.Tag != "!!null", false
	case yaml.SequenceNode:
		var parts []string
		for _, c := range n.Content {
			t, k, m := qwenEntry(c)
			if m {
				return "", true, true
			}
			if k {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, ","), true, false
	}
	return "", true, true
}

// deref is n, or the node an alias n names.
func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// duplicateKeys reports whether mapping m holds a key twice, which Qwen Code's
// YAML parser refuses, so it reads the frontmatter by its line reader.
func duplicateKeys(m *yaml.Node) bool {
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if seen[m.Content[i].Value] {
			return true
		}
		seen[m.Content[i].Value] = true
	}
	return false
}

// qwenLineScoped reports whether front names a path under the first of s's
// keys its line reader yields ([qwenLineValue]): a list with an item other
// than `null`, empty or `""`; a value that reads as a non-empty string, not
// `true`, `false`, `null` or a number.
func qwenLineScoped(front string, s treeScope) bool {
	values := qwenLineValues(front)
	for _, key := range s.Keys {
		v, ok := values[key]
		if !ok {
			continue
		}
		switch {
		case v.object:
			return false
		case v.list != nil:
			for _, item := range v.list {
				if item != "null" && item != "" && item != `""` {
					return true
				}
			}
			return false
		}
		return qwenLineString(v.scalar)
	}
	return false
}

// qwenLineValue is a key's value as Qwen Code's line reader leaves it: a list
// of `  - ` items, an object of indented lines, or a scalar's text.
type qwenLineValue struct {
	list   []string
	object bool
	scalar string
}

// qwenLineValues is Qwen Code 0.28.4's line reader, its fallback where its
// YAML parser fails, ported line for line: blank and `#` lines dropped; `k:
// v` a scalar; `k:` then `  - ` lines a list, or then indented lines an object.
func qwenLineValues(front string) map[string]qwenLineValue {
	var lines []string
	for _, l := range strings.Split(front, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			lines = append(lines, l)
		}
	}
	values := map[string]qwenLineValue{}
	key, objectKey := "", ""
	var list []string
	inList, inObject := false, false
	for i, l := range lines {
		if item, ok := strings.CutPrefix(l, "  - "); ok {
			if !inList {
				inList, list = true, []string{}
			}
			list = append(list, strings.TrimSpace(item))
			continue
		}
		if inList {
			values[key] = qwenLineValue{list: list}
			inList, list, key = false, nil, ""
		}
		if strings.HasPrefix(l, "  ") && inObject {
			continue
		}
		if inObject {
			values[objectKey] = qwenLineValue{object: true}
			inObject, objectKey = false, ""
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			values[strings.TrimSpace(k)] = qwenLineValue{scalar: v}
			continue
		}
		key = strings.TrimSpace(k)
		if i+1 < len(lines) && !strings.HasPrefix(lines[i+1], "  - ") && strings.HasPrefix(lines[i+1], "  ") {
			inObject, objectKey, key = true, key, ""
		}
	}
	if inList {
		values[key] = qwenLineValue{list: list}
	}
	if inObject {
		values[objectKey] = qwenLineValue{object: true}
	}
	return values
}

// qwenLineString reports whether the line reader's scalar v reads as a
// non-empty string: not `true`, `false` or `null`, not a quoted empty string,
// and not a number as JavaScript's Number reads one, finite.
func qwenLineString(v string) bool {
	switch v {
	case "true", "false", "null":
		return false
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return len(v) > 2
	}
	return !jsFiniteNumber(v)
}

// jsFiniteNumber reports whether JavaScript's Number reads v, trimmed and not
// empty, as a finite number: a decimal, or an unsigned 0x, 0o or 0b integer;
// no underscore, and Infinity not finite.
func jsFiniteNumber(v string) bool {
	if strings.Contains(v, "_") {
		return false
	}
	if len(v) > 2 && v[0] == '0' {
		if base, ok := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[v[1]]; ok {
			_, err := strconv.ParseUint(v[2:], base, 64)
			return err == nil
		}
	}
	if strings.ContainsAny(v, "xXpP") {
		return false
	}
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}
