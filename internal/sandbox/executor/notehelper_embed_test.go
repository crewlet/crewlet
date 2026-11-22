package executor

import (
	"bytes"
	"debug/elf"
	"testing"
)

// EACH BOX ARCH GETS A STATIC LINUX EXECUTABLE OF ITS OWN ARCH: the embedded
// build for amd64 is an x86-64 ELF and arm64's an AArch64 one, executable,
// with no interpreter, since a box's image promises no C library.
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
	if _, err := noteHelperBinary("riscv64"); err == nil {
		t.Error("an arch with no build answered a note helper")
	}
}
