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
// an alternate object directory, a config of its own — is absent, and what is
// present is exactly the pool's table, its base configuration first.
func TestTheEnvironmentIsBuiltNotInherited(t *testing.T) {
	for name, value := range map[string]string{
		"CREWLET_SECRET_X":                 "leaked",
		"GIT_CONFIG_PARAMETERS":            "'core.hooksPath'='/tmp'",
		"GIT_EXEC_PATH":                    "/nonexistent",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/tmp",
		"GIT_SSH_COMMAND":                  "x",
		"GIT_TRACE":                        "1",
		"XDG_CONFIG_HOME":                  "/tmp",
		"GIT_CONFIG_SYSTEM":                "/tmp/x",
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
		"PATH", "HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT",
		"GIT_NO_REPLACE_OBJECTS", "GIT_GRAFT_FILE", "GIT_SHALLOW_FILE", "LC_ALL", "GIT_DIR",
		"GIT_PROTOCOL", "GIT_CONFIG_COUNT", gitproc.EnvHookToken,
	}
	for i := range 6 {
		want = append(want, "GIT_CONFIG_KEY_"+string(rune('0'+i)), "GIT_CONFIG_VALUE_"+string(rune('0'+i)))
	}
	slices.Sort(want)
	if got := slices.Sorted(maps.Keys(env)); !slices.Equal(got, want) {
		t.Fatalf("the process saw\n  %v\nwant exactly\n  %v", got, want)
	}
	for name, value := range map[string]string{
		"PATH":                   os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_GLOBAL":      "/dev/null",
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_GRAFT_FILE":         "/dev/null/grafts",
		"GIT_SHALLOW_FILE":       "/dev/null/shallow",
		"LC_ALL":                 "C",
		"GIT_DIR":                bare,
		"GIT_PROTOCOL":           "version=2",
		gitproc.EnvHookToken:     "t",
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
// it after the pool checked it was empty.
func TestTheHomeCanNeverConfigureAGitProcess(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	p := realPool(t, gitproc.Options{Home: home})
	hooks := t.TempDir()
	writeHook(t, filepath.Join(hooks, "pre-receive"), "exit 7")
	planted := "[uploadpack]\n\tallowAnySHA1InWant = true\n[core]\n\thooksPath = " + hooks + "\n"
	for _, rel := range []string{".gitconfig", ".config/git/config"} {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(planted), hostbox.FileMode); err != nil {
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
		Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
	})
	if !errors.Is(err, gitproc.ErrNotOurRef) {
		t.Fatalf("a v0 want of an unreachable commit = %v, want ErrNotOurRef", err)
	}

	res, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"hook", "run", "--ignore-missing", "pre-receive"},
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil || res.Exit != 0 {
		t.Fatalf("hook run with a planted hooksPath = exit %d, %v; want no hook run", res.Exit, err)
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
	for name, spoil := range map[string]func(t *testing.T, bare string){
		"its config": func(t *testing.T, bare string) {
			replaceWithDir(t, filepath.Join(bare, "config"))
		},
		"its alternates": func(t *testing.T, bare string) {
			replaceWithDir(t, filepath.Join(bare, "objects", "info", "alternates"))
		},
		"its alternate's config": func(t *testing.T, bare string) {
			c := gittest.Empty(t)
			replaceWithDir(t, filepath.Join(c, "config"))
			if err := os.MkdirAll(filepath.Join(bare, "objects", "info"), hostbox.DirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bare, "objects", "info", "alternates"), []byte(c+"/objects\n"), hostbox.FileMode); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bare := gittest.Empty(t)
			spoil(t, bare)
			_, err := p.Run(t.Context(), gitproc.Cmd{Dir: bare, Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
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

// AN ALTERNATE IS HELD TO THE SAME SHAPE, one hop further than the process's
// own repository, because git lists an alternate's refs with a git it
// configures with nothing of the pool's.
func TestAnAlternateIsHeldToTheSameShape(t *testing.T) {
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
	run := func(t *testing.T, scratch string) error {
		t.Helper()
		_, err := p.Run(t.Context(), gitproc.Cmd{Dir: scratch, Args: []string{"rev-parse", "--git-dir"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
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
	alternates := func(s string) string { return filepath.Join(s, "objects", "info", "alternates") }

	t.Run("one cache", func(t *testing.T) {
		s, c := gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(s), c+"/objects\n")
		if err := run(t, s); err != nil || !ranIn(s) {
			t.Fatalf("a scratch reading one cache: %v (ran %v)", err, ranIn(s))
		}
	})
	t.Run("two caches", func(t *testing.T) {
		s, c1, c2 := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		write(t, alternates(s), c1+"/objects\n"+c2+"/objects\n")
		if err := run(t, s); err != nil {
			t.Fatalf("a scratch reading two caches: %v", err)
		}
	})
	for name, spoil := range map[string]func(t *testing.T, c string){
		"its config planted": func(t *testing.T, c string) {
			write(t, filepath.Join(c, "config"), gitproc.RepoConfig+"[uploadpack]\n\tallowAnySHA1InWant = true\n")
		},
		"a commondir": func(t *testing.T, c string) { write(t, filepath.Join(c, "commondir"), "/elsewhere\n") },
		"an alternate of its own": func(t *testing.T, c string) {
			write(t, alternates(c), gittest.Empty(t)+"/objects\n")
		},
	} {
		t.Run("the cache with "+name, func(t *testing.T) {
			s, c := gittest.Empty(t), gittest.Empty(t)
			write(t, alternates(s), c+"/objects\n")
			spoil(t, c)
			err := run(t, s)
			if !errors.Is(err, gitproc.ErrRepoShape) || !strings.Contains(err.Error(), c) {
				t.Fatalf("%v, want ErrRepoShape naming the cache %s", err, c)
			}
			if ranIn(s) {
				t.Fatal("git ran in the scratch whose alternate was refused")
			}
		})
	}
	// GIT LISTS AN ALTERNATE'S REFS AT ITS REAL PATH — the line resolved,
	// "/objects" stripped — so the guard checks the repository a link leads
	// to, never the one whose name the line merely passes through.
	link := func(t *testing.T, c, target string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(c, "objects")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(c, "objects")); err != nil {
			t.Fatal(err)
		}
	}
	resolved := func(t *testing.T, path string) string {
		t.Helper()
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	t.Run("the cache's objects a link into a planted repository", func(t *testing.T) {
		s, c, other := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		write(t, filepath.Join(other, "config"), gitproc.RepoConfig+"[uploadpack]\n\tallowAnySHA1InWant = true\n")
		link(t, c, filepath.Join(other, "objects"))
		write(t, alternates(s), c+"/objects\n")
		err := run(t, s)
		if !errors.Is(err, gitproc.ErrRepoShape) || !strings.Contains(err.Error(), resolved(t, other)) {
			t.Fatalf("%v, want ErrRepoShape naming the repository the link leads to, %s", err, other)
		}
		if ranIn(s) {
			t.Fatal("git ran in the scratch whose alternate leads to a planted repository")
		}
	})
	t.Run("the cache's objects a link to a directory not named objects", func(t *testing.T) {
		s, c, other := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		if err := os.Rename(filepath.Join(other, "objects"), filepath.Join(other, "store")); err != nil {
			t.Fatal(err)
		}
		link(t, c, filepath.Join(other, "store"))
		write(t, alternates(s), c+"/objects\n")
		if err := run(t, s); !errors.Is(err, gitproc.ErrRepoShape) {
			t.Fatalf("%v, want ErrRepoShape", err)
		}
		if ranIn(s) {
			t.Fatal("git ran in the scratch whose alternate is no repository's objects")
		}
	})
	t.Run("the cache's objects a link that leads nowhere", func(t *testing.T) {
		s, c := gittest.Empty(t), gittest.Empty(t)
		link(t, c, filepath.Join(t.TempDir(), "gone", "objects"))
		write(t, alternates(s), c+"/objects\n")
		if err := run(t, s); !errors.Is(err, gitproc.ErrRepoShape) {
			t.Fatalf("%v, want ErrRepoShape", err)
		}
		if ranIn(s) {
			t.Fatal("git ran in the scratch whose alternate leads nowhere")
		}
	})
	t.Run("the cache's objects a link to a repository of the engine's shape", func(t *testing.T) {
		s, c, other := gittest.Empty(t), gittest.Empty(t), gittest.Empty(t)
		link(t, c, filepath.Join(other, "objects"))
		write(t, alternates(s), c+"/objects\n")
		if err := run(t, s); err != nil || !ranIn(s) {
			t.Fatalf("a scratch whose cache links to a well-shaped repository: %v (ran %v)", err, ranIn(s))
		}
	})
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
			if err := run(t, s); !errors.Is(err, gitproc.ErrRepoShape) {
				t.Fatalf("%v, want ErrRepoShape", err)
			}
			if ranIn(s) {
				t.Fatal("git ran in the scratch whose alternates was refused")
			}
		})
	}
}

// NO REPOSITORY FILE CHANGES WHAT HISTORY IS: info/grafts and shallow are
// ignored, and no deprecation hint is printed for the path that disables them.
func TestNoRepositoryFileChangesWhatHistoryIs(t *testing.T) {
	t.Parallel()
	p := realPool(t, gitproc.Options{})
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
	p := realPool(t, gitproc.Options{})
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
	c := gittest.NewClient(t)
	wt := c.Init(t)
	p := realPool(t, gitproc.Options{Home: filepath.Join(wt, "home")})
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
	for key, says := range map[string]string{"protocol.allow": "sets it for every process", "core.hooksPath": "Cmd.Hooks", "pack.threads": "sets it for every process"} {
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
		"uploadpack.allowFilter", "uploadpack.allowRefInWant", "uploadpack.keepAlive",
		"receive.procReceiveRefs", "receive.unpackLimit", "receive.fsckObjects", "receive.maxInputSize",
		"receive.advertisePushOptions", "receive.autogc", "receive.updateServerInfo",
		"pack.writeReverseIndex", "pack.indexVersion",
	} {
		if _, err := fake.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Config: []gitproc.KV{{Key: key, Value: "1"}}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}); err != nil {
			t.Errorf("the allowlisted %s: %v", key, err)
		}
	}
}

// AN ENVIRONMENT NAME OUTSIDE THE TWO IS REFUSED.
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
	_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Env: []gitproc.KV{
		{Key: gitproc.EnvHookSocket, Value: "/s"}, {Key: gitproc.EnvHookToken, Value: "t"},
	}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if err != nil {
		t.Fatalf("both allowed names: %v", err)
	}
}

// A MALFORMED COMMAND IS REFUSED before any process: nothing a Cmd says can
// make git something other than a subcommand in a repository the engine made.
func TestAMalformedCommandIsRefused(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	before := len(calls())
	ok := gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}
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
		"Indexed on an upload-pack": func(c *gitproc.Cmd) {
			c.Args, c.Class, c.Indexed = []string{"upload-pack"}, gitproc.ClassTransfer, make(chan struct{})
		},
	} {
		c := ok
		spoil(&c)
		if _, err := p.Run(t.Context(), c); !errors.Is(err, gitproc.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
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
