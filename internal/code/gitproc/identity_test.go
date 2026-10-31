package gitproc_test

import (
	"crypto/sha1" //nolint:gosec // git's own object name, which is what the case computes, not a security hash
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// emptyTree is git's id of the tree with nothing in it: an argument the cases
// that never reach a real git give a commit-tree.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// suiteIdentity is the identity the cases give a commit-tree: an author, the
// committer an engine-built commit names, and a committer date an hour after
// the author's, as a commit held to a later parent's committer date is dated.
func suiteIdentity() gitproc.Identity {
	return gitproc.Identity{
		AuthorName: "Ada Author", AuthorEmail: "ada@example.com",
		CommitterName: "Crewlet", CommitterEmail: "engine@seats.invalid",
		AuthorWhen: time.Unix(1700000000, 0).UTC(), CommitterWhen: time.Unix(1700003600, 0).UTC(),
	}
}

// ONLY COMMIT-TREE TAKES AN IDENTITY, AND IT ALWAYS TAKES ONE: an identity on
// any other subcommand, a commit-tree with none, and an identity whose part git
// would not write as given — each of Unicode's 66 noncharacters among them — or
// whose date it cannot are each refused before any process starts, naming what
// to change; and the identity a commit-tree is given reaches its process as
// exactly the six variables git reads it from, each date in the one form git
// reads as seconds — the epoch included — while a run of any other subcommand
// sees none of them.
//
// Mutations: drop the subcommand check, and a version given an identity runs;
// drop the check of a commit-tree with none, and one runs with no identity;
// drop the noncharacter rule, and a name ending in U+FFFE runs.
func TestOnlyCommitTreeTakesAnIdentity(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	before := len(calls())
	run := func(c gitproc.Cmd) error {
		t.Helper()
		c.Class, c.Deadline = gitproc.ClassRead, gitproc.ReadDeadline
		_, err := p.Run(t.Context(), c)
		return err
	}

	id := suiteIdentity()
	for _, args := range [][]string{
		{"version"}, {"hash-object", "-w", "--stdin"}, {"mktree", "-z", "--batch"},
		{"merge-tree", "--write-tree", "a", "b"}, {"cat-file", "--batch"}, {"commit", "-m", "x"},
	} {
		if err := run(gitproc.Cmd{Args: args, Identity: &id}); !errors.Is(err, gitproc.ErrInvalid) ||
			!strings.Contains(err.Error(), "commit-tree alone") {
			t.Errorf("git %s given an identity: %v, want ErrInvalid saying an identity is commit-tree's alone", args[0], err)
		}
	}
	if err := run(gitproc.Cmd{Args: []string{"commit-tree", emptyTree}}); !errors.Is(err, gitproc.ErrInvalid) ||
		!strings.Contains(err.Error(), "carries no Cmd.Identity") {
		t.Errorf("a commit-tree with no identity: %v, want ErrInvalid saying commit-tree takes the identity its call names", err)
	}

	for name, tc := range map[string]struct {
		field string
		spoil func(*gitproc.Identity)
	}{
		"an empty author name":        {"AuthorName", func(id *gitproc.Identity) { id.AuthorName = "" }},
		"an empty author address":     {"AuthorEmail", func(id *gitproc.Identity) { id.AuthorEmail = "" }},
		"an empty committer name":     {"CommitterName", func(id *gitproc.Identity) { id.CommitterName = "" }},
		"an empty committer address":  {"CommitterEmail", func(id *gitproc.Identity) { id.CommitterEmail = "" }},
		"a NUL in an address":         {"CommitterEmail", func(id *gitproc.Identity) { id.CommitterEmail = "engine\x00@seats.invalid" }},
		"a line feed in a name":       {"AuthorName", func(id *gitproc.Identity) { id.AuthorName = "Ada\nAuthor" }},
		"a < in an address":           {"AuthorEmail", func(id *gitproc.Identity) { id.AuthorEmail = "ada<@example.com" }},
		"a > in a name":               {"CommitterName", func(id *gitproc.Identity) { id.CommitterName = "Crew>let" }},
		"a name that is not UTF-8":    {"CommitterName", func(id *gitproc.Identity) { id.CommitterName = "Crew\xfflet" }},
		"a noncharacter in an email":  {"AuthorEmail", func(id *gitproc.Identity) { id.AuthorEmail = "ada\U0000FDD0@example.com" }},
		"a name git trims at its end": {"AuthorName", func(id *gitproc.Identity) { id.AuthorName = " Ada Author" }},
		"an address git trims":        {"CommitterEmail", func(id *gitproc.Identity) { id.CommitterEmail = "engine@seats.invalid;" }},
		"an unset author date":        {"AuthorWhen", func(id *gitproc.Identity) { id.AuthorWhen = time.Time{} }},
		"an unset committer date":     {"CommitterWhen", func(id *gitproc.Identity) { id.CommitterWhen = time.Time{} }},
		"a date before the epoch":     {"AuthorWhen", func(id *gitproc.Identity) { id.AuthorWhen = time.Unix(-1, 0) }},
		"a fraction of a second":      {"AuthorWhen", func(id *gitproc.Identity) { id.AuthorWhen = time.Unix(1700000000, 5e8) }},
		"a committer's fraction":      {"CommitterWhen", func(id *gitproc.Identity) { id.CommitterWhen = time.Unix(1700003600, 1) }},
		"a commit before its author": {"CommitterWhen", func(id *gitproc.Identity) {
			id.CommitterWhen = id.AuthorWhen.Add(-time.Second)
		}},
	} {
		bad := suiteIdentity()
		tc.spoil(&bad)
		if err := run(gitproc.Cmd{Args: []string{"commit-tree", emptyTree}, Identity: &bad}); !errors.Is(err, gitproc.ErrInvalid) ||
			!strings.Contains(err.Error(), "Cmd.Identity."+tc.field) {
			t.Errorf("%s: %v, want ErrInvalid naming Cmd.Identity.%s", name, err, tc.field)
		}
	}
	// Every code point Unicode names a noncharacter — read from Unicode's own
	// table, not from the pool's rule, so the case cannot share its mistake —
	// is refused, naming the code point.
	noncharacters := everyRune(unicode.Noncharacter_Code_Point)
	if len(noncharacters) != 66 {
		t.Fatalf("Unicode's table names %d noncharacters, want its fixed 66", len(noncharacters))
	}
	for _, r := range noncharacters {
		bad := suiteIdentity()
		bad.AuthorName = "Ada" + string(r)
		if err := run(gitproc.Cmd{Args: []string{"commit-tree", emptyTree}, Identity: &bad}); !errors.Is(err, gitproc.ErrInvalid) ||
			!strings.Contains(err.Error(), "Cmd.Identity.AuthorName") || !strings.Contains(err.Error(), fmt.Sprintf("noncharacter %U", r)) {
			t.Errorf("an author name ending in the noncharacter %U: %v, want ErrInvalid naming the field and %U", r, err, r)
		}
	}
	if got := len(calls()); got != before {
		t.Fatalf("%d git processes ran for refused identities", got-before)
	}

	fake := fakePool(t, "exec env", gitproc.Options{})
	epoch := gitproc.Identity{
		AuthorName: "Ada Author", AuthorEmail: "ada@example.com",
		CommitterName: "Crewlet", CommitterEmail: "engine@seats.invalid",
		AuthorWhen: time.Unix(0, 0).UTC(), CommitterWhen: time.Unix(0, 0).UTC(),
	}
	// A REBASED COMMIT'S AUTHOR DATE KEEPS ITS AUTHOR'S ZONE (U43 a): the
	// offset of the time it is given, written as git writes one.
	zoned := suiteIdentity()
	zoned.AuthorWhen = time.Unix(1700000000, 0).In(time.FixedZone("", -(5*3600 + 30*60)))
	for _, tc := range []struct {
		id                gitproc.Identity
		author, committer string
	}{
		{suiteIdentity(), "@1700000000 +0000", "@1700003600 +0000"},
		{epoch, "@0 +0000", "@0 +0000"},
		{zoned, "@1700000000 -0530", "@1700003600 +0000"},
	} {
		res, err := fake.Run(t.Context(), gitproc.Cmd{Args: []string{"commit-tree", emptyTree}, Identity: &tc.id,
			Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if err != nil {
			t.Fatalf("a commit-tree given %v: %v", tc.id, err)
		}
		env := envOf(res.Stdout)
		want := map[string]string{
			"GIT_AUTHOR_NAME": tc.id.AuthorName, "GIT_AUTHOR_EMAIL": tc.id.AuthorEmail, "GIT_AUTHOR_DATE": tc.author,
			"GIT_COMMITTER_NAME": tc.id.CommitterName, "GIT_COMMITTER_EMAIL": tc.id.CommitterEmail,
			"GIT_COMMITTER_DATE": tc.committer,
		}
		for name, value := range want {
			if got, ok := env[name]; !ok || got != value {
				t.Errorf("a commit-tree's %s = %q (present %v), want %q", name, got, ok, value)
			}
		}
		for name := range env {
			if _, six := want[name]; !six && identityVariable(name) {
				t.Errorf("a commit-tree sees %s beside the six variables of its identity", name)
			}
		}
	}
	res, err := fake.Run(t.Context(), gitproc.Cmd{Args: []string{"env"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		t.Fatal(err)
	}
	for name := range envOf(res.Stdout) {
		if identityVariable(name) {
			t.Errorf("a run that is no commit-tree sees %s", name)
		}
	}
}

// identityVariable reports whether git reads a commit's identity from the
// variable name: an author's or a committer's, or the address git falls back
// to (EMAIL).
func identityVariable(name string) bool {
	return strings.HasPrefix(name, "GIT_AUTHOR_") || strings.HasPrefix(name, "GIT_COMMITTER_") || name == "EMAIL"
}

// everyRune is every code point table names, in order: each range of its R16
// and its R32 walked at the range's stride.
func everyRune(table *unicode.RangeTable) []rune {
	var out []rune
	for _, r := range table.R16 {
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			out = append(out, c)
		}
	}
	for _, r := range table.R32 {
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			out = append(out, c)
		}
	}
	return out
}

// A COMMIT IS ITS IDENTITY AND NOTHING ELSE: a commit-tree given one identity
// in two scratches over one cache writes one commit, and its bytes are exactly
// the tree, the parent, the identity and the message the call states — while
// the engine's own environment names another identity, another date and an
// encoding that would add a header of its own, and the pool's HOME another
// identity and that encoding. Run on both of the node's gits: what reaches
// git from either is a claim about every git a git node may run.
//
// Not parallel, because the environment it plants is the process's
// (t.Setenv); its two arms run beside each other once it returns, before the
// environment is put back.
//
// Mutations: let the engine's GIT_AUTHOR_DATE through, or GIT_CONFIG_PARAMETERS,
// or read the HOME's configuration (no GIT_CONFIG_GLOBAL=/dev/null), and the
// commit is another.
func TestACommitIsItsIdentityAndNothingElse(t *testing.T) {
	for name, value := range map[string]string{
		"GIT_AUTHOR_NAME":       "Planted Author",
		"GIT_AUTHOR_EMAIL":      "planted@example.com",
		"GIT_AUTHOR_DATE":       "@1234567890 +0500",
		"GIT_COMMITTER_NAME":    "Planted Committer",
		"GIT_COMMITTER_EMAIL":   "planted@example.com",
		"GIT_COMMITTER_DATE":    "@1234567890 +0500",
		"EMAIL":                 "planted@example.com",
		"GIT_CONFIG_PARAMETERS": "'i18n.commitencoding'='ISO-8859-1' 'user.name'='Planted Parameter'",
	} {
		t.Setenv(name, value)
	}
	onNodeGits(t, commitIsItsIdentityOn)
}

// commitIsItsIdentityOn is TestACommitIsItsIdentityAndNothingElse on the git at
// git.
func commitIsItsIdentityOn(t *testing.T, git string) {
	home := filepath.Join(t.TempDir(), "home")
	p := poolOn(t, git, gitproc.Options{Home: home})
	// Written after New checked the HOME was empty, as anything on the node
	// could write it: a git that read it would name this identity, and add
	// the encoding header the configuration asks for whatever identity it
	// was given.
	planted := "[user]\n\tname = Planted Home\n\temail = home@example.com\n" +
		"[author]\n\tname = Planted Home\n[committer]\n\tname = Planted Home\n" +
		"[i18n]\n\tcommitEncoding = ISO-8859-1\n"
	for _, rel := range []string{".gitconfig", ".config/git/config"} {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(planted), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
	}

	c := gittest.NewClientWith(t, git)
	wt := c.Init(t)
	parent := c.Commit(t, wt, map[string]string{"a": "a\n"}, "one")
	tree := strings.TrimSpace(c.Run(t, wt, "rev-parse", parent+"^{tree}"))
	cache := gittest.Empty(t)
	gittest.Pack(t, c, cache, wt, parent)

	id := suiteIdentity()
	const message = "Add the file\n"
	body := "tree " + tree + "\nparent " + parent + "\n" +
		"author Ada Author <ada@example.com> 1700000000 +0000\n" +
		"committer Crewlet <engine@seats.invalid> 1700003600 +0000\n" +
		"\n" + message
	want := objectID("commit", body)
	for i := range 2 {
		scratch := scratchOver(t, cache)
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: scratch, Alternate: cache,
			Args: []string{"commit-tree", tree, "-p", parent}, Stdin: strings.NewReader(message), Identity: &id,
			Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if err != nil {
			t.Fatalf("commit-tree in scratch %d: %v\n%s", i, err, res.Stderr)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != want {
			t.Errorf("commit-tree in scratch %d wrote %s, want %s, the commit its call states:\n%s\nwhat it wrote:\n%s",
				i, got, want, body, catCommit(t, p, scratch, cache, got))
		}
	}
}

// THE POOL ADMITS AN IDENTITY PART EXACTLY WHERE GIT WRITES IT AS GIVEN. Every
// name and address of a corpus — plain, UTF-8, every mark git trims inside a
// part and at either end of one, a line feed, a < and a >, bytes that are not
// UTF-8, and Unicode's noncharacters, which are UTF-8 to Go and not to git,
// beside the code points next to them — is put both to the pool, as a
// commit-tree's identity, and to git itself, by a client whose environment
// names it: the pool admits a part exactly where git writes it into the
// commit byte for byte, and what the pool admits its commit-tree writes so.
// The pool's own two rules are held apart, each beside what git would do: an
// empty address, which git writes as <>, and a NUL, which no environment can
// carry. And every date the pool admits is written as its second — the epoch,
// a second and a day after it, and the last second of the year 9999 — which a
// date written without its @ would not be. Run on both of the node's gits,
// since what git writes as given is git's.
//
// Mutations: drop the rule for either end of a part, and " Ada" is admitted
// and written "Ada"; drop the noncharacter rule, and a name ending in U+FFFE
// is admitted and written as its bytes read as Latin-1, "Adaï¿¾"; write a date
// without its @, and the epoch is refused by git as a date it cannot read.
func TestThePoolAdmitsAnIdentityPartExactlyWhereGitWritesItAsGiven(t *testing.T) {
	t.Parallel()
	onNodeGits(t, identityPartsOn)
}

// identityPartsOn is TestThePoolAdmitsAnIdentityPartExactlyWhereGitWritesItAsGiven
// on the git at git.
func identityPartsOn(t *testing.T, git string) {
	p := poolOn(t, git, gitproc.Options{})
	c := gittest.NewClientWith(t, git)
	wt := c.Init(t)
	commit := c.Commit(t, wt, map[string]string{"a": "a\n"}, "one")
	tree := strings.TrimSpace(c.Run(t, wt, "rev-parse", commit+"^{tree}"))
	repo := gittest.Empty(t)
	gittest.Pack(t, c, repo, wt, commit)

	// poolWrites runs a commit-tree of the tree through the pool under id:
	// the commit it wrote, or admitted false where the pool refused id.
	poolWrites := func(id gitproc.Identity) (written string, admitted bool) {
		t.Helper()
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: repo, Args: []string{"commit-tree", tree},
			Stdin: strings.NewReader("m\n"), Identity: &id, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if errors.Is(err, gitproc.ErrInvalid) {
			return "", false
		}
		if err != nil {
			t.Fatalf("commit-tree under %+v: %v\n%s", id, err, res.Stderr)
		}
		return catCommit(t, p, repo, "", strings.TrimSpace(string(res.Stdout))), true
	}
	// field is one part of an identity: where the pool takes it, the
	// variable git reads it from, and which line of a commit carries it.
	type field struct {
		name, variable, who string
		email               bool
		set                 func(*gitproc.Identity, string)
	}
	fields := []field{
		{"AuthorName", "GIT_AUTHOR_NAME", "author", false, func(id *gitproc.Identity, s string) { id.AuthorName = s }},
		{"CommitterEmail", "GIT_COMMITTER_EMAIL", "committer", true, func(id *gitproc.Identity, s string) { id.CommitterEmail = s }},
	}
	// gitWrites is what git itself writes for s as f, given it by its
	// environment, and false where git refused it.
	gitWrites := func(f field, s string) (string, bool) {
		t.Helper()
		out, _, err := c.Env(f.variable+"="+s).Try(t, wt, strings.NewReader("m\n"), "commit-tree", tree)
		if err != nil {
			return "", false
		}
		return part(t, c.Run(t, wt, "cat-file", "commit", strings.TrimSpace(out)), f.who, f.email), true
	}

	for _, s := range []string{
		"Ada Lovelace", "Zoë Ångström", "名前", "O'Brien", `Jane "JJ" Doe`, "a, b", "Dr. Who.", "a\tb", "a\x7f",
		"ada@example.com", "a+b@example.com", "",
		" Ada", "Ada ", "\tAda", "Ada\r", "Ada,", ",Ada", ":Ada", "Ada;", `"Ada"`, `Ada\`, "'t Hooft", ",",
		"A\nda", "A<da", "Ad>a", "<ada@example.com>", "\xffAda", "Ada\xc3",
		// Unicode's noncharacters, which Go's UTF-8 admits and git's does
		// not — both ends of U+FDD0..U+FDEF, the last two code points of
		// plane 0, one of the last two of plane 1 and of plane 16, and one
		// inside a part — then the code points beside them, which git writes
		// as given.
		"Ada\U0000FDD0", "Ada\U0000FDEF", "Ada\U0000FFFE", "Ada\U0000FFFF", "Ada\U0001FFFE", "Ada\U0010FFFF",
		"A\U0000FFFFda",
		"Ada\U0000FDCF", "Ada\U0000FDF0", "Ada\U0000FFFD", "Ada\U0001FFFD", "Ada\U00010000",
	} {
		for _, f := range fields {
			id := suiteIdentity()
			f.set(&id, s)
			commit, admitted := poolWrites(id)
			byGit, wrote := gitWrites(f, s)
			if s == "" && f.email {
				// The pool's own rule: an address git would write as <>.
				if admitted || !wrote || byGit != "" {
					t.Errorf("an empty %s: the pool admits it %v, and git writes it %q (%v); want the pool to refuse "+
						"what git writes as <>", f.name, admitted, byGit, wrote)
				}
				continue
			}
			if asGiven := wrote && byGit == s; admitted != asGiven {
				t.Errorf("%s %q: the pool admits it %v, and git writes it as given %v (git wrote %q, written %v)",
					f.name, s, admitted, asGiven, byGit, wrote)
			}
			if admitted {
				if got := part(t, commit, f.who, f.email); got != s {
					t.Errorf("%s %q admitted by the pool and written %q", f.name, s, got)
				}
			}
		}
	}
	nul := suiteIdentity()
	nul.AuthorName = "Ada\x00Author"
	if _, admitted := poolWrites(nul); admitted {
		t.Error("an author name holding a NUL is admitted, and no process's environment can carry it")
	}

	for _, sec := range []int64{0, 1, 86400, 1700000000, 253402300799} {
		id := suiteIdentity()
		id.AuthorWhen, id.CommitterWhen = time.Unix(sec, 0).UTC(), time.Unix(sec, 0).UTC()
		commit, admitted := poolWrites(id)
		if !admitted {
			t.Errorf("the second %d is refused", sec)
			continue
		}
		want := strconv.FormatInt(sec, 10) + " +0000"
		for _, who := range []string{"author", "committer"} {
			if got := date(t, commit, who); got != want {
				t.Errorf("the second %d is written on the %s line as %q, want %q", sec, who, got, want)
			}
		}
	}
}

// objectID is git's name for an object of kind holding body.
func objectID(kind, body string) string {
	sum := sha1.Sum([]byte(kind + " " + strconv.Itoa(len(body)) + "\x00" + body)) //nolint:gosec // git's object name
	return hex.EncodeToString(sum[:])
}

// catCommit is the commit oid as the pool reads it in dir, leaning on
// alternate ("" for none), or why it could not be read.
func catCommit(t *testing.T, p *gitproc.Pool, dir, alternate, oid string) string {
	t.Helper()
	res, err := p.Run(t.Context(), gitproc.Cmd{Dir: dir, Alternate: alternate, Args: []string{"cat-file", "commit", oid},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		return "(unread: " + err.Error() + ")"
	}
	return string(res.Stdout)
}

// identityLine is the rest of a commit's line for who ("author" or
// "committer"), after its keyword: name <address> date zone.
func identityLine(t *testing.T, commit, who string) (name, email, when string) {
	t.Helper()
	headers, _, _ := strings.Cut(commit, "\n\n")
	for _, line := range strings.Split(headers, "\n") {
		rest, ok := strings.CutPrefix(line, who+" ")
		if !ok {
			continue
		}
		open, end := strings.IndexByte(rest, '<'), strings.IndexByte(rest, '>')
		if open < 1 || end < open || rest[open-1] != ' ' || len(rest) < end+2 {
			t.Fatalf("the %s line %q is no identity line", who, line)
		}
		return rest[:open-1], rest[open+1 : end], rest[end+2:]
	}
	t.Fatalf("no %s line in the commit %q", who, commit)
	return "", "", ""
}

// part is the name, or the address where email, on a commit's line for who.
func part(t *testing.T, commit, who string, email bool) string {
	t.Helper()
	name, address, _ := identityLine(t, commit, who)
	if email {
		return address
	}
	return name
}

// date is the date and zone on a commit's line for who.
func date(t *testing.T, commit, who string) string {
	t.Helper()
	_, _, when := identityLine(t, commit, who)
	return when
}
