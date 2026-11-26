package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// THE LAUNCH'S BUDGET (launchConfig.Budget): a harness that renders the home
// instruction file the launch writes into one bound it shares with what else
// it loads at the start would cut a person's own text where the sections took
// it all. Muse Code 1.4.25 renders the home's rules file (the own file's
// text, then the sections) and W's own (the first of the slots' names there,
// none where it is whitespace), markup escaped, into one reminder of at most
// 65,536 bytes, cutting what passes it (measured, rows capa and capb), so
// each checkout is first named ("read <file> for its instructions"), then
// handed whole, in order, while that reminder fits by Muse's exact
// rendering; the home's and W's own text are never cut for them.

// budgetMuse is the [launchBudget] form of Muse Code 1.4.25's rules reminder.
const budgetMuse = "muse"

// launchBudget is the bound the launch's sections share with what the harness
// loads beside them: Form, whose rendering counts ([budgetMuse]), and Own, the
// file holding the text the home instruction file opens with before the
// sections (the run's script writes it; none where the file is not there).
type launchBudget struct {
	Form string `json:"form"`
	Own  string `json:"own"`
}

// The reminder Muse Code 1.4.25 renders the rules files it loads at the start
// into (measured, rows capb and fr3): this opening, then each file wrapped in
// its tag ([museTag]) and [museFileClose], [museFilesApart] between two, then
// [museRulesClose]; at most [museRulesCap] bytes in all, past which it cuts.
const (
	museRulesOpen = "<system-reminder source=\"rules-file\">\nMuse Code loaded standing rules at session open. " +
		"Follow higher-priority instructions first. If user and project rules conflict, project rules win.\n\n"
	museRulesClose = "\n</system-reminder>"
	museFileClose  = "</rules-file>"
	museFilesApart = len("\n\n")
	museRulesCap   = 65536
)

// museUserPath is the path Muse Code 1.4.25 names the home's rules file by,
// $CONFIG_DIR/AGENTS.md or $CONFIG_DIR/CLAUDE.md, the same length (measured).
const museUserPath = "$CONFIG_DIR/AGENTS.md"

// museTag is a rules file's opening tag in the reminder, its path as Muse names
// it: the start directory's relative to it.
func museTag(scope, path string) string {
	return `<rules-file scope="` + scope + `" path="` + path + "\">\n"
}

// museBodyBytes is the most bytes text takes as a rules file's body: its own
// ([museTextBytes]) and the line break Muse adds where it does not end in one
// (measured, row capa).
func museBodyBytes(text string) int {
	if strings.HasSuffix(text, "\n") {
		return museTextBytes(text)
	}
	return museTextBytes(text) + 1
}

// museTextBytes is the most bytes text takes in the reminder: `<`, `>`, `&`,
// `"` and `'` as the entities Muse writes for them (measured, row esc1), and
// each byte that is no UTF-8 as the three of U+FFFD, which a decoder may put
// in its place. It adds up over pieces joined, an upper bound where a join
// mends a split character.
func museTextBytes(text string) int {
	n := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			n += len("\uFFFD")
		case r < utf8.RuneSelf && museEntity[byte(r)] != "":
			n += len(museEntity[byte(r)])
		default:
			n += size
		}
		i += size
	}
	return n
}

// museEntity is what Muse Code 1.4.25 writes in a rules file's body for each
// character it escapes (measured, row esc1); every other byte goes in as it is.
var museEntity = [utf8.RuneSelf]string{'<': "&lt;", '>': "&gt;", '&': "&amp;", '"': "&quot;", '\'': "&apos;"}

// museEscape is text as Muse Code puts it in a rules file's body ([museEntity]).
func museEscape(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if c := text[i]; c < utf8.RuneSelf && museEntity[c] != "" {
			b.WriteString(museEntity[c])
			continue
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

// museSectionsApart is what the run's script writes between the home file's
// own text and the sections, and the launch between two sections.
const museSectionsApart = len("\n\n")

// museSections is the launch's sections under Muse Code's reminder bound
// ([launchBudget]): every checkout with instructions named first ("read
// <file> for its instructions"), then each made whole, in order, where the
// reminder holding the home file's own text ([launchBudget.Own]), the
// sections after it as the run's script joins them, and the start
// directory's own rules file (the first of slots' names there) still fits;
// one too big is named, said. Neither the home's own text nor the start
// directory's is ever cut for them; a name that would cut one is left out,
// said.
func museSections(b launchBudget, work string, slots []instructionSlot, handed []handedCheckout, said *skips) []string {
	own, _ := readBounded(b.Own, museRulesCap+1)
	fixed := len(museRulesOpen) + len(museRulesClose) + len(museTag("user", museUserPath)) + len(museFileClose)
	if len(own) > 0 {
		fixed += museTextBytes(string(own)) + museSectionsApart
	}
	if rel, text, ok := museStartFile(work, slots); ok {
		fixed += museFilesApart + len(museTag("project", rel)) + museBodyBytes(text) + len(museFileClose)
	}
	var whole, named []string
	var roots []string
	for _, h := range handed {
		if h.text == "" {
			continue
		}
		files := make([]string, len(h.parts))
		for i, p := range h.parts {
			files[i] = p.path
		}
		label := "for `" + h.name + "` with root " + h.root + "\n\n"
		whole = append(whole, label+h.text)
		named = append(named, label+"read "+strings.Join(files, ", ")+" for its instructions, which pass what "+
			"Muse Code loads at the start beside the home's and the start directory's own")
		roots = append(roots, h.root)
	}
	fits := func(blocks []string) bool {
		if len(blocks) == 0 {
			return true
		}
		n := fixed + museSectionsApart*(len(blocks)-1)
		for _, block := range blocks {
			n += museTextBytes(block)
		}
		if !strings.HasSuffix(blocks[len(blocks)-1], "\n") {
			n++
		}
		return n <= museRulesCap
	}
	blocks := named
	for len(blocks) > 0 && !fits(blocks) {
		blocks = blocks[:len(blocks)-1]
	}
	for _, root := range roots[len(blocks):] {
		said.say("the instructions of %s are left out: even naming them would cut the home's or the start "+
			"directory's own rules out of Muse Code's %d-byte rules reminder", root, museRulesCap)
	}
	blocks = append([]string(nil), blocks...)
	for i := range blocks {
		name := blocks[i]
		if blocks[i] = whole[i]; !fits(blocks) {
			blocks[i] = name
			said.say("the instructions of %s are named, not handed: whole they pass what is left of Muse Code's "+
				"%d-byte rules reminder beside the home's and the start directory's own", roots[i], museRulesCap)
		}
	}
	return blocks
}

// museStartFile is the start directory's own rules file as Muse Code renders
// it, the first regular file of slots' names in work, its path relative to
// work as Muse prints it (measured, rows tp1-tp3) and its text (read to just
// past the bound, which is all the count needs); false for none, and for a
// first file of whitespace alone, which Muse renders nothing of and falls
// through nowhere from (measured, rows em5 and em6).
func museStartFile(work string, slots []instructionSlot) (string, string, bool) {
	for _, slot := range slots {
		for _, name := range slot.Names {
			path := filepath.Join(work, name)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			raw, err := readBounded(path, museRulesCap+1)
			if err != nil {
				continue
			}
			whole := info.Size() <= int64(len(raw))
			return name, string(raw), !whole || strings.TrimSpace(string(raw)) != ""
		}
	}
	return "", "", false
}

// museFrontName is the names a SKILL.md's frontmatter may have Muse Code
// 1.4.25 list its skill under, "" for its directory's, and whether it can be
// read plainly: a file opening with `---` and a `name:` line before the `---`
// that closes it, read no further ([scanFrontmatter]), its value a one-line
// YAML scalar ([yamlScalar]); none, or an empty or null value, is the
// directory's; a number or boolean, its text or the directory's. A value no
// one-line reading settles (a block, flow, anchor, tag or alias, an escape,
// one a later indented line continues) is not read plainly: never guessed.
func museFrontName(path string) ([]string, bool) {
	var names []string
	plain, seen := true, false
	scanFrontmatter(path, func(l string) bool { return l == "---" }, func(l string) bool {
		if seen { // a later indented line continues the value; a blank one may too
			if strings.TrimSpace(l) == "" {
				return false
			}
			plain = plain && l[0] != ' ' && l[0] != '\t'
			return true
		}
		if key, ok := frontKey(l); !ok || key != "name" {
			return false
		}
		_, value, _ := strings.Cut(l, ":")
		name, ok := yamlScalar(strings.TrimSpace(value))
		_, number := strconv.ParseFloat(name, 64)
		switch {
		case !ok:
			plain = false
		case name == "" || name == "~" || strings.EqualFold(name, "null"):
		case number == nil || strings.EqualFold(name, "true") || strings.EqualFold(name, "false"):
			names = []string{name, ""}
		default:
			names = []string{name}
		}
		seen = true
		return false
	})
	if len(names) == 0 {
		names = []string{""}
	}
	return names, plain
}

// yamlScalar is a one-line YAML scalar's value, and whether one line settles
// it: a quoted one's inside (in single quotes, a doubled quote read as one),
// then nothing but a comment; a plain one up to a comment (` #` or a leading
// `#`), trimmed. Not settled: a block, flow, anchor, tag, alias or reserved
// indicator first, a double-quoted escape, a quote closed on a later line, and
// a plain value holding `: ` or ending in `:`, which a parser refuses.
func yamlScalar(v string) (string, bool) {
	switch {
	case v == "" || v[0] == '#':
		return "", true
	case strings.ContainsRune("|>[{&!*@`%", rune(v[0])),
		len(v) > 1 && strings.ContainsRune("-?:", rune(v[0])) && (v[1] == ' ' || v[1] == '\t'):
		return "", false
	case v[0] == '"' || v[0] == '\'':
		q := v[0]
		for i := 1; i < len(v); i++ {
			switch {
			case q == '"' && v[i] == '\\':
				return "", false
			case v[i] == q && q == '\'' && i+1 < len(v) && v[i+1] == '\'':
				i++
			case v[i] == q:
				rest := strings.TrimLeft(v[i+1:], " \t")
				if rest != "" && (rest[0] != '#' || len(rest) == len(v[i+1:])) {
					return "", false
				}
				return strings.ReplaceAll(v[1:i], string(q)+string(q), string(q)), true
			}
		}
		return "", false
	}
	for j := 1; j < len(v); j++ {
		if v[j] == '#' && (v[j-1] == ' ' || v[j-1] == '\t') {
			v = strings.TrimSpace(v[:j])
			break
		}
	}
	if strings.Contains(v, ": ") || strings.Contains(v, ":\t") || strings.HasSuffix(v, ":") {
		return "", false
	}
	return v, true
}
