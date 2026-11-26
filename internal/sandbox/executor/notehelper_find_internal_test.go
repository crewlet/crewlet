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

// THE FIRST PLACE HOLDING THE BUILD WINS, in the order the places are looked
// in: each place's own build is read while the places before it hold none.
// Mutation, red: look in the places from the last.
func TestTheNoteHelperIsReadFromTheFirstPlaceHoldingIt(t *testing.T) {
	t.Parallel()
	places := []helperPlace{{dir: t.TempDir()}, {dir: t.TempDir()}, {dir: t.TempDir()}}
	for i, p := range places {
		if err := os.WriteFile(filepath.Join(p.dir, helperbuild.File("amd64")), []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range places {
		got, err := findNoteHelper("amd64", places)
		if err != nil || string(got) != string([]byte{byte('a' + i)}) {
			t.Fatalf("with places %d on holding the build, read %q, %v; want place %d's", i, got, err, i)
		}
		if err = os.Remove(filepath.Join(p.dir, helperbuild.File("amd64"))); err != nil {
			t.Fatal(err)
		}
	}
}

// THE ENGINE LOOKS BESIDE ITSELF, THEN IN THE IMAGE'S DIRECTORY, THEN WHERE
// THE VARIABLE SAYS: a release archive's builds sit beside its binary, the
// image's under /usr/local/share/crewlet, a checkout's where it names them.
// Mutation, red: swap two places.
func TestTheEngineLooksBesideItselfThenInTheImageThenWhereTheVariableSays(t *testing.T) {
	named := t.TempDir()
	t.Setenv(helperbuild.DirEnv, named)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range noteHelperPlaces() {
		got = append(got, p.dir)
	}
	if want := []string{filepath.Dir(exe), "/usr/local/share/crewlet", named}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the engine looks in %q, want %q", got, want)
	}
}

// A MISSING BUILD REFUSES THE LAUNCH, NAMING WHERE IT LOOKED AND HOW TO GET
// ONE, and is never errNoHelperBuild, which several harnesses start without:
// an install missing its helper must not quietly run boxes without it.
// Mutations, red: answer errNoHelperBuild; drop a place from the sentence.
func TestAMissingNoteHelperRefusesNamingWhereItLookedAndHowToGetIt(t *testing.T) {
	t.Parallel()
	_, err := findNoteHelper("arm64", []helperPlace{
		{dir: t.TempDir(), says: "beside the engine (/opt/crewlet)"},
		{dir: t.TempDir(), says: "in /usr/local/share/crewlet"},
		{says: "in CREWLET_NOTE_HELPER_DIR (unset)"}})
	if err == nil {
		t.Fatal("no build anywhere answered a note helper")
	}
	if errors.Is(err, errNoHelperBuild) {
		t.Errorf("a missing build is errNoHelperBuild, which a harness starts without: %v", err)
	}
	for _, want := range []string{"linux/arm64", "notehelper-linux-arm64 is not beside the engine (/opt/crewlet), " +
		"in /usr/local/share/crewlet or in CREWLET_NOTE_HELPER_DIR (unset)", "release archive", "make notehelper"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}
