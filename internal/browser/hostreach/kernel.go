package hostreach

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

// Kernel is a kernel's boot id, as /proc/sys/kernel/random/boot_id holds it:
// what a box on that kernel reads there, and one on any other cannot know.
type Kernel string

// ErrNoSharedKernel is an engine host whose kernel no box can share: one on
// another system than Linux, or under WSL. [EngineKernel]'s error says which,
// and what to change.
var ErrNoSharedKernel = errors.New("hostreach: no container box can run on this engine's kernel")

// The files the engine reads its own kernel from.
const (
	bootIDFile    = "/proc/sys/kernel/random/boot_id"
	osReleaseFile = "/proc/sys/kernel/osrelease"
)

// wslMarks are what WSL leaves on every distro whatever its kernel is named:
// its interop's binfmt_misc entries (the second a later WSL's name) and the
// directory its init keeps its sockets in.
var wslMarks = []string{"/proc/sys/fs/binfmt_misc/WSLInterop", "/proc/sys/fs/binfmt_misc/WSLInterop-late",
	"/run/WSL"}

// sharedKernelError is [ErrNoSharedKernel] in words that say which host this
// is and what to change, which a withheld browser's log line carries whole.
type sharedKernelError struct{ detail string }

func (e *sharedKernelError) Error() string { return e.detail }
func (e *sharedKernelError) Unwrap() error { return ErrNoSharedKernel }

// remedy is what gives a seat a browser where the engine's kernel is no box's.
const remedy = "run the engine on Linux, outside WSL, beside Docker Engine or Podman on the same kernel, " +
	"to give this seat a browser"

// EngineKernel is the kernel this engine runs on, the one a box must show it
// runs on: [ErrNoSharedKernel] on another system than Linux or under WSL, any
// other error where the boot id cannot be read or is no boot id. Read at each
// call, two small files, so a read that failed once is not held for the
// process's life.
func EngineKernel() (Kernel, error) {
	return readEngineKernel(runtime.GOOS, os.ReadFile, os.Stat)
}

// readEngineKernel is [EngineKernel] on the system goos, its files read
// through read and looked for through stat. WSL is told by its kernel's
// release naming Microsoft or WSL in any case, as systemd's detect_container
// tells it but for the case, which a WSL 2 kernel of 4.19 spells lower; or,
// for a kernel named neither, by one of [wslMarks].
func readEngineKernel(goos string, read func(string) ([]byte, error),
	stat func(string) (fs.FileInfo, error)) (Kernel, error) {
	if goos != "linux" {
		return "", &sharedKernelError{detail: fmt.Sprintf("this host runs %s, whose container runtimes all run "+
			"their boxes in a virtual machine: %s", goos, remedy)}
	}
	raw, err := read(osReleaseFile)
	if err != nil {
		return "", fmt.Errorf("its release in %s, which tells WSL, is unreadable: %w", osReleaseFile, err)
	}
	release := strings.TrimSpace(string(raw))
	wsl := strings.Contains(strings.ToLower(release), "microsoft") || strings.Contains(strings.ToLower(release), "wsl")
	for _, mark := range wslMarks {
		switch _, statErr := stat(mark); {
		case statErr == nil:
			wsl = true
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", fmt.Errorf("%s, which tells WSL, cannot be looked for: %w", mark, statErr)
		}
	}
	if wsl {
		return "", &sharedKernelError{detail: fmt.Sprintf("this host's kernel is WSL's (%s), which every WSL "+
			"distro shares, Docker Desktop's and Podman's machine's among them: %s", release, remedy)}
	}
	raw, err = read(bootIDFile)
	if err != nil {
		return "", fmt.Errorf("%s is unreadable: %w", bootIDFile, err)
	}
	id, err := uuid.Parse(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", fmt.Errorf("%s holds %q, which is no boot id", bootIDFile, strings.TrimSpace(string(raw)))
	}
	return Kernel(id.String()), nil
}
