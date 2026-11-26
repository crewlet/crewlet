package executor

import (
	"bytes"
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
)

// EACH BOX ARCH GETS A STATIC LINUX EXECUTABLE OF ITS OWN ARCH: the build for
// amd64 is an x86-64 ELF and arm64's an AArch64 one, executable, with no
// interpreter, since a box's image promises no C library. It reads the builds
// TestMain made with helperbuild's recipe, so it holds the recipe's flags.
func TestEachBoxArchGetsAStaticLinuxNoteHelperOfItsOwnArch(t *testing.T) {
	t.Parallel()
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		bin, err := noteHelperBinary(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		f, err := elf.NewFile(bytes.NewReader(bin))
		if err != nil {
			t.Fatalf("%s: the note helper is not an ELF file: %v", arch, err)
		}
		if f.Machine != machine || f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX || f.Type != elf.ET_EXEC {
			t.Errorf("%s: the note helper is %v %v %v, want a %v executable", arch, f.Machine, f.OSABI, f.Type, machine)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("%s: the note helper names an interpreter, so it is not static", arch)
			}
		}
	}
	if _, err := noteHelperBinary("riscv64"); !errors.Is(err, errNoHelperBuild) {
		t.Errorf("an arch with no build answered %v, want errNoHelperBuild", err)
	}
}

// THE FIRST DIRECTORY HOLDING THE BUILD WINS, in the order they are looked
// in: each one's own build is read while those before it hold none.
// Mutation, red: look in the directories from the last.
func TestTheNoteHelperIsReadFromTheFirstDirectoryHoldingIt(t *testing.T) {
	t.Parallel()
	search := helperSearch{dirs: []string{t.TempDir(), t.TempDir(), t.TempDir()}}
	for i, dir := range search.dirs {
		if err := os.WriteFile(filepath.Join(dir, helperbuild.File("amd64")), []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, dir := range search.dirs {
		got, err := findNoteHelper("amd64", search)
		if err != nil || string(got) != string([]byte{byte('a' + i)}) {
			t.Fatalf("with directories %d on holding the build, read %q, %v; want directory %d's", i, got, err, i)
		}
		if err = os.Remove(filepath.Join(dir, helperbuild.File("amd64"))); err != nil {
			t.Fatal(err)
		}
	}
}

// A NAMED DIRECTORY IS THE ONLY PLACE LOOKED, AND OTHERWISE THE ENGINE LOOKS
// BESIDE ITSELF, THEN IN THE IMAGE'S: whoever sets the variable means that
// build, so nothing found elsewhere stands in for it; a release archive's
// builds sit beside its binary, the image's under /usr/local/share/crewlet.
// Mutations, red: fall back past a named directory; swap the two others.
func TestANamedDirectoryIsTheOnlyPlaceLookedElseBesideTheEngineThenTheImage(t *testing.T) {
	named := t.TempDir()
	t.Setenv(helperbuild.DirEnv, named)
	if got := noteHelperSearch().dirs; strings.Join(got, "\n") != named {
		t.Errorf("with %s set the engine looks in %q, want %q alone", helperbuild.DirEnv, got, named)
	}
	t.Setenv(helperbuild.DirEnv, "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	got, want := noteHelperSearch().dirs, []string{filepath.Dir(exe), "/usr/local/share/crewlet"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("with %s unset the engine looks in %q, want %q", helperbuild.DirEnv, got, want)
	}
}

// A MISSING BUILD REFUSES THE LAUNCH, SAYING WHERE IT LOOKED, IN WHAT ORDER,
// AND HOW TO GET ONE, and is never errNoHelperBuild, which several harnesses
// start without: an install missing its helper must not quietly run boxes
// without it. Mutations, red: answer errNoHelperBuild; drop the order.
func TestAMissingNoteHelperRefusesNamingWhereItLookedAndHowToGetIt(t *testing.T) {
	t.Setenv(helperbuild.DirEnv, "")
	unset := noteHelperSearch()
	unset.dirs = []string{t.TempDir(), t.TempDir()}
	named := t.TempDir()
	t.Setenv(helperbuild.DirEnv, named)
	for _, tc := range []struct {
		search helperSearch
		says   []string
	}{
		{unset, []string{"notehelper-linux-arm64 is not beside the engine (", "), nor then in " +
			"/usr/local/share/crewlet (CREWLET_NOTE_HELPER_DIR is unset)", "release archive", "make crewlet"}},
		{noteHelperSearch(), []string{"notehelper-linux-arm64 is not in " + named + ", which " +
			"CREWLET_NOTE_HELPER_DIR names, and while it is set the engine looks nowhere else", "unset " +
			"CREWLET_NOTE_HELPER_DIR for the engine to look beside itself, then in /usr/local/share/crewlet"}},
	} {
		_, err := findNoteHelper("arm64", tc.search)
		if err == nil {
			t.Fatal("no build anywhere answered a note helper")
		}
		if errors.Is(err, errNoHelperBuild) {
			t.Errorf("a missing build is errNoHelperBuild, which a harness starts without: %v", err)
		}
		for _, want := range append(tc.says, "linux/arm64") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q", err, want)
			}
		}
	}
}
