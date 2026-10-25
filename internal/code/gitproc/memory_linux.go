package gitproc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

// readNodeMemory reads this node's memory from the running system's /proc and
// cgroup filesystems (readLinuxMemory).
func readNodeMemory() (Memory, error) { return readLinuxMemory(os.DirFS("/")) }

// readLinuxMemory reads the memory a process under root may hold: the least of
// the machine's (/proc/meminfo's MemTotal) and every limit the process's
// cgroups set, at its own cgroup or any above it that the mount shows — v2's
// memory.max and v1's memory.limit_in_bytes alike, since a host still on the
// v1 hierarchy shows a container's limit nowhere else. root is "/" in
// production and a fixture tree in the suite.
//
// FROM THE FILES, NOT A CALL: sysinfo(2) answers the machine's memory and
// knows nothing of a container's limit, which is the figure that decides
// whether a run is killed.
func readLinuxMemory(root fs.FS) (Memory, error) {
	physical, err := memTotal(root)
	if err != nil {
		return Memory{}, err
	}
	m := Memory{Bytes: physical, From: "/proc/meminfo"}
	limits, err := cgroupLimits(root)
	if err != nil {
		return Memory{}, err
	}
	for _, l := range limits {
		if l.Bytes < m.Bytes {
			m = l
		}
	}
	return m, nil
}

// memTotal is /proc/meminfo's MemTotal, in bytes.
func memTotal(root fs.FS) (int64, error) {
	data, err := fs.ReadFile(root, "proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("reading /proc/meminfo: %w", err)
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	for scan.Scan() {
		name, value, ok := strings.Cut(scan.Text(), ":")
		if !ok || name != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("/proc/meminfo's MemTotal %q is not a count of kB", strings.TrimSpace(value))
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 || kb > math.MaxInt64>>10 {
			return 0, fmt.Errorf("/proc/meminfo's MemTotal %q is not a count of kB", strings.TrimSpace(value))
		}
		return kb << 10, nil
	}
	return 0, errors.New("/proc/meminfo has no MemTotal line")
}

// The two cgroup hierarchies' memory limit files, each at every cgroup but a
// hierarchy's root.
const (
	cgroupV2Limit = "memory.max"
	cgroupV1Limit = "memory.limit_in_bytes"
)

// cgroupLimits is every memory limit set on the process's cgroups or above
// them, each with the file that sets it: none on a kernel without cgroups
// (no /proc/self/cgroup) or where no hierarchy carrying the memory controller
// is mounted, since then nothing the process can see limits it.
func cgroupLimits(root fs.FS) ([]Memory, error) {
	data, err := fs.ReadFile(root, "proc/self/cgroup")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading /proc/self/cgroup: %w", err)
	}
	var v2, v1 string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, ":")
		controllers, cgroup, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 || !strings.HasPrefix(cgroup, "/") {
			return nil, fmt.Errorf("/proc/self/cgroup's line %q is not hierarchy:controllers:path", line)
		}
		switch {
		case id == "0" && controllers == "":
			v2 = cgroup
		case slices.Contains(strings.Split(controllers, ","), "memory"):
			v1 = cgroup
		}
	}
	if v2 == "" && v1 == "" {
		return nil, nil
	}
	mounts, err := cgroupMounts(root)
	if err != nil {
		return nil, err
	}
	var out []Memory
	for _, m := range mounts {
		cgroup, file := v2, cgroupV2Limit
		if !m.v2 {
			cgroup, file = v1, cgroupV1Limit
		}
		if cgroup == "" {
			continue
		}
		limit, found, err := cgroupLimit(root, m, cgroup, file)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, limit)
		}
	}
	return out, nil
}

// cgroupMount is one mount of a cgroup hierarchy that can carry a memory
// limit: the v2 hierarchy, or a v1 one with the memory controller. root is
// the cgroup the mount shows at its point.
type cgroupMount struct {
	root, point string
	v2          bool
}

// cgroupMounts is every mount /proc/self/mountinfo lists of a hierarchy that
// can carry a memory limit.
func cgroupMounts(root fs.FS) ([]cgroupMount, error) {
	data, err := fs.ReadFile(root, "proc/self/mountinfo")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading /proc/self/mountinfo: %w", err)
	}
	var out []cgroupMount
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		// id parent major:minor root point options [optional...] - type source super
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+4 {
			return nil, fmt.Errorf("/proc/self/mountinfo's line %q is not a mount", line)
		}
		kind, super := fields[sep+1], fields[sep+3]
		m := cgroupMount{root: unescapeMount(fields[3]), point: unescapeMount(fields[4])}
		switch {
		case kind == "cgroup2":
			m.v2 = true
		case kind == "cgroup" && slices.Contains(strings.Split(super, ","), "memory"):
		default:
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// unescapeMount undoes the kernel's escaping of a path in mountinfo: a space,
// a tab, a newline and a backslash each written as a backslash and three
// octal digits.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// cgroupLimit is the least limit file sets on cgroup or any cgroup above it
// that mount m shows, and whether any does. A cgroup m does not show — one
// outside the cgroup at its root, as a process outside a namespace's root
// sees it — has nothing m can say. A file that is absent is a cgroup with no
// limit of its own (a hierarchy's root holds none); "max" is v2's no limit,
// and v1's is a count past any machine's memory, which the least of them
// leaves out by itself.
func cgroupLimit(root fs.FS, m cgroupMount, cgroup, file string) (Memory, bool, error) {
	rel, ok := within(cgroup, m.root)
	if !ok {
		return Memory{}, false, nil
	}
	least, found := Memory{Bytes: math.MaxInt64}, false
	for dir := path.Join(m.point, rel); ; dir = path.Dir(dir) {
		name := path.Join(dir, file)
		data, err := fs.ReadFile(root, strings.TrimPrefix(name, "/"))
		value := strings.TrimSpace(string(data))
		switch {
		case errors.Is(err, fs.ErrNotExist), err == nil && value == "max":
		case err != nil:
			return Memory{}, false, fmt.Errorf("reading %s: %w", name, err)
		default:
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return Memory{}, false, fmt.Errorf("%s holds %q, not a count of bytes", name, value)
			}
			if b := int64(min(n, math.MaxInt64)); b < least.Bytes {
				least, found = Memory{Bytes: b, From: name}, true
			}
		}
		if dir == m.point || dir == "/" {
			return least, found, nil
		}
	}
}

// within is cgroup's path below the cgroup root shows — "/" for root itself —
// and whether cgroup is root or below it.
func within(cgroup, root string) (string, bool) {
	cgroup, root = path.Clean(cgroup), path.Clean(root)
	switch {
	case root == "/":
		return cgroup, true
	case cgroup == root:
		return "/", true
	case strings.HasPrefix(cgroup, root+"/"):
		return cgroup[len(root):], true
	}
	return "", false
}
