package gitproc_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE ENVIRONMENT IS BUILT, NOT INHERITED: every variable git reads that the
// engine's own environment could carry — its secrets, a trace, an exec path,
// an alternate object directory, a config of its own, a tree to read
// attributes from, a commit's identity — is absent, and what is present is
// exactly the pool's table, its base configuration first. The trace2 event
// stream is the pool's own (fd 3, brief, nesting 1), whatever the engine's
// environment names.
func TestTheEnvironmentIsBuiltNotInherited(t *testing.T) {
	for name, value := range map[string]string{
		"CREWLET_SECRET_X":                 "leaked",
		"GIT_AUTHOR_NAME":                  "Planted Author",
		"GIT_AUTHOR_EMAIL":                 "planted@example.com",
		"GIT_AUTHOR_DATE":                  "@1234567890 +0500",
		"GIT_COMMITTER_NAME":               "Planted Committer",
		"GIT_COMMITTER_EMAIL":              "planted@example.com",
		"GIT_COMMITTER_DATE":               "@1234567890 +0500",
		"EMAIL":                            "planted@example.com",
		"GIT_CONFIG_PARAMETERS":            "'core.hooksPath'='/tmp'",
		"GIT_EXEC_PATH":                    "/nonexistent",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/tmp",
		"GIT_SSH_COMMAND":                  "x",
		"GIT_TRACE":                        "1",
		"XDG_CONFIG_HOME":                  "/tmp",
		"GIT_CONFIG_SYSTEM":                "/tmp/x",
		"GIT_ATTR_SOURCE":                  "HEAD",
		"GIT_ATTR_NOSYSTEM":                "0",
		"GIT_TRACE2":                       "/tmp/normal",
		"GIT_TRACE2_EVENT":                 "/tmp/event",
		"GIT_TRACE2_EVENT_NESTING":         "9",
		"GIT_TRACE2_CONFIG_PARAMS":         "core.*",
		"GIT_TRACE2_ENV_VARS":              "CREWLET_SECRET_X",
	} {
		t.Setenv(name, value)
	}
	p := fakePool(t, "exec env", gitproc.Options{})
	bare := gittest.Empty(t)
	res, err := p.Run(t.Context(), gitproc.Cmd{
		Dir: bare, Args: []string{"env"}, Protocol: "version=2",
		Env:    []gitproc.KV{{Key: gitproc.EnvHookToken, Value: "t"}},
		Config: []gitproc.KV{{Key: "receive.unpackLimit", Value: "1"}},
		Class:  gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(res.Stdout)
	want := []string{
		"PATH", "HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_ATTR_NOSYSTEM", "GIT_TERMINAL_PROMPT",
		"GIT_NO_REPLACE_OBJECTS", "GIT_GRAFT_FILE", "GIT_SHALLOW_FILE", "LC_ALL", "GIT_DIR",
		"GIT_PROTOCOL", "GIT_CONFIG_COUNT", gitproc.EnvHookToken,
		"GIT_TRACE2_EVENT", "GIT_TRACE2_EVENT_BRIEF", "GIT_TRACE2_EVENT_NESTING",
	}
	for i := range 7 {
		want = append(want, "GIT_CONFIG_KEY_"+string(rune('0'+i)), "GIT_CONFIG_VALUE_"+string(rune('0'+i)))
	}
	slices.Sort(want)
	if got := slices.Sorted(maps.Keys(env)); !slices.Equal(got, want) {
		t.Fatalf("the process saw\n  %v\nwant exactly\n  %v", got, want)
	}
	for name, value := range map[string]string{
		"PATH":                     os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM":      "1",
		"GIT_CONFIG_GLOBAL":        "/dev/null",
		"GIT_ATTR_NOSYSTEM":        "1",
		"GIT_TERMINAL_PROMPT":      "0",
		"GIT_NO_REPLACE_OBJECTS":   "1",
		"GIT_GRAFT_FILE":           "/dev/null/grafts",
		"GIT_SHALLOW_FILE":         "/dev/null/shallow",
		"LC_ALL":                   "C",
		"GIT_DIR":                  bare,
		"GIT_PROTOCOL":             "version=2",
		gitproc.EnvHookToken:       "t",
		"GIT_TRACE2_EVENT":         "3",
		"GIT_TRACE2_EVENT_BRIEF":   "1",
		"GIT_TRACE2_EVENT_NESTING": "1",
	} {
		if env[name] != value {
			t.Errorf("%s = %q, want %q", name, env[name], value)
		}
	}
	if home := env["HOME"]; home == "" || !filepath.IsAbs(home) {
		t.Errorf("HOME = %q, want the pool's own", home)
	}
	wantConfig := []gitproc.KV{
		{Key: "protocol.allow", Value: "never"},
		{Key: "gc.auto", Value: "0"},
		{Key: "maintenance.auto", Value: "false"},
		{Key: "pack.threads", Value: "1"},
		{Key: "core.hooksPath", Value: "/dev/null/no-hooks"},
		{Key: "core.attributesFile", Value: "/dev/null/no-attributes"},
		{Key: "receive.unpackLimit", Value: "1"},
	}
	if got := configOf(t, env); !slices.Equal(got, wantConfig) {
		t.Errorf("configuration %v, want %v", got, wantConfig)
	}

	res, err = p.Run(t.Context(), gitproc.Cmd{Args: []string{"env"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		t.Fatal(err)
	}
	env = envOf(res.Stdout)
	if _, ok := env["GIT_DIR"]; ok {
		t.Errorf("a run with no repository carries GIT_DIR %q", env["GIT_DIR"])
	}
	if env["GIT_CEILING_DIRECTORIES"] != filepath.Dir(env["HOME"]) {
		t.Errorf("GIT_CEILING_DIRECTORIES = %q, want HOME's parent %q", env["GIT_CEILING_DIRECTORIES"], filepath.Dir(env["HOME"]))
	}
}

// A CLIENT'S GIT_PROTOCOL IS PASSED ONLY WHEN IT IS ONE: anything else is
// dropped and the run is v0, which every client speaks.
func TestAClientProtocolIsPassedOnlyWhenWellFormed(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "exec env", gitproc.Options{})
	for value, passed := range map[string]bool{
		"version=2":                    true,
		"version=2:object-format=sha1": true,
		"version=2\nX=1":               false,
		"--upload-pack=x":              false,
		"version=two":                  false,
	} {
		res, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"env"}, Protocol: value, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if err != nil {
			t.Fatalf("protocol %q: %v", value, err)
		}
		got, ok := envOf(res.Stdout)["GIT_PROTOCOL"]
		if ok != passed || (passed && got != value) {
			t.Errorf("protocol %q: GIT_PROTOCOL %q (present %v), want present %v", value, got, ok, passed)
		}
	}
}

// THE HOME CAN NEVER CONFIGURE A GIT PROCESS — even when something writes into
// it after the pool checked it was empty — and neither can the HOST: no
// configuration and no ATTRIBUTES are read from either. The attributes are the
// half GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL do not reach: git reads
// $HOME/.config/git/attributes unless core.attributesFile names another file,
// and /etc/gitattributes unless GIT_ATTR_NOSYSTEM is set, and pack-objects
// asks a path's `delta` attribute whenever it searches for a delta — so a
// `* -delta` in either would have every delta a compaction recomputes, and
// every object stored whole, written undeltified (the deltas a clone reuses
// it leaves alone; the package doc has the measurement).
func TestTheHomeCanNeverConfigureAGitProcess(t *testing.T) {
	t.Parallel()
	onNodeGits(t, homeCannotConfigureOn)
}

// homeCannotConfigureOn is TestTheHomeCanNeverConfigureAGitProcess on the git at git.
func homeCannotConfigureOn(t *testing.T, git string) {
	home := filepath.Join(t.TempDir(), "home")
	p := poolOn(t, git, gitproc.Options{Home: home})
	hooks := t.TempDir()
	writeHook(t, filepath.Join(hooks, "pre-receive"), "exit 7")
	planted := "[uploadpack]\n\tallowAnySHA1InWant = true\n[core]\n\thooksPath = " + hooks + "\n"
	for rel, body := range map[string]string{
		".gitconfig":             planted,
		".config/git/config":     planted,
		".config/git/attributes": "* -delta\n",
	} {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
	}

	// An unreachable commit: honoured, allowAnySHA1InWant would send it.
	c := gittest.NewClient(t)
	wt := c.Init(t)
	main := c.Commit(t, wt, map[string]string{"a": "a\n"}, "main")
	c.Run(t, wt, "checkout", "-q", "-b", "side")
	side := c.Commit(t, wt, map[string]string{"b": "b\n"}, "side")
	bare := gittest.Empty(t)
	gittest.Pack(t, c, bare, wt, side)
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": main})
	_, err := p.Run(t.Context(), gitproc.Cmd{
		Dir: bare, Args: []string{"upload-pack", "--stateless-rpc", bare},
		Stdin: strings.NewReader(pkt("want "+side+"\n") + "0000" + pkt("done\n")),
		Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.ReadDeadline,
	})
	if !errors.Is(err, gitproc.ErrNotOurRef) {
		t.Fatalf("a v0 want of an unreachable commit = %v, want ErrNotOurRef", err)
	}

	res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"hook", "run", "--ignore-missing", "pre-receive"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || res.Exit != 0 {
		t.Fatalf("hook run with a planted hooksPath = exit %d, %v; want no hook run", res.Exit, err)
	}

	// The planted attributes file answers nothing, and the path that
	// replaces it — one that can never exist — costs no warning either.
	res, err = p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"check-attr", "delta", "--", "x"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || string(res.Stdout) != "x: delta: unspecified\n" || res.Stderr != "" {
		t.Fatalf("check-attr delta with a planted HOME attributes file = %q, stderr %q, %v; want unspecified and silent",
			res.Stdout, res.Stderr, err)
	}

	// The HOST's attributes file cannot be planted from a test, so git is
	// asked: `git var GIT_ATTR_SYSTEM` names the system file only while it
	// is read, and answers 1 otherwise. GIT_ATTR_GLOBAL names the global
	// file git reads, which must be the base's path that cannot exist.
	res, err = p.Run(t.Context(), gitproc.Cmd{Args: []string{"var", "GIT_ATTR_SYSTEM"}, Answers: []int{1},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || res.Exit != 1 {
		t.Errorf("git var GIT_ATTR_SYSTEM = exit %d %q, %v: the pool's git reads the host's attributes file", res.Exit, res.Stdout, err)
	}
	res, err = p.Run(t.Context(), gitproc.Cmd{Args: []string{"var", "GIT_ATTR_GLOBAL"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "/dev/null/no-attributes" {
		t.Errorf("git var GIT_ATTR_GLOBAL = exit %d %q, %v: want the base's /dev/null/no-attributes", res.Exit, res.Stdout, err)
	}
}

// A REPOSITORY CANNOT CONFIGURE ITS OWN PROCESS: a config that is not the one
// the engine writes is refused before any git runs.
func TestARepositoryCanNeverConfigureAGitProcess(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	for name, config := range map[string]*string{
		"a planted key":                ptr(gitproc.RepoConfig + "[uploadpack]\n\tallowAnySHA1InWant = true\n"),
		"a config missing its newline": ptr(strings.TrimSuffix(gitproc.RepoConfig, "\n")),
		"no config at all":             nil,
	} {
		bare := gittest.Empty(t)
		if config == nil {
			if err := os.Remove(filepath.Join(bare, "config")); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(bare, "config"), []byte(*config), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
		_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if !errors.Is(err, gitproc.ErrRepoShape) || !strings.Contains(err.Error(), bare) {
			t.Errorf("%s: %v, want ErrRepoShape naming %s", name, err, bare)
		}
		for _, call := range calls() {
			if call.GitDir == bare {
				t.Errorf("%s: git ran in the refused repository: %v", name, call.Args)
			}
		}
	}
}

// A FILE THAT COULD NOT BE READ IS NOT A REPOSITORY OF THE WRONG SHAPE.
// ErrRepoShape is a definite answer — the config absent or holding other
// bytes, a commondir present, an alternate that is not the engine's — and a
// read that failed (a busy node out of descriptors, an I/O error) answers
// nothing about the shape: it is the read's own error, wrapped, so neither a
// caller nor the operator reading it is told a healthy repository was
// tampered with.
func TestAnUnreadableRepositoryIsNotCalledMisshapen(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	// Each spoils bare and answers the alternate the call names, if any.
	for name, spoil := range map[string]func(t *testing.T, bare string) string{
		"its config": func(t *testing.T, bare string) string {
			replaceWithDir(t, filepath.Join(bare, "config"))
			return ""
		},
		"its alternates": func(t *testing.T, bare string) string {
			replaceWithDir(t, filepath.Join(bare, "objects", "info", "alternates"))
			return ""
		},
		"its alternate's config": func(t *testing.T, bare string) string {
			c := gittest.Empty(t)
			replaceWithDir(t, filepath.Join(c, "config"))
			if err := os.MkdirAll(filepath.Join(bare, "objects", "info"), hostbox.DirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bare, "objects", "info", "alternates"), []byte(c+"/objects\n"), hostbox.FileMode); err != nil {
				t.Fatal(err)
			}
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			bare := gittest.Empty(t)
			alternate := spoil(t, bare)
			_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Alternate: alternate, Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
			if err == nil || errors.Is(err, gitproc.ErrRepoShape) || !errors.Is(err, syscall.EISDIR) {
				t.Fatalf("%v, want the read's own error (EISDIR) and not ErrRepoShape", err)
			}
			for _, call := range calls() {
				if call.GitDir == bare {
					t.Fatalf("git ran in a repository that could not be read: %v", call.Args)
				}
			}
		})
	}
}

// replaceWithDir puts an empty directory where path's file was, so reading
// it fails with EISDIR — a read that fails whoever runs it, root included,
// where a mode of 0000 is no refusal at all.
func replaceWithDir(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, hostbox.DirMode); err != nil {
		t.Fatal(err)
	}
}

// A COMMONDIR IS REFUSED: git would read another repository's objects and
// refs through it, and GIT_COMMON_DIR does not override it.
func TestACommondirIsRefused(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	bare, other := gittest.Empty(t), gittest.Empty(t)
	if err := os.WriteFile(filepath.Join(bare, "commondir"), []byte(other+"\n"), hostbox.FileMode); err != nil {
		t.Fatal(err)
	}
	_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "main"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if !errors.Is(err, gitproc.ErrRepoShape) || !strings.Contains(err.Error(), "commondir") {
		t.Fatalf("a repository with a commondir = %v, want ErrRepoShape naming it", err)
	}
	for _, call := range calls() {
		if call.GitDir == bare {
			t.Fatalf("git ran in the refused repository: %v", call.Args)
		}
	}
}

// AN ALTERNATE IS THE CALL'S, AND HELD TO THE SAME SHAPE, one hop further than
// the process's own repository, because git lists an alternate's refs with a
// git it configures with nothing of the pool's. A repository does not choose
// its alternate: an alternates file the call did not name is refused, so a
// cache — which never has one — cannot be made to serve another repository's
// objects by id through a file planted in it. Every refusal says which rule
// it is and which repository broke it.
func TestAnAlternateIsTheCallsAndHeldToTheSameShape(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, dir, alternate string) error {
		t.Helper()
		_, err := p.Run(t.Context(), gitproc.Cmd{Dir: dir, Alternate: alternate, Args: []string{"rev-parse", "--git-dir"},
			Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		return err
	}
	ranIn := func(dir string) bool {
		for _, call := range calls() {
			if call.GitDir == dir {
				return true
			}
		}
		return false
	}
	// refused fails t unless err refuses under rule, naming dir — the
	// alternate when inAlternate — and git never ran in s.
	refused := func(t *testing.T, err error, s string, rule gitproc.ShapeRule, dir string, inAlternate bool) {
		t.Helper()
		var shape *gitproc.ShapeError
		if !errors.As(err, &shape) || !errors.Is(err, gitproc.ErrRepoShape) {
			t.Fatalf("%v, want a *ShapeError, ErrRepoShape", err)
		}
		if shape.Rule != rule || shape.InAlternate != inAlternate || !strings.Contains(err.Error(), dir) {
			t.Fatalf("refused under %s (in the alternate %v): %v; want %s (in the alternate %v) naming %s", shape.Rule,
				shape.InAlternate, err, rule, inAlternate, dir)
		}
		if ranIn(s) {
			t.Fatal("git ran in the refused repository")
		}
	}
	alternates := func(s string) string { return filepath.Join(s, "objects", "info", "alternates") }

	t.Run("the call's one cache", func(t *testing.T) {
		s, c := gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(s), c+"/objects\n")
		if err := run(t, s, c); err != nil || !ranIn(s) {
			t.Fatalf("a scratch reading one cache: %v (ran %v)", err, ranIn(s))
		}
	})
	t.Run("an alternates file the call does not name", func(t *testing.T) {
		// What a cache planted with an alternates file is to every process
		// run in it: a well-formed line naming a repository of the engine's
		// own shape — another repository's cache — refused all the same.
		c, other := gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(c), other+"/objects\n")
		refused(t, run(t, c, ""), c, gitproc.ShapeAlternates, c, false)
	})
	t.Run("no alternates file where the call names one", func(t *testing.T) {
		s, c := gittest.Empty(t), gittest.Empty(t)
		refused(t, run(t, s, c), s, gitproc.ShapeAlternates, s, false)
	})
	t.Run("an alternates file naming another than the call's", func(t *testing.T) {
		s, c, other := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(s), other+"/objects\n")
		refused(t, run(t, s, c), s, gitproc.ShapeAlternates, s, false)
	})
	t.Run("two caches", func(t *testing.T) {
		s, c1, c2 := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(s), c1+"/objects\n"+c2+"/objects\n")
		refused(t, run(t, s, c1), s, gitproc.ShapeAlternates, s, false)
	})
	for name, tc := range map[string]struct {
		rule  gitproc.ShapeRule
		spoil func(t *testing.T, c string)
	}{
		"its config planted": {gitproc.ShapeConfig, func(t *testing.T, c string) {
			write(t, filepath.Join(c, "config"), gitproc.RepoConfig+"[uploadpack]\n\tallowAnySHA1InWant = true\n")
		}},
		"a commondir": {gitproc.ShapeCommondir, func(t *testing.T, c string) {
			write(t, filepath.Join(c, "commondir"), "/elsewhere\n")
		}},
		"an alternate of its own": {gitproc.ShapeAlternates, func(t *testing.T, c string) {
			write(t, alternates(c), gittest.Empty(t)+"/objects\n")
		}},
	} {
		t.Run("the cache with "+name, func(t *testing.T) {
			s, c := gittest.Empty(t), gittest.Empty(t)
			write(t, alternates(s), c+"/objects\n")
			tc.spoil(t, c)
			refused(t, run(t, s, c), s, tc.rule, c, true)
		})
	}
	// GIT LISTS AN ALTERNATE'S REFS AT ITS REAL PATH — the line resolved,
	// "/objects" stripped — so an alternate whose objects directory is a link
	// would be configured by the repository the link leads to. Its objects
	// directory must be its own, which makes the alternate checked the one git
	// runs in: a link is refused wherever it leads, a repository of the
	// engine's own shape included.
	link := func(t *testing.T, c, target string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(c, "objects")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(c, "objects")); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]func(t *testing.T) string{
		"a planted repository": func(t *testing.T) string {
			other := gittest.Empty(t)
			write(t, filepath.Join(other, "config"), gitproc.RepoConfig+"[uploadpack]\n\tallowAnySHA1InWant = true\n")
			return filepath.Join(other, "objects")
		},
		"a repository of the engine's shape": func(t *testing.T) string {
			return filepath.Join(gittest.Empty(t), "objects")
		},
		"a directory not named objects": func(t *testing.T) string {
			other := gittest.Empty(t)
			if err := os.Rename(filepath.Join(other, "objects"), filepath.Join(other, "store")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(other, "store")
		},
		"nowhere": func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone", "objects") },
	} {
		t.Run("the cache's objects a link to "+name, func(t *testing.T) {
			s, c := gittest.Empty(t), gittest.Empty(t)
			link(t, c, target(t))
			write(t, alternates(s), c+"/objects\n")
			refused(t, run(t, s, c), s, gitproc.ShapeObjects, c, true)
		})
	}
	for name, line := range map[string]func(c string) string{
		"a relative path":    func(string) string { return "objects\n" },
		"an unclean path":    func(c string) string { return c + "/objects/\n" },
		"not an objects dir": func(c string) string { return c + "\n" },
		"a comment":          func(c string) string { return "#" + c + "/objects\n" },
		"a quoted path":      func(c string) string { return `"` + c + `/objects"` + "\n" },
		"no final newline":   func(c string) string { return c + "/objects" },
		"an empty file":      func(string) string { return "" },
		"an empty line":      func(c string) string { return c + "/objects\n\n" },
	} {
		t.Run(name, func(t *testing.T) {
			s, c := gittest.Empty(t), gittest.Empty(t)
			write(t, alternates(s), line(c))
			refused(t, run(t, s, c), s, gitproc.ShapeAlternates, s, false)
		})
	}
	t.Run("an alternate the call cannot name", func(t *testing.T) {
		s := gittest.Empty(t)
		for name, cmd := range map[string]gitproc.Cmd{
			"a relative path": {Dir: s, Alternate: "cache.git"},
			"an unclean path": {Dir: s, Alternate: s + "/../c.git"},
			"its own Dir":     {Dir: s, Alternate: s},
			"no Dir":          {Alternate: s},
		} {
			cmd.Args, cmd.Class, cmd.Deadline = []string{"rev-parse", "--git-dir"}, gitproc.ClassRead, gitproc.ReadDeadline
			if _, err := p.Run(t.Context(), cmd); !errors.Is(err, gitproc.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", name, err)
			}
		}
	})
}

// EVERY SHAPE RULE IS ONE THE POOL REFUSES BY, AND CheckShape IS THE POOL'S
// OWN CHECK: a repository CheckShape refuses under a rule, Run refuses under
// the same rule before any git starts, and one CheckShape accepts, Run runs
// in. The cache's sweep holds what it repairs to CheckShape, so the two can
// never disagree about what the pool runs in.
func TestCheckShapeIsThePoolsOwnCheck(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	for _, rule := range gitproc.ShapeRules {
		if !rule.Valid() {
			t.Errorf("%s is in ShapeRules and not Valid", rule)
		}
	}
	if gitproc.ShapeRule("layout").Valid() {
		t.Error("a rule ShapeRules does not hold is Valid")
	}
	spoil := map[gitproc.ShapeRule]func(t *testing.T, bare string){
		gitproc.ShapeConfig: func(t *testing.T, bare string) {
			if err := os.Remove(filepath.Join(bare, "config")); err != nil {
				t.Fatal(err)
			}
		},
		gitproc.ShapeCommondir: func(t *testing.T, bare string) {
			if err := os.WriteFile(filepath.Join(bare, "commondir"), []byte("..\n"), hostbox.FileMode); err != nil {
				t.Fatal(err)
			}
		},
		gitproc.ShapeObjects: func(t *testing.T, bare string) {
			moved := filepath.Join(bare, "objects.d")
			if err := os.Rename(filepath.Join(bare, "objects"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(bare, "objects")); err != nil {
				t.Fatal(err)
			}
		},
		gitproc.ShapeAlternates: func(t *testing.T, bare string) {
			path := filepath.Join(bare, "objects", "info", "alternates")
			if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(gittest.Empty(t)+"/objects\n"), hostbox.FileMode); err != nil {
				t.Fatal(err)
			}
		},
		gitproc.ShapeDropping: func(t *testing.T, bare string) {
			if err := os.Symlink(filepath.Join(gittest.Empty(t), "objects"),
				filepath.Join(bare, gitproc.DroppingDir)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for rule := range spoil {
		if !rule.Valid() {
			t.Errorf("the spoils name %s, which is no ShapeRule", rule)
		}
	}
	for _, rule := range gitproc.ShapeRules {
		if spoil[rule] == nil {
			t.Errorf("%s is a ShapeRule this test spoils no repository by: add its spoil", rule)
			continue
		}
		t.Run(string(rule), func(t *testing.T) {
			bare := gittest.Empty(t)
			if err := gitproc.CheckShape(bare, ""); err != nil {
				t.Fatalf("CheckShape of an empty repository: %v", err)
			}
			spoil[rule](t, bare)
			var checked, ran *gitproc.ShapeError
			if err := gitproc.CheckShape(bare, ""); !errors.As(err, &checked) || checked.Rule != rule {
				t.Fatalf("CheckShape = %v, want a refusal under %s", err, rule)
			}
			_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--git-dir"},
				Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
			if !errors.As(err, &ran) || ran.Rule != rule || ran.Dir != bare || ran.InAlternate {
				t.Fatalf("Run = %v, want the refusal CheckShape gave, under %s naming %s", err, rule, bare)
			}
			for _, call := range calls() {
				if call.GitDir == bare {
					t.Fatalf("git ran in the refused repository: %v", call.Args)
				}
			}
		})
	}
}

// NO REPOSITORY FILE CHANGES WHAT HISTORY IS: info/grafts and shallow are
// ignored, and no deprecation hint is printed for the path that disables them.
func TestNoRepositoryFileChangesWhatHistoryIs(t *testing.T) {
	t.Parallel()
	onNodeGits(t, repositoryFilesOn)
}

// repositoryFilesOn is TestNoRepositoryFileChangesWhatHistoryIs on the git at git.
func repositoryFilesOn(t *testing.T, git string) {
	p := poolOn(t, git, gitproc.Options{})
	c := gittest.NewClient(t)
	wt := c.Init(t)
	c.Commit(t, wt, map[string]string{"a": "1\n"}, "one")
	c.Commit(t, wt, map[string]string{"a": "2\n"}, "two")
	tip := c.Commit(t, wt, map[string]string{"a": "3\n"}, "three")
	for _, file := range []string{"info/grafts", "shallow"} {
		bare := gittest.Empty(t)
		gittest.Pack(t, c, bare, wt, tip)
		gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": tip})
		path := filepath.Join(bare, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(tip+"\n"), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-list", "--count", "main"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "3" {
			t.Errorf("with %s naming the tip, rev-list --count = %s, want 3", file, got)
		}
		if strings.Contains(res.Stderr, "hint:") {
			t.Errorf("with %s, git printed a hint: %s", file, res.Stderr)
		}
	}
}

// HOOKS RUN ONLY WHERE THE CALL SITE NAMES THEM: a repository's own hooks/
// never runs.
func TestHooksRunOnlyWhereTheCallSiteNamesThem(t *testing.T) {
	t.Parallel()
	onNodeGits(t, hooksOn)
}

// hooksOn is TestHooksRunOnlyWhereTheCallSiteNamesThem on the git at git.
func hooksOn(t *testing.T, git string) {
	p := poolOn(t, git, gitproc.Options{})
	bare := gittest.Empty(t)
	writeHook(t, filepath.Join(bare, "hooks", "pre-receive"), "echo REPO HOOK >&2\nexit 1")
	cmd := gitproc.Cmd{Dir: bare, Args: []string{"hook", "run", "--ignore-missing", "pre-receive"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}
	res, err := p.Run(t.Context(), cmd)
	if err != nil || strings.Contains(res.Stderr, "REPO HOOK") {
		t.Fatalf("with no Hooks: %v, stderr %q; want no hook run", err, res.Stderr)
	}
	named := t.TempDir()
	writeHook(t, filepath.Join(named, "pre-receive"), "exit 3")
	cmd.Hooks = named
	_, err = p.Run(t.Context(), cmd)
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) || exit.Result.Exit != 3 {
		t.Fatalf("with Hooks naming a hook that exits 3: %v", err)
	}
}

// A RUN WITH NO REPOSITORY FINDS NONE, even when HOME sits inside one.
func TestARunWithNoRepositoryFindsNone(t *testing.T) {
	t.Parallel()
	onNodeGits(t, noRepositoryOn)
}

// noRepositoryOn is TestARunWithNoRepositoryFindsNone on the git at git.
func noRepositoryOn(t *testing.T, git string) {
	c := gittest.NewClient(t)
	wt := c.Init(t)
	p := poolOn(t, git, gitproc.Options{Home: filepath.Join(wt, "home")})
	res, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) || !strings.Contains(res.Stderr, "not a git repository") {
		t.Fatalf("rev-parse with no repository inside a work tree = %q, %v; want not a git repository", res.Stdout, err)
	}
}

// A CONFIGURATION KEY OFF THE ALLOWLIST IS REFUSED before any git runs —
// whatever its case — and so is a key the pool sets itself.
func TestAConfigKeyOutsideTheAllowlistIsRefused(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	before := len(calls())
	refused := map[string][]gitproc.KV{
		"allowAnySHA1InWant":       {{Key: "uploadpack.allowAnySHA1InWant", Value: "true"}},
		"its other case":           {{Key: "UploadPack.AllowAnySHA1InWant", Value: "true"}},
		"allowReachableSHA1InWant": {{Key: "uploadpack.allowReachableSHA1InWant", Value: "true"}},
		"allowTipSHA1InWant":       {{Key: "uploadpack.allowTipSHA1InWant", Value: "true"}},
		"allowRefInWant":           {{Key: "uploadpack.allowRefInWant", Value: "true"}},
		"packObjectsHook":          {{Key: "uploadpack.packObjectsHook", Value: "sh"}},
		"an alias":                 {{Key: "alias.x", Value: "!sh"}},
		"an include":               {{Key: "include.path", Value: "/etc/x"}},
		"fsmonitor":                {{Key: "core.fsmonitor", Value: "sh"}},
		"alternateRefsCommand":     {{Key: "core.alternateRefsCommand", Value: "sh"}},
		"duplicated":               {{Key: "receive.unpackLimit", Value: "1"}, {Key: "receive.unpacklimit", Value: "2"}},
		"a NUL value":              {{Key: "receive.unpackLimit", Value: "1\x00"}},
	}
	for name, config := range refused {
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"version"}, Config: config, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if !errors.Is(err, gitproc.ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
	for key, says := range map[string]string{
		"protocol.allow": "sets it for every process", "core.hooksPath": "Cmd.Hooks", "pack.threads": "sets it for every process",
		"core.attributesFile": "sets it for every process", "CORE.ATTRIBUTESFILE": "sets it for every process",
	} {
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"version"}, Config: []gitproc.KV{{Key: key, Value: "x"}}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if !errors.Is(err, gitproc.ErrConfig) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want ErrConfig saying %q", key, err, says)
		}
	}
	if got := len(calls()); got != before {
		t.Fatalf("%d git processes ran for refused configurations", got-before)
	}

	fake := fakePool(t, "exit 0", gitproc.Options{})
	for _, key := range []string{
		"core.multiPackIndex", "pack.allowPackReuse", "pack.windowMemory", "pack.useBitmaps",
		"pack.writeBitmapLookupTable", "commitGraph.changedPathsVersion",
		"uploadpack.allowFilter", "uploadpack.keepAlive",
		"uploadpackfilter.allow", "uploadpackfilter.blob:none.allow", "uploadpackfilter.blob:limit.allow",
		"uploadpackfilter.tree.maxDepth", "uploadpackfilter.object:type.allow", "uploadpackfilter.combine.allow",
		"receive.procReceiveRefs", "receive.unpackLimit", "receive.fsckObjects", "receive.maxInputSize",
		"receive.advertisePushOptions", "receive.autogc", "receive.updateServerInfo",
		"pack.writeReverseIndex", "pack.indexVersion",
	} {
		if _, err := fake.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Config: []gitproc.KV{{Key: key, Value: "1"}}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}); err != nil {
			t.Errorf("the allowlisted %s: %v", key, err)
		}
	}
}

// AN ENVIRONMENT NAME OUTSIDE THE TWO IS REFUSED — a commit's identity among
// them, whatever subcommand spells it, with the field that carries one named.
func TestAnEnvironmentNameOutsideTheTwoIsRefused(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "exit 0", gitproc.Options{})
	for name, env := range map[string][]gitproc.KV{
		"GIT_DIR":     {{Key: "GIT_DIR", Value: "x"}},
		"PATH":        {{Key: "PATH", Value: "x"}},
		"a duplicate": {{Key: gitproc.EnvHookToken, Value: "a"}, {Key: gitproc.EnvHookToken, Value: "b"}},
		"a NUL":       {{Key: gitproc.EnvHookToken, Value: "a\x00"}},
	} {
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Env: env, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if !errors.Is(err, gitproc.ErrEnv) {
			t.Errorf("%s: %v, want ErrEnv", name, err)
		}
	}
	id := suiteIdentity()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE"} {
		for _, c := range []gitproc.Cmd{
			{Args: []string{"commit-tree", emptyTree}, Identity: &id},
			{Args: []string{"hash-object", "--stdin"}},
		} {
			c.Env, c.Class, c.Deadline = []gitproc.KV{{Key: name, Value: "x"}}, gitproc.ClassRead, gitproc.ReadDeadline
			if _, err := p.Run(t.Context(), c); !errors.Is(err, gitproc.ErrEnv) || !strings.Contains(err.Error(), "Cmd.Identity") {
				t.Errorf("git %s naming %s in Env: %v, want ErrEnv pointing at Cmd.Identity", c.Args[0], name, err)
			}
		}
	}
	_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Env: []gitproc.KV{
		{Key: gitproc.EnvHookSocket, Value: "/s"}, {Key: gitproc.EnvHookToken, Value: "t"},
	}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		t.Fatalf("both allowed names: %v", err)
	}
}

// A MALFORMED COMMAND IS REFUSED before any process: nothing a Cmd says can
// make git something other than a subcommand in a repository the engine made.
// Every case is put to a pool of the machine's cores and to a pool of ONE,
// whose read lane has nothing to lend: a rule about loans that held only where
// a loan is made — the Indexed a push's receive-pack must carry above all —
// would pass a call site's tests on a one-core runner and fail it everywhere
// else.
func TestAMalformedCommandIsRefused(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	pools := map[string]*gitproc.Pool{
		"the machine's cores": newPool(t, gitproc.Options{Path: recorder}),
		"one core":            newPool(t, gitproc.Options{Path: recorder, MaxProcesses: 1}),
	}
	before := len(calls())
	ok := gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}
	// receivePack spoils a command into a receive-pack in the transfer lane
	// given args after its subcommand and indexed as its Cmd.Indexed.
	receivePack := func(indexed <-chan struct{}, args ...string) func(*gitproc.Cmd) {
		return func(c *gitproc.Cmd) {
			c.Args, c.Class, c.Share, c.Indexed = append([]string{"receive-pack"}, args...), gitproc.ClassTransfer, suiteShare, indexed
		}
	}
	for name, spoil := range map[string]func(*gitproc.Cmd){
		"an unknown class":  func(c *gitproc.Cmd) { c.Class = "install" },
		"no args":           func(c *gitproc.Cmd) { c.Args = nil },
		"-c":                func(c *gitproc.Cmd) { c.Args = []string{"-c", "x=y", "version"} },
		"--exec-path":       func(c *gitproc.Cmd) { c.Args = []string{"--exec-path=/x", "version"} },
		"a capital":         func(c *gitproc.Cmd) { c.Args = []string{"Upload-Pack"} },
		"a NUL argument":    func(c *gitproc.Cmd) { c.Args = []string{"version", "a\x00b"} },
		"a relative Dir":    func(c *gitproc.Cmd) { c.Dir = "repo" },
		"an unclean Dir":    func(c *gitproc.Cmd) { c.Dir = "/a/../b" },
		"a negative Stall":  func(c *gitproc.Cmd) { c.Stall = -1 },
		"an answer of 0":    func(c *gitproc.Cmd) { c.Answers = []int{0} },
		"an answer of 256":  func(c *gitproc.Cmd) { c.Answers = []int{256} },
		"a relative Hooks":  func(c *gitproc.Cmd) { c.Hooks = "hooks" },
		"a missing Hooks":   func(c *gitproc.Cmd) { c.Hooks = filepath.Join(t.TempDir(), "absent") },
		"Indexed on a read": func(c *gitproc.Cmd) { c.Indexed = make(chan struct{}) },
		"a transfer with no Share": func(c *gitproc.Cmd) {
			c.Args, c.Class = []string{"upload-pack"}, gitproc.ClassTransfer
		},
		// A principal no identity is given: past every name the bound
		// leaves room for, or holding a NUL — a caller handing the share
		// the wrong string.
		"a transfer whose principal is over-long": func(c *gitproc.Cmd) {
			c.Args, c.Class = []string{"upload-pack"}, gitproc.ClassTransfer
			c.Share = gitproc.Share{Class: gitproc.ShareMachine, Principal: strings.Repeat("p", 256)}
		},
		"a transfer whose principal holds a NUL": func(c *gitproc.Cmd) {
			c.Args, c.Class = []string{"upload-pack"}, gitproc.ClassTransfer
			c.Share = gitproc.Share{Class: gitproc.ShareMachine, Principal: "token:ci\x00"}
		},
		"a Share on a read": func(c *gitproc.Cmd) { c.Share = suiteShare },
		"Indexed on an upload-pack": func(c *gitproc.Cmd) {
			c.Args, c.Class, c.Share, c.Indexed = []string{"upload-pack"}, gitproc.ClassTransfer, suiteShare, make(chan struct{})
		},
		// A push's receive-pack, as the host runs it, without the Indexed
		// that repays its loan once its pack is indexed: its loan would be
		// held through the hook, the upload and the wait for its record.
		"a push's receive-pack without Indexed":     receivePack(nil, "--stateless-rpc", "/r"),
		"a receive-pack with no options or Indexed": receivePack(nil, "/r"),
		// A push's ADVERTISEMENT — by either full name of the option, and
		// wherever it stands, as git reads it — starts no index-pack and
		// reaches no hook, so nothing would ever close one.
		"Indexed on an advertisement":                     receivePack(make(chan struct{}), "--stateless-rpc", "--advertise-refs", "/r"),
		"Indexed on an advertisement by its other name":   receivePack(make(chan struct{}), "--stateless-rpc", "--http-backend-info-refs", "/r"),
		"Indexed on an advertisement after its directory": receivePack(make(chan struct{}), "/r", "--advertise-refs"),
		// The pool tells an advertisement from a push by its options, so it
		// admits only the spellings it reads exactly as git does: git also
		// reads an abbreviation, a negation and "--", and each would be
		// judged a push — or a push judged an advertisement — by a rule
		// that guessed.
		"an abbreviated option": receivePack(make(chan struct{}), "--stateless-rpc", "--adv", "/r"),
		"a negated option":      receivePack(nil, "--stateless-rpc", "--advertise-refs", "--no-advertise-refs", "/r"),
		"a short option":        receivePack(make(chan struct{}), "-q", "/r"),
		"an end of options":     receivePack(make(chan struct{}), "--stateless-rpc", "--", "/r"),
		"an option's value":     receivePack(make(chan struct{}), "--stateless-rpc=1", "/r"),
		"a receive-pack option in another lane": func(c *gitproc.Cmd) {
			c.Args = []string{"receive-pack", "--adv", "/r"}
		},
	} {
		for pool, p := range pools {
			c := ok
			spoil(&c)
			if _, err := p.Run(t.Context(), c); !errors.Is(err, gitproc.ErrInvalid) {
				t.Errorf("%s, on %s: %v, want ErrInvalid", name, pool, err)
			}
		}
	}
	if got := len(calls()); got != before {
		t.Fatalf("%d git processes ran for malformed commands", got-before)
	}
}

// A POPULATED HOME IS REFUSED, NEVER CLEARED; an empty one of the wrong mode
// is accepted and tightened.
func TestAPopulatedHomeIsRefused(t *testing.T) {
	t.Parallel()
	fake := gittest.Fake(t, fakeVersion, "exit 0")
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "left-behind"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: fake, Home: home})
	if !errors.Is(err, gitproc.ErrHome) || !strings.Contains(err.Error(), "left-behind") {
		t.Fatalf("a HOME holding a file = %v, want ErrHome naming it", err)
	}
	if _, err := os.Stat(filepath.Join(home, "left-behind")); err != nil {
		t.Fatalf("the refusal touched HOME's file: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	newPool(t, gitproc.Options{Path: fake, Home: empty})
	info, err := os.Stat(empty)
	if err != nil || info.Mode().Perm() != hostbox.DirMode {
		t.Fatalf("an empty 0755 HOME after New: %v, %v; want %v", info.Mode().Perm(), err, hostbox.DirMode)
	}
}

// writeHook writes an executable shell hook and waits until it can be exec'd
// (the window hostbox.Settle closes: a parallel suite forks constantly).
func writeHook(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"${1-}\" = --settle-probe ] && exit 0\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hostbox.Settle(t.Context(), path, "--settle-probe"); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// pkt is s as one pkt-line, its length computed rather than written by hand.
func pkt(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

// NO CONFIGURATION MOVES THE TRACE. git reads its trace2 targets from its
// system and global configuration as well as the environment, so a HOME that
// names every trace2 key — the targets moved to files, the event stream made
// verbose and nested, every configuration value and environment variable
// listed — would move or bloat the stream the pool classifies from. After
// the pool is built its HOME gains both files naming them: a run's stream
// still holds its version event and exactly the events it held before (no
// def_param, no def_param per variable), and nothing is written at any
// target. A call site's own configuration naming a trace2 key is refused
// before git starts.
//
// Mutation: drop GIT_CONFIG_GLOBAL=/dev/null from the base environment, and
// the run's events grow by the def_param lines and the targets fill.
func TestNoConfigurationMovesTheTrace(t *testing.T) {
	t.Parallel()
	onNodeGits(t, configurationCannotMoveTheTraceOn)
}

// configurationCannotMoveTheTraceOn is TestNoConfigurationMovesTheTrace on the git at git.
func configurationCannotMoveTheTraceOn(t *testing.T, git string) {
	home := filepath.Join(t.TempDir(), "home")
	p := poolOn(t, git, gitproc.Options{Home: home})
	bare := gittest.Empty(t)
	run := func() gitproc.Trace {
		t.Helper()
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--verify", "refs/heads/absent"},
			Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
		if !errors.Is(err, gitproc.ErrFailed) {
			t.Fatalf("rev-parse of an absent ref = %v, want ErrFailed", err)
		}
		return res.Trace
	}
	before := run()
	if before.Version == "" || before.Errors != 1 {
		t.Fatalf("the stream before any planting: version %q, %d error events", before.Version, before.Errors)
	}

	targets := t.TempDir()
	planted := "[trace2]\n" +
		"\teventTarget = " + filepath.Join(targets, "event") + "\n" +
		"\tnormalTarget = " + filepath.Join(targets, "normal") + "\n" +
		"\tperfTarget = " + filepath.Join(targets, "perf") + "\n" +
		"\teventBrief = false\n\teventNesting = 9\n" +
		"\tconfigParams = *\n\tenvVars = HOME,PATH,GIT_DIR\n" +
		"\tdestinationDebug = true\n\tmaxFiles = 1\n"
	for _, rel := range []string{".gitconfig", ".config/git/config"} {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(planted), hostbox.FileMode); err != nil {
			t.Fatal(err)
		}
	}
	after := run()
	if after.Version != before.Version || after.Events != before.Events || after.Errors != before.Errors ||
		after.Unread != 0 {
		t.Errorf("with trace2 keys in HOME the stream holds %d events (%d errors, %d unread, version %q); "+
			"before, %d (%d, version %q)", after.Events, after.Errors, after.Unread, after.Version,
			before.Events, before.Errors, before.Version)
	}
	if written, _ := os.ReadDir(targets); len(written) != 0 {
		t.Errorf("git wrote trace files at the planted targets: %v", written)
	}

	_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "HEAD"},
		Config: []gitproc.KV{{Key: "trace2.eventTarget", Value: filepath.Join(targets, "event")}},
		Class:  gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if !errors.Is(err, gitproc.ErrConfig) {
		t.Errorf("a call site naming trace2.eventTarget = %v, want ErrConfig", err)
	}
}
