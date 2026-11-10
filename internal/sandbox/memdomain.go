package sandbox

import (
	"strconv"
	"strings"
)

// A JOB'S MEMORY DOMAIN is the memory accounting the kernel holds its
// processes to: the cgroup the calling process is in (v2, then v1), else the
// machine, where a VM is a box of its own. One shell function reads it, for a
// box's read and around every job, so both name the same domain.

// MemoryDomainScript defines the shell function crewlet_memory, which prints
// the caller's memory domain as `key=value` lines: oom_kill (the kernel's count
// of processes it killed for memory), limit (bytes, or `max`) and the
// machine's total (total_kb from /proc/meminfo, total in bytes from sysctl).
const MemoryDomainScript = `crewlet_memory() {
d=""; c=$(sed -n 's/^0::\(.*\)$/\1/p' /proc/self/cgroup 2>/dev/null)
for x in "/sys/fs/cgroup${c%/}" /sys/fs/cgroup; do if [ -r "$x/memory.events" ]; then d=$x; break; fi; done
if [ -n "$d" ]; then awk '$1=="oom_kill"{print "oom_kill=" $2}' "$d/memory.events"; [ -r "$d/memory.max" ] && echo "limit=$(cat "$d/memory.max")"
elif [ -r /sys/fs/cgroup/memory/memory.oom_control ]; then awk '$1=="oom_kill"{print "oom_kill=" $2}' /sys/fs/cgroup/memory/memory.oom_control; echo "limit=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null)"
elif [ -r /proc/vmstat ]; then awk '$1=="oom_kill"{print "oom_kill=" $2}' /proc/vmstat; fi
if [ -r /proc/meminfo ]; then awk '$1=="MemTotal:"{print "total_kb=" $2}' /proc/meminfo; elif command -v sysctl >/dev/null 2>&1; then echo "total=$(sysctl -n hw.memsize 2>/dev/null)"; fi
}`

// MemoryDomain is a memory domain as [MemoryDomainScript] printed it.
type MemoryDomain struct {
	// OOMKills is the kernel's count of processes it killed for memory in the
	// domain, -1 where the domain keeps none.
	OOMKills int64
	// LimitBytes is the domain's own limit and TotalBytes the machine's, each
	// 0 where unread or unlimited.
	LimitBytes, TotalBytes int64
}

// Ceiling is the memory the domain is held to: the smaller of its own limit and
// the machine's, 0 where neither was read.
func (m MemoryDomain) Ceiling() int64 {
	if m.LimitBytes > 0 && (m.TotalBytes == 0 || m.LimitBytes < m.TotalBytes) {
		return m.LimitBytes
	}
	return m.TotalBytes
}

// Known reports whether the read said anything of the domain.
func (m MemoryDomain) Known() bool { return m.OOMKills >= 0 || m.Ceiling() > 0 }

// ParseMemoryDomain reads the lines out holds, as [MemoryDomainScript] prints
// them; a value that is no count is unread.
func ParseMemoryDomain(out string) MemoryDomain {
	m := MemoryDomain{OOMKills: -1}
	for line := range strings.Lines(out) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch key {
		case "oom_kill":
			m.OOMKills = n
		case "limit":
			m.LimitBytes = n
		case "total_kb":
			if n < 1<<53 {
				m.TotalBytes = n << 10
			}
		case "total":
			m.TotalBytes = n
		}
	}
	return m
}

// KilledBetween is how many processes the kernel killed for memory in the
// domain from before to after, false where either did not count them or the
// count went back (another domain, or a reset).
func KilledBetween(before, after MemoryDomain) (int64, bool) {
	if before.OOMKills < 0 || after.OOMKills < before.OOMKills {
		return 0, false
	}
	return after.OOMKills - before.OOMKills, true
}
