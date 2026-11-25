package hostreach

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// THE ENGINE'S KERNEL IS ITS BOOT ID, AND NONE UNDER WSL OR OFF LINUX: Linux
// with a boot id is that id; another system, a kernel whose release names WSL
// (WSL 2's) or Microsoft in any case (WSL 1's, and WSL 2's of 4.19), and a
// kernel named neither on a host bearing one of WSL's marks, is
// ErrNoSharedKernel, saying which host and what to change; a boot id unread
// or malformed, or a mark that cannot be looked for, is another error, which
// Check reads as unchecked, never as another kernel.
//
// Mutations, each red: drop the WSL test (WSL 2's release); match only
// "microsoft" (WSL 1's), or only "Microsoft" (4.19's); drop a mark; trust a
// malformed id; read an unreadable id as no shared kernel.
func TestTheEnginesKernelIsItsBootIDAndNoneUnderWSL(t *testing.T) {
	t.Parallel()
	const id = "6b9a3f1e-2c4d-4e5f-8a7b-9c0d1e2f3a4b"
	files := func(release, boot string) func(string) ([]byte, error) {
		return func(name string) ([]byte, error) {
			switch {
			case name == osReleaseFile && release != "":
				return []byte(release + "\n"), nil
			case name == bootIDFile && boot != "":
				return []byte(boot + "\n"), nil
			}
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
	}
	// marked is a host bearing mark, failing to be looked for with err where
	// err is set; nothing else is there.
	marked := func(mark string, err error) func(string) (fs.FileInfo, error) {
		return func(name string) (fs.FileInfo, error) {
			switch {
			case name == mark && err != nil:
				return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
			case name == mark:
				return nil, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		}
	}
	unmarked := marked("", nil)
	got, err := readEngineKernel("linux", files("7.0.0-44-generic", id), unmarked)
	if err != nil || got != Kernel(id) {
		t.Errorf("Linux with a boot id answers %q, %v; want %q", got, err, id)
	}
	type host struct {
		goos, release, said string
		stat                func(string) (fs.FileInfo, error)
	}
	cases := map[string]host{
		"macOS":          {goos: "darwin", said: "this host runs darwin"},
		"WSL 2's kernel": {goos: "linux", release: "6.6.87.2-microsoft-standard-WSL2", said: "WSL's (6.6.87.2"},
		"WSL 2's kernel of 4.19": {goos: "linux", release: "4.19.128-microsoft-standard",
			said: "WSL's (4.19.128-microsoft-standard)"},
		"WSL 1's kernel": {goos: "linux", release: "4.4.0-19041-Microsoft", said: "WSL's (4.4.0-19041-Microsoft)"},
		"Windows":        {goos: "windows", said: "this host runs windows"},
	}
	for _, mark := range wslMarks {
		cases["a kernel named neither, beside "+mark] = host{goos: "linux", release: "6.6.36-custom",
			said: "WSL's (6.6.36-custom)", stat: marked(mark, nil)}
	}
	for name, c := range cases {
		if c.stat == nil {
			c.stat = unmarked
		}
		got, err := readEngineKernel(c.goos, files(c.release, id), c.stat)
		if !errors.Is(err, ErrNoSharedKernel) || got != "" {
			t.Errorf("%s answers %q, %v; want ErrNoSharedKernel", name, got, err)
			continue
		}
		if !strings.Contains(err.Error(), c.said) || !strings.Contains(err.Error(), "Docker Engine or Podman") {
			t.Errorf("%s says %q, which does not name the host (%q) and what to run", name, err, c.said)
		}
	}
	for name, c := range map[string]struct {
		read func(string) ([]byte, error)
		stat func(string) (fs.FileInfo, error)
	}{
		"an unreadable boot id": {read: files("7.0.0-44-generic", "")},
		"a malformed boot id":   {read: files("7.0.0-44-generic", "not-a-boot-id")},
		"an unreadable release": {read: files("", id)},
		"a mark that cannot be looked for": {read: files("7.0.0-44-generic", id),
			stat: marked(wslMarks[0], syscall.EACCES)},
	} {
		if c.stat == nil {
			c.stat = unmarked
		}
		got, err := readEngineKernel("linux", c.read, c.stat)
		if err == nil || errors.Is(err, ErrNoSharedKernel) || got != "" {
			t.Errorf("%s answers %q, %v; want an error that is not ErrNoSharedKernel", name, got, err)
		}
	}
}
