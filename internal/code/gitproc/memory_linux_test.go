package gitproc_test

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE NODE'S MEMORY IS THE LEAST OF WHAT LIMITS THE PROCESS: the machine's,
// and every cgroup limit on the process's cgroup or above it that a mount
// shows — v2's memory.max and v1's memory.limit_in_bytes alike, since a host
// still on the v1 hierarchy (the machine this suite was written on among
// them) shows a container's limit nowhere else. Each case is a fixture tree
// of the files the kernel writes, the way the kernel writes them.
func TestTheNodesMemoryIsTheLeastOfWhatLimitsIt(t *testing.T) {
	t.Parallel()
	const meminfo = "MemTotal:       16479424 kB\nMemFree:         6601724 kB\n"
	const physical = 16479424 << 10
	v1Mounts := "41 37 0:33 / /sys/fs/cgroup/memory rw,relatime - cgroup cgroup rw,memory\n" +
		"38 37 0:30 / /sys/fs/cgroup/cpu rw,relatime - cgroup cgroup rw,cpu\n" +
		"47 37 0:39 / /sys/fs/cgroup/unified rw,relatime - cgroup2 cgroup2 rw\n"
	v2Mount := "30 23 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate\n"
	for name, tc := range map[string]struct {
		// self is /proc/self's cgroup and mountinfo, by name; limits the
		// cgroup filesystem's files, by path. /proc/meminfo is every case's.
		self   map[string]string
		limits map[string]string
		bytes  int64
		from   string
	}{
		"no cgroups at all": {
			bytes: physical, from: "/proc/meminfo",
		},
		"a v2 limit two levels up": {
			self: map[string]string{
				"cgroup":    "0::/kubepods/pod1/c1\n",
				"mountinfo": "22 1 8:1 / / rw - ext4 /dev/root rw\n" + v2Mount,
			},
			limits: map[string]string{
				"sys/fs/cgroup/kubepods/pod1/c1/memory.max": "max\n",
				"sys/fs/cgroup/kubepods/pod1/memory.max":    "2147483648\n",
				"sys/fs/cgroup/kubepods/memory.max":         "max\n",
			},
			bytes: 2 << 30, from: "/sys/fs/cgroup/kubepods/pod1/memory.max",
		},
		"a v2 limit at a namespace's root": {
			self: map[string]string{"cgroup": "0::/\n", "mountinfo": v2Mount},
			limits: map[string]string{
				"sys/fs/cgroup/memory.max":   "1073741824\n",
				"sys/fs/cgroup/memory.peak":  "1\n",
				"sys/fs/cgroup/cgroup.procs": "1\n",
			},
			bytes: 1 << 30, from: "/sys/fs/cgroup/memory.max",
		},
		"the least of two v2 limits": {
			self: map[string]string{"cgroup": "0::/a/b\n", "mountinfo": v2Mount},
			limits: map[string]string{
				"sys/fs/cgroup/a/b/memory.max": "3221225472\n",
				"sys/fs/cgroup/a/memory.max":   "4294967296\n",
			},
			bytes: 3 << 30, from: "/sys/fs/cgroup/a/b/memory.max",
		},
		"no v2 limit anywhere": {
			self: map[string]string{"cgroup": "0::/system.slice/crewlet.service\n", "mountinfo": v2Mount},
			limits: map[string]string{
				"sys/fs/cgroup/system.slice/crewlet.service/memory.max": "max\n",
				"sys/fs/cgroup/system.slice/memory.max":                 "max\n",
			},
			bytes: physical, from: "/proc/meminfo",
		},
		"a limit past the machine": {
			self:   map[string]string{"cgroup": "0::/big\n", "mountinfo": v2Mount},
			limits: map[string]string{"sys/fs/cgroup/big/memory.max": "68719476736\n"},
			bytes:  physical, from: "/proc/meminfo",
		},
		"a v1 limit, as a hybrid host shows it": {
			self: map[string]string{
				"cgroup": "9:name=systemd:/\n8:pids:/\n4:memory:/process_api/x/sandbox\n" +
					"2:cpuacct:/process_api/x/shell\n1:cpu:/\n0::/\n",
				"mountinfo": v1Mounts,
			},
			limits: map[string]string{
				"sys/fs/cgroup/memory/process_api/x/sandbox/memory.limit_in_bytes": "14343688192\n",
				"sys/fs/cgroup/memory/process_api/x/memory.limit_in_bytes":         "9223372036854771712\n",
				"sys/fs/cgroup/memory/process_api/memory.limit_in_bytes":           "9223372036854771712\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes":                       "9223372036854771712\n",
			},
			bytes: 14343688192, from: "/sys/fs/cgroup/memory/process_api/x/sandbox/memory.limit_in_bytes",
		},
		"a v1 controller among others on one hierarchy": {
			self: map[string]string{
				"cgroup":    "3:cpu,memory:/docker/abc\n",
				"mountinfo": "40 37 0:33 /docker/abc /sys/fs/cgroup/cpu,memory ro - cgroup cgroup rw,cpu,memory\n",
			},
			limits: map[string]string{"sys/fs/cgroup/cpu,memory/memory.limit_in_bytes": "536870912\n"},
			bytes:  512 << 20, from: "/sys/fs/cgroup/cpu,memory/memory.limit_in_bytes",
		},
		"a mount showing a cgroup above the process's": {
			self: map[string]string{
				"cgroup":    "0::/docker/abc/worker\n",
				"mountinfo": "30 23 0:26 /docker/abc /sys/fs/cgroup ro - cgroup2 cgroup2 rw\n",
			},
			limits: map[string]string{
				"sys/fs/cgroup/worker/memory.max": "max\n",
				"sys/fs/cgroup/memory.max":        "805306368\n",
			},
			bytes: 768 << 20, from: "/sys/fs/cgroup/memory.max",
		},
		"a mount that does not show the process's cgroup": {
			self: map[string]string{
				"cgroup":    "0::/elsewhere\n",
				"mountinfo": "30 23 0:26 /docker/abc /sys/fs/cgroup ro - cgroup2 cgroup2 rw\n",
			},
			limits: map[string]string{"sys/fs/cgroup/memory.max": "805306368\n"},
			bytes:  physical, from: "/proc/meminfo",
		},
		"a mount point the kernel escaped": {
			self: map[string]string{
				"cgroup":    "0::/\n",
				"mountinfo": `30 23 0:26 / /mnt/cgroup\040two rw - cgroup2 cgroup2 rw` + "\n",
			},
			limits: map[string]string{"mnt/cgroup two/memory.max": "1073741824\n"},
			bytes:  1 << 30, from: "/mnt/cgroup two/memory.max",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"proc/meminfo": meminfo}
			for name, text := range tc.self {
				files["proc/self/"+name] = text
			}
			for name, text := range tc.limits {
				files[name] = text
			}
			bytes, from, err := gitproc.ReadLinuxMemory(tree(files))
			if err != nil {
				t.Fatalf("reading the node's memory: %v", err)
			}
			if bytes != tc.bytes || from != tc.from {
				t.Errorf("read %d bytes from %s, want %d from %s", bytes, from, tc.bytes, tc.from)
			}
		})
	}
}

// A MEMORY THAT CANNOT BE READ IS AN ERROR NAMING WHAT, never a guess: a
// budget guessed from nothing is the OOM the budget exists to prevent.
func TestAnUnreadableMemoryNamesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	const meminfo = "MemTotal:       16479424 kB\n"
	v2Mount := "30 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n"
	for name, tc := range map[string]struct {
		files map[string]string
		names string
	}{
		"no meminfo": {
			files: map[string]string{"proc/self/cgroup": "0::/\n"},
			names: "/proc/meminfo",
		},
		"a meminfo with no MemTotal": {
			files: map[string]string{"proc/meminfo": "MemFree: 1 kB\n"},
			names: "MemTotal",
		},
		"a MemTotal in another unit": {
			files: map[string]string{"proc/meminfo": "MemTotal: 16 GB\n"},
			names: "MemTotal",
		},
		"a limit that is not a count": {
			files: map[string]string{
				"proc/meminfo":             meminfo,
				"proc/self/cgroup":         "0::/\n",
				"proc/self/mountinfo":      v2Mount,
				"sys/fs/cgroup/memory.max": "lots\n",
			},
			names: "/sys/fs/cgroup/memory.max",
		},
		"a cgroup line the kernel never writes": {
			files: map[string]string{"proc/meminfo": meminfo, "proc/self/cgroup": "nonsense\n"},
			names: "/proc/self/cgroup",
		},
		"a mount line the kernel never writes": {
			files: map[string]string{
				"proc/meminfo":        meminfo,
				"proc/self/cgroup":    "0::/\n",
				"proc/self/mountinfo": "30 23 0:26 /\n",
			},
			names: "/proc/self/mountinfo",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := gitproc.ReadLinuxMemory(tree(tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("read the memory with %v, want an error naming %s", err, tc.names)
			}
		})
	}
}

// THE MACHINE THE SUITE RUNS ON READS: a positive memory no larger than its
// /proc/meminfo, and named — the production reader, over the real files.
func TestThisMachinesMemoryReads(t *testing.T) {
	t.Parallel()
	bytes, from, err := gitproc.ReadLinuxMemory(os.DirFS("/"))
	if err != nil {
		t.Fatalf("reading this machine's memory: %v", err)
	}
	physical, _, err := gitproc.ReadLinuxMemory(tree(map[string]string{"proc/meminfo": readFile(t, "/proc/meminfo")}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes <= 0 || bytes > physical || from == "" {
		t.Errorf("this machine's memory read as %d bytes from %q, want a positive count no larger than "+
			"its /proc/meminfo's %d, and where it came from", bytes, from, physical)
	}
}

// tree is a filesystem of files, each at its path with its text.
func tree(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, text := range files {
		m[name] = &fstest.MapFile{Data: []byte(text)}
	}
	return m
}

// readFile is the text of the file at path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s does not exist on a Linux machine with /proc mounted", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
