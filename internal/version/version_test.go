package version

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// What a binary says it is, and what keeps that true.

// A BINARY WITH NO STAMP NAMES ITSELF HONESTLY rather than reporting an
// empty string. `go install` and a plain `go build` produce one, and a
// version field that renders empty reads as a broken page rather than as an
// unreleased build.
func TestAnUnstampedBuildStillReportsSomething(t *testing.T) {
	t.Parallel()
	// The suite itself is an unstamped build, which is exactly the case.
	if got := String(); got == "" {
		t.Error("an unstamped build reported no version at all")
	}
}

// THE STAMP WINS over whatever the toolchain recorded. It is the only thing
// the release build can set, and a build info that disagreed with it would
// be the module version rather than the tag.
func TestAStampedValueWins(t *testing.T) {
	t.Parallel()
	if got := resolve("v9.9.9", "v0.0.1"); got != "v9.9.9" {
		t.Errorf("version = %q, want the stamped value", got)
	}
}

// AN UNSTAMPED BUILD REPORTS WHAT THE TOOLCHAIN RECORDED, which is how a
// `go install`ed binary names itself honestly.
func TestNoStampFallsBackToBuildInfo(t *testing.T) {
	t.Parallel()
	if got := resolve("", "v1.4.0"); got != "v1.4.0" {
		t.Errorf("version = %q, want the recorded module version", got)
	}
}

// NEITHER SOURCE IS A REAL ANSWER, and the answer is still not an empty
// string: a version field that renders empty reads as a broken page rather
// than as a build from someone's checkout.
func TestNothingRecordedStillNamesTheBuild(t *testing.T) {
	t.Parallel()
	if got := resolve("", ""); got != "dev" {
		t.Errorf("version = %q, want dev", got)
	}
}

// --- the release configuration -------------------------------------------

// THE LDFLAGS STAMP RESOLVES TO A REAL VARIABLE IN THIS PACKAGE.
//
// Rename `value`, move this package, change the module path, and the -X flag
// silently applies to nothing: the link succeeds, the binary builds, and
// every release reports the build-info fallback instead of its tag.
//
// So this does not compare the flag against a literal — a literal is the same
// forgettable second copy the whole "the tag is the version" rule exists to
// avoid, and it stays green through exactly the renames it is meant to catch.
// It takes the target APART and checks both halves against the tree: the
// import path against the one this package actually has (the module path from
// go.mod plus where this directory sits under it), and the variable against a
// package-level string var that is really declared here. Both are what the
// linker resolves, so both moving is what breaks a release.
func TestTheReleaseConfigStampsThisVariable(t *testing.T) {
	t.Parallel()
	importPath, varName := stampTarget(t, releaseFile(t, ".goreleaser.yaml"))

	if want := thisPackagesImportPath(t); importPath != want {
		t.Errorf(".goreleaser.yaml stamps %q, but this package is %q — the "+
			"-X flag applies to nothing and every release reports the "+
			"unstamped fallback", importPath, want)
	}
	if !declaresStringVar(t, varName) {
		t.Errorf(".goreleaser.yaml stamps a variable named %q, which this "+
			"package does not declare as a string — the -X flag applies to "+
			"nothing and every release reports the unstamped fallback",
			varName)
	}
}

// stampTarget pulls the `-X <importpath>.<name>=<value>` ldflag out of the
// release config and splits it where the linker does.
//
// EVERY BUILD STAMPS, AND THEY ALL STAMP THE SAME THING. There is more than
// one build now — the glibc linux/darwin matrix and the musl linux one — and
// a second build id is exactly the shape that ships an UNSTAMPED binary: the
// ldflags block is per-build, so a copied entry that lost the -X line links
// fine and reports its module build info instead of the tag. Nothing else
// would notice, because the artifact that went out is the one nobody checked.
//
// So the count is compared against the number of builds rather than against
// 1, and the targets have to agree with each other.
//
// The split is on the first dot AFTER the last slash: an import path is full
// of dots (github.com), and only the final path element can carry the one that
// separates the package from the symbol.
func stampTarget(t *testing.T, config string) (importPath, varName string) {
	t.Helper()
	var flags []string
	for _, line := range strings.Split(config, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if rest, ok := strings.CutPrefix(trimmed, "-X "); ok {
			flags = append(flags, strings.TrimSpace(rest))
		}
	}
	if builds := countBuilds(t, config); len(flags) != builds {
		t.Fatalf("-X ldflags in .goreleaser.yaml = %v (%d), but there are %d "+
			"builds: every build must stamp the version, or the one that "+
			"does not ships a binary reporting its module build info "+
			"instead of the tag", flags, len(flags), builds)
	}
	for _, flag := range flags[1:] {
		if flag != flags[0] {
			t.Fatalf("the builds stamp different targets (%q and %q); one "+
				"of them is writing to a variable the other does not have",
				flags[0], flag)
		}
	}
	target, _, ok := strings.Cut(flags[0], "=")
	if !ok {
		t.Fatalf("the -X flag %q assigns nothing", flags[0])
	}
	slash := strings.LastIndex(target, "/")
	if slash < 0 {
		t.Fatalf("the -X target %q names no import path", target)
	}
	pkg, name, ok := strings.Cut(target[slash+1:], ".")
	if !ok || name == "" {
		t.Fatalf("the -X target %q names no variable", target)
	}
	return target[:slash+1] + pkg, name
}

// countBuilds reports how many entries the release config's `builds:` block
// has, by counting the `- id:` lines between it and the next top-level key.
//
// Line-scanned rather than YAML-parsed, like everything else in this file: the
// point of these guards is that they read the same bytes goreleaser does with
// no dependency of their own, so a malformed config fails here rather than
// somewhere a test had already normalised it.
func countBuilds(t *testing.T, config string) int {
	t.Helper()
	inBuilds, n := false, 0
	for _, line := range strings.Split(config, "\n") {
		switch {
		case line == "builds:":
			inBuilds = true
		case !inBuilds:
			continue
		case strings.HasPrefix(line, "  - id:"):
			n++
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			// A new top-level key ends the block.
			inBuilds = false
		}
	}
	if n == 0 {
		t.Fatal(".goreleaser.yaml declares no builds")
	}
	return n
}

// thisPackagesImportPath is what the linker would have to be given to reach
// this package: the module path go.mod declares, plus this directory's place
// under the module root. Derived rather than written down, so that moving the
// package moves the expectation with it.
func thisPackagesImportPath(t *testing.T) string {
	t.Helper()
	root, modulePath := moduleRoot(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("locating this package under %s: %v", root, err)
	}
	return path.Join(modulePath, filepath.ToSlash(rel))
}

// declaresStringVar reports whether this package declares name as a
// package-level string variable — the only shape `go tool link -X` can write
// to. A var of another type, or one that is not package-level, is not an
// error the link reports; it is a stamp that silently does nothing.
func declaresStringVar(t *testing.T, name string) bool {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || !slices.ContainsFunc(value.Names, func(id *ast.Ident) bool {
					return id.Name == name
				}) {
					continue
				}
				if isStringVar(value) {
					return true
				}
			}
		}
	}
	return false
}

// isStringVar reports whether a var spec declares a string — either written
// out, or inferred from a string literal. Those are the two forms the linker
// accepts: an uninitialized string variable, or one initialized to a constant
// string expression.
func isStringVar(spec *ast.ValueSpec) bool {
	if ident, ok := spec.Type.(*ast.Ident); ok {
		return ident.Name == "string"
	}
	if spec.Type != nil || len(spec.Values) == 0 {
		return false
	}
	return slices.ContainsFunc(spec.Values, func(expr ast.Expr) bool {
		lit, ok := expr.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING
	})
}

// THE MATRIX IS A PLAIN GOOS/GOARCH LOOP ONLY WHILE THE BUILD IS PURE GO.
//
// A dependency that needs cgo does not fail here — it fails on whichever
// cross target the release machine cannot build for, at tag time. Asserting
// the flag keeps the intent visible; `go build` with it set on every target
// is what proves the intent holds.
func TestTheReleaseBuildIsPureGo(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".goreleaser.yaml"); !strings.Contains(config, "CGO_ENABLED=0") {
		t.Error(".goreleaser.yaml does not set CGO_ENABLED=0")
	}
}

// THE IMAGE COPIES THE RIGHT ARCHITECTURE'S BINARY.
//
// goreleaser stages each platform's binary under its own TARGETPLATFORM
// directory, so a Dockerfile that copies a bare path builds nothing at all —
// and it fails inside the release, after every binary is already built.
func TestTheImageCopiesPerPlatform(t *testing.T) {
	t.Parallel()
	dockerfile := releaseFile(t, "Dockerfile")
	if !strings.Contains(dockerfile, "ARG TARGETPLATFORM") ||
		!strings.Contains(dockerfile, "${TARGETPLATFORM}/crewlet") {
		t.Error("the Dockerfile does not copy from ${TARGETPLATFORM}, so the " +
			"image build cannot find the binary goreleaser staged")
	}
}

// EVERY DEPENDENCY SURFACE IS WATCHED.
//
// CLAUDE.md's rule, asserted because nothing reports the omission: a
// manifest with no Dependabot entry produces no pull requests, which looks
// exactly like a manifest with nothing to update. The Go module went the
// length of the rewrite that way.
//
// The required set is READ OFF THE TREE rather than listed here, which is the
// only reading of "every" a test can hold to. A hardcoded list asserts the
// entries that already exist — it passes on the day a new manifest lands with
// no entry, which is the entire failure. So each probe below is a manifest
// this repository could grow, and finding one is what makes its ecosystem
// mandatory.
func TestEveryDependencySurfaceHasADependabotEntry(t *testing.T) {
	t.Parallel()
	watched := dependabotEcosystems(t)
	for _, surface := range []struct {
		ecosystem string
		manifests []string
	}{
		{"gomod", []string{"go.mod"}},
		{"docker", []string{"Dockerfile"}},
		{"docker-compose", []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}},
		{"npm", []string{"package.json"}},
		{"pip", []string{"pyproject.toml", "requirements.txt"}},
		{"cargo", []string{"Cargo.toml"}},
	} {
		found, ok := firstPresent(t, surface.manifests)
		if !ok || slices.Contains(watched, surface.ecosystem) {
			continue
		}
		t.Errorf("%s is a %s surface with no dependabot.yml entry, so it is "+
			"watched by nothing", found, surface.ecosystem)
	}
	// The actions are their own surface, and they are not one manifest:
	// Dependabot reads every workflow under .github/workflows.
	if len(workflowFiles(t)) > 0 && !slices.Contains(watched, "github-actions") {
		t.Error(".github/workflows has workflows but dependabot.yml has no " +
			"github-actions entry, so every action they pin is watched by " +
			"nothing")
	}
	// The directory is not asserted: every surface here is at the
	// repository root now, so naming "/" would only restate what the
	// entries already say and would break the day one of them legitimately
	// moves. What must not silently vanish is the ENTRY.
}

// dependabotEcosystems is the set of ecosystems the config actually watches.
//
// The values are compared WHOLE. Matching them as substrings is what let
// `docker` be satisfied by the `docker-compose` entry — two ecosystems reading
// two different manifests, one of which could then be deleted with the test
// still green.
func dependabotEcosystems(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(releaseFile(t, ".github/dependabot.yml"), "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if rest, ok := strings.CutPrefix(trimmed, "package-ecosystem:"); ok {
			found = append(found, strings.Trim(strings.TrimSpace(rest), `"'`))
		}
	}
	return found
}

// firstPresent returns the first of these repository-root files that exists.
func firstPresent(t *testing.T, names []string) (string, bool) {
	t.Helper()
	root, _ := moduleRoot(t)
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return name, true
		}
	}
	return "", false
}

// releaseFile reads a repository-root file, found by walking up rather than
// by counting "../.." — these assertions are about a package that can move,
// and a relative hop that has to be edited alongside the move is one more way
// for them to go quiet.
func releaseFile(t *testing.T, name string) string {
	t.Helper()
	root, _ := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}

// moduleRoot walks up from this package until it finds the go.mod that owns
// it, and returns that directory with the module path it declares.
func moduleRoot(t *testing.T) (dir, modulePath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return dir, strings.TrimSpace(rest)
				}
			}
			t.Fatalf("%s declares no module path", filepath.Join(dir, "go.mod"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = parent
	}
}

// workflowFiles is every workflow GitHub would run.
//
// BOTH suffixes. GitHub reads .yml and .yaml alike, so a check that globs one
// of them is blind to half the directory — and blind in the direction that
// matters, since the file nobody remembered to name consistently is exactly
// the one an assertion is looking for.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	root, _ := moduleRoot(t)
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matched, err := filepath.Glob(filepath.Join(root, ".github", "workflows", pattern))
		if err != nil {
			t.Fatalf("globbing the workflows: %v", err)
		}
		paths = append(paths, matched...)
	}
	slices.Sort(paths)
	return paths
}

// THE GENERATED NOTES CANNOT SILENTLY DROP A PULL REQUEST.
//
// GitHub writes each release body from the pull requests merged since the
// previous tag, and .github/release.yml sorts that list into categories. A
// pull request matching NO category is omitted — not flagged, not appended,
// omitted — so without a catch-all the notes are missing whatever nobody
// labelled. Those notes are the only record a release has, and the omission
// is discoverable only by reading a release and noticing an absence.
func TestTheGeneratedNotesHaveACatchAllCategory(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".github/release.yml"); !strings.Contains(config, `- "*"`) {
		t.Error(".github/release.yml has no catch-all category, so a pull " +
			"request with no labels is dropped from the release notes")
	}
}

// GITHUB WRITES THE NOTES, NOT GORELEASER.
//
// goreleaser's default changelog is assembled from commit SUBJECTS. Ours
// must come from pull request TITLES, because a pull request squash-merges
// into one commit and the title is what CONTRIBUTING.md asks contributors to
// write as a release note. Flip this to any other mode and .github/release.yml
// becomes a dead file: the categories keep being maintained and stop
// appearing anywhere.
func TestTheReleaseNotesComeFromGitHub(t *testing.T) {
	t.Parallel()
	if config := releaseFile(t, ".goreleaser.yaml"); !strings.Contains(config, "use: github-native") {
		t.Error(".goreleaser.yaml does not use github-native release notes, " +
			"so the body would be built from commit subjects and " +
			".github/release.yml would shape nothing")
	}
}

// A PRE-RELEASE NEVER BECOMES THE DEFAULT DOWNLOAD.
//
// Two surfaces hand an unpinned user whatever the newest release is: GitHub's
// "Latest" badge and the image's `latest` tag. A release candidate taking
// either one ships it to everyone who pinned nothing, and both are set at
// release time from this file alone.
func TestAPreReleaseTakesNeitherLatest(t *testing.T) {
	t.Parallel()
	config := releaseFile(t, ".goreleaser.yaml")
	if !strings.Contains(config, "prerelease: auto") {
		t.Error(".goreleaser.yaml does not flag pre-releases, so an rc tag " +
			"would become GitHub's Latest release")
	}
	if !strings.Contains(config, "{{ if not .Prerelease }}latest{{ end }}") {
		t.Error(".goreleaser.yaml does not guard the `latest` image tag on " +
			"the release being stable, so `docker pull crewlet` would serve " +
			"a pre-release")
	}
}

// EXACTLY ONE WORKFLOW PUBLISHES A TAG.
//
// Two workflows triggered by `v*` both try to create one GitHub Release for
// one tag, and the loser fails the run after its artifacts are already built.
// That is how the rewrite's pipeline and the one it replaced coexisted, and
// the whole reason the tag trigger had to move in the same commit that
// deleted the old workflow.
func TestOnlyOneWorkflowIsTriggeredByAVersionTag(t *testing.T) {
	t.Parallel()
	var triggered []string
	for _, path := range workflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "tags:") && strings.Contains(trimmed, `"v*"`) {
				triggered = append(triggered, filepath.Base(path))
				break
			}
		}
	}
	if len(triggered) != 1 {
		t.Errorf("workflows triggered by a v* tag = %v, want exactly one — "+
			"two of them race to create one GitHub Release", triggered)
	}
}

// THE DOCS REBUILD WATCHES THE RELEASE WORKFLOW BY ITS EXACT NAME.
//
// A release changes which versions docs.crewlet.ai publishes, and nothing in
// a tag push touches docs/, so .github/workflows/docs-publish.yml watches the
// release workflow finishing instead. `workflow_run.workflows` matches the
// watched workflow's `name:` STRING. GitHub does not report a name that
// matches nothing — there is no such workflow to complain about — so a rename
// on either side, or a difference in case, just means the trigger silently
// never fires again and the site quietly falls back to the hourly poll the
// trigger exists to pre-empt. Two strings in two files with nothing joining
// them is exactly the shape this package exists to assert.
//
// This asserts the PAIR; the sweep below asserts the rule. Neither covers the
// other: a docs-publish.yml that watches nothing at all passes the sweep with
// nothing to check, and is the same outage by a different route.
func TestTheDocsRebuildWatchesTheReleaseWorkflowByItsName(t *testing.T) {
	t.Parallel()
	release := workflowName(t, "release.yml")
	watched := watchedWorkflows(t, "docs-publish.yml")
	if !slices.Contains(watched, release) {
		t.Errorf("docs-publish.yml watches %v, but the release workflow is "+
			"named %q — the workflow_run trigger matches nothing and the "+
			"docs site never learns a release happened", watched, release)
	}
}

// EVERY WATCHED WORKFLOW NAME IS A WORKFLOW THAT EXISTS.
//
// The pair above is the one that broke; this is the rule it broke, held over
// every watcher in the directory. A name in a `workflows:` list that matches
// nothing is not an error, a warning or a skipped run — the workflow simply
// never starts, which looks exactly like a workflow with nothing to do. The
// next `workflow_run` trigger anyone adds inherits the guard without having
// to remember it.
func TestEveryWatchedWorkflowNameExists(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	watched := map[string]string{} // watched name -> the file watching it
	for _, path := range workflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		body := string(raw)
		names[declaredName(body)] = true
		for _, name := range watchedIn(body) {
			watched[name] = filepath.Base(path)
		}
	}
	if len(watched) == 0 {
		t.Fatal("no workflow watches another one, so this test is checking nothing")
	}
	for name, watcher := range watched {
		if !names[name] {
			t.Errorf("%s watches a workflow called %q and no workflow is "+
				"named that — the trigger never fires, and nothing reports it",
				watcher, name)
		}
	}
}

// workflowName is the `name:` a workflow declares, and fails the test if it
// declares none — a caller asking for one workflow by file has nothing to
// compare against without it.
func workflowName(t *testing.T, file string) string {
	t.Helper()
	name := declaredName(releaseFile(t, filepath.Join(".github", "workflows", file)))
	if name == "" {
		t.Fatalf("%s declares no top-level name", file)
	}
	return name
}

// declaredName is a workflow's own `name:` — the top-level one, which is the
// only unindented key of that name in the file, and the reason this matches
// on the raw line rather than a trimmed one: every step in the file has a
// `name:` too, and they are all indented.
func declaredName(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// watchedWorkflows is the inline `workflows: [a, b]` list of a workflow_run
// trigger, and fails the test if the file has none — "watches nothing" is not
// an answer a caller naming one file can act on.
func watchedWorkflows(t *testing.T, file string) []string {
	t.Helper()
	names := watchedIn(releaseFile(t, filepath.Join(".github", "workflows", file)))
	if len(names) == 0 {
		t.Fatalf("%s has no workflow_run workflows list", file)
	}
	return names
}

// watchedIn is every name in every inline `workflows: [a, b]` list in one
// workflow.
//
// The key has to START the trimmed line. That is what keeps prose out: a
// comment carries its `#` through the trim, and docs-publish.yml explains
// this very trigger in comments that name it — an assertion that read them
// would be checking an explanation rather than the trigger it explains.
func watchedIn(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "workflows:")
		if !ok {
			continue
		}
		for _, name := range strings.Split(strings.Trim(strings.TrimSpace(rest), "[]"), ",") {
			if name = strings.Trim(strings.TrimSpace(name), `"'`); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// --- the dependency auto-merge -------------------------------------------

// A BUMP IS APPROVED ONLY WHILE BOTH HALVES OF THE GUARD HOLD.
//
// .github/workflows/dependabot-merge.yml approves a pull request and queues
// it to merge with no person in the loop, so what it refuses to do that for
// is the whole of its safety. Checking the AUTHOR is what limits it to a
// bump. Checking the ACTOR is what stops it approving a commit a PERSON
// pushed onto a Dependabot branch — anyone with write access can push one,
// and dropping that half turns the workflow into a way to have your own
// change approved by the repository itself. Neither omission has a symptom:
// the workflow keeps running, and it runs on the wrong pull requests.
func TestTheAutoMergeGuardChecksBothAuthorAndActor(t *testing.T) {
	t.Parallel()
	workflow := releaseFile(t, ".github/workflows/dependabot-merge.yml")
	for _, want := range []string{
		"github.event.pull_request.user.login == 'dependabot[bot]'",
		"github.actor == 'dependabot[bot]'",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("dependabot-merge.yml does not gate on %s, so it would "+
				"approve and merge a pull request Dependabot did not write",
				want)
		}
	}
}

// THE UNATTENDED MERGE WAITS FOR CI, AND LANDS AS ONE COMMIT.
//
// `--auto` is what makes merging without a reviewer safe: it queues the pull
// request behind the checks `main`'s protection rule requires rather than
// merging it on the spot. Losing that one word is a silent change of meaning
// — the step still succeeds and the bump still lands, only now before
// anything has reported on it. `--squash` is the other half: a pull request
// becomes ONE commit whose subject is its title, which is what CONTRIBUTING.md
// asks a title to be written for and what the generated release notes carry.
func TestTheAutoMergeQueuesBehindTheRequiredChecks(t *testing.T) {
	t.Parallel()
	workflow := releaseFile(t, ".github/workflows/dependabot-merge.yml")
	if !strings.Contains(workflow, "gh pr merge --auto --squash") {
		t.Error("dependabot-merge.yml does not run `gh pr merge --auto " +
			"--squash`, so a bump would merge without waiting for CI, or " +
			"would land as something other than the one commit its title " +
			"describes")
	}
}
