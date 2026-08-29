package knowledge

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"gopkg.in/yaml.v3"
)

// Reading a directory of authored markdown, for the publishing CLIs.
//
// # What a knowledge doc IS, on disk
//
// A `.md` file. Its TITLE is the first `# H1`, its CONTAINER is the name of
// the directory holding it, and its BODY is everything after the heading.
// Optional YAML frontmatter can override the title and the container, name a
// PARENT page to nest under, and attach LABELS — and nothing else. A file's
// location in the tree is the authoring convention, and a frontmatter that
// could redirect a doc anywhere would make the tree meaningless; `parent:`
// is not a redirection, it is the one thing a flat directory of files cannot
// express about a wiki that has trees in it.
//
// # Why the title comes from the H1 rather than the filename
//
// The title is the page name at the backend AND half the idempotency key,
// so a re-import has to derive the same one. A filename is the thing an
// operator changes most casually — a rename would orphan the published page
// and create a second one beside it — while the H1 is content they edit
// deliberately.

// FrontmatterPattern splits a leading `---` block from the body.
//
// THE SAME GRAMMAR the skill parser uses, and deliberately: one file format
// with two readers that disagreed about where the frontmatter ends is a
// file that publishes as a skill and imports as a doc.
var FrontmatterPattern = regexp.MustCompile(`(?s)\A---\s*\n(.*?\n)---\s*\n?(.*)\z`)

// headingPattern matches the first ATX H1.
var headingPattern = regexp.MustCompile(`(?m)^#\s+(.+?)\s*$`)

// Doc is one authored knowledge document.
type Doc struct {
	// Path is where it was read from, for the report.
	Path string
	// Title is the page name, and half the idempotency key.
	Title string
	// Container is the backend container it belongs in — a Confluence
	// space key today, named neutrally for the same reason the searcher's
	// is.
	Container string
	// Markdown is the body, with the title heading removed: the backend
	// renders the title itself, so leaving it would show it twice.
	Markdown string
	// Parent is the TITLE of the page this one nests under, within the
	// same container. A title rather than an id, because an id is a thing
	// the backend minted and an authored file cannot know one — and
	// because the parent is very often published by the same run, minutes
	// before this page exists.
	Parent string
	// Labels are the author's own page labels. Confluence has a
	// first-class field for them; a backend without one must say so rather
	// than dropping them silently.
	Labels []string
}

// docFrontmatter is the only thing a knowledge doc may declare.
//
// Decoded LOOSELY, unlike a skill's: a knowledge doc is prose somebody
// wrote, and a key this build does not know is far more likely to be a note
// to a human than a typo that silently disables something.
type docFrontmatter struct {
	Title     string   `yaml:"title"`
	Container string   `yaml:"space"`
	Project   string   `yaml:"project"`
	Parent    string   `yaml:"parent"`
	Labels    []string `yaml:"labels"`
}

// ParseDoc reads a knowledge doc from a file.
//
// The CONTAINER defaults to the parent directory's name, which is the
// authoring convention: a file at `<root>/ENG/onboarding.md` belongs to
// ENG. Frontmatter may name it explicitly — `space:` or `project:`, the two
// backends' words for the same thing — for a doc that lives somewhere the
// tree cannot express.
func ParseDoc(path string) (Doc, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, fmt.Errorf("knowledge: %s: %w", path, err)
	}
	text := string(body)

	var fm docFrontmatter
	if match := FrontmatterPattern.FindStringSubmatch(text); match != nil {
		if err := yaml.Unmarshal([]byte(match[1]), &fm); err != nil {
			return Doc{}, fmt.Errorf("knowledge: %s: invalid frontmatter: %w", path, err)
		}
		text = match[2]
	}

	doc := Doc{
		Path:      path,
		Title:     strings.TrimSpace(fm.Title),
		Container: firstNonEmpty(fm.Container, fm.Project, filepath.Base(filepath.Dir(path))),
		Parent:    strings.TrimSpace(fm.Parent),
		Labels:    cleanLabels(fm.Labels),
	}
	heading := headingPattern.FindStringSubmatchIndex(text)
	if doc.Title == "" {
		if heading == nil {
			// NAMED AS THE FIX rather than skipped quietly: a doc with
			// no title cannot be published or found again, and the
			// operator's remedy is one line at the top of the file.
			return Doc{}, fmt.Errorf(
				"knowledge: %s has no title: give it a `# Heading` first line, "+
					"or a `title:` in its frontmatter — the title is the page "+
					"name and half of what finds the page again on a re-import",
				path)
		}
		doc.Title = strings.TrimSpace(text[heading[2]:heading[3]])
	}
	if heading != nil {
		// THE HEADING IS REMOVED, because the backend renders the page
		// title itself and leaving it prints the same words twice.
		text = text[:heading[0]] + text[heading[1]:]
	}
	doc.Markdown = strings.TrimSpace(text)
	return doc, nil
}

// CollectMarkdown walks a directory for `.md` files, in a stable order.
//
// SORTED, because the report is read by a person and a run that listed its
// files in filesystem order would produce a different report every time
// against an unchanged tree.
func CollectMarkdown(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			// Dot-directories are tooling, not content: `.git` alone
			// would otherwise contribute every markdown file in every
			// checked-out dependency.
			if name := entry.Name(); strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		case strings.EqualFold(filepath.Ext(path), ".md"):
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: walk %s: %w", root, err)
	}
	return out, nil
}

// markdown is the renderer, built once.
//
// GFM, because that is what the authored files are: the tables, task lists
// and autolinks in this repo's own knowledge docs render as literal pipes
// and brackets under plain CommonMark.
//
// Raw HTML passes through unescaped, deliberately: the backends sanitize
// what they accept, and escaping here would publish an author's deliberate
// `<br>` as visible text while the sanitizer was the thing actually
// deciding what is allowed.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// RenderMarkdown turns authored markdown into the HTML a page carries.
func RenderMarkdown(source string) (string, error) {
	var out bytes.Buffer
	if err := markdown.Convert([]byte(source), &out); err != nil {
		return "", fmt.Errorf("knowledge: render: %w", err)
	}
	return out.String(), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// cleanLabels normalises an author's label list.
//
// LOWER-CASED and de-duplicated, because Confluence stores labels lower-case
// and answers with what it stored: a file declaring `Runbook` would otherwise
// look like it had lost its label on every re-read, and a run that then
// re-added it would write on every import for ever.
func cleanLabels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, label := range in {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}
