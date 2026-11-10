package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// A FRESH BOX IS READ ONCE, after its setup and before its job: the memory the
// kernel holds it to, whether its image has a browser, and whether a container
// runtime answers in it. Its seat is told what was found on every turn after
// (ADR-0104), since a seat left to infer its box from one failed command keeps
// the wrong conclusion as a note. The answer is read into closed values only:
// nothing the box prints reaches a prompt as text.

// BoxFacts is what a box was found to have.
type BoxFacts struct {
	// MemoryBytes is the memory the box is held to, 0 where it could not be read.
	MemoryBytes int64
	// Browser and ContainerRuntime name what the box has, from
	// [coord.BoxBrowsers] and [coord.BoxContainerRuntimes]; empty for none.
	Browser          string
	ContainerRuntime string
}

// boxFactsTimeout bounds the whole read: each runtime's `info` is held to five
// seconds where the box has `timeout`, and the rest is a handful of reads.
const boxFactsTimeout = 20

// boxFactsScript prints one `key=value` line per fact it finds and always exits
// 0: a box without a fact is an answer, not a failure.
var boxFactsScript = strings.Join([]string{
	`for b in ` + strings.Join(coord.BoxBrowsers, " ") + `; do if command -v "$b" >/dev/null 2>&1; then echo "browser=$b"; break; fi; done`,
	`for d in "${PLAYWRIGHT_BROWSERS_PATH:-}" /ms-playwright "$HOME/.cache/ms-playwright"; do if [ -n "$d" ] && ls -d "$d"/chromium-* >/dev/null 2>&1; then echo "playwright=chromium"; break; fi; done`,
	`t=""; if command -v timeout >/dev/null 2>&1; then t="timeout 5"; fi`,
	`for r in ` + strings.Join(coord.BoxContainerRuntimes, " ") + `; do if command -v "$r" >/dev/null 2>&1 && $t "$r" info >/dev/null 2>&1; then echo "runtime=$r"; break; fi; done`,
	`if [ -r /sys/fs/cgroup/memory.max ]; then echo "cgroup=$(cat /sys/fs/cgroup/memory.max)"; elif [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then echo "cgroup=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)"; fi`,
	`if [ -r /proc/meminfo ]; then awk '/^MemTotal:/ {print "total_kb=" $2}' /proc/meminfo; elif command -v sysctl >/dev/null 2>&1; then echo "total=$(sysctl -n hw.memsize 2>/dev/null)"; fi`,
	`exit 0`,
}, "\n")

// errBoxFactsEmpty is a box that answered the read with nothing it knows.
var errBoxFactsEmpty = errors.New("sandbox: the box answered its read with nothing")

// ReadBoxFacts reads box once. An error is a read that did not run or said nothing;
// the box itself is no worse for it.
func ReadBoxFacts(ctx context.Context, box Sandbox) (BoxFacts, error) {
	res, err := box.Exec(ctx, boxFactsScript, ExecOptions{TimeoutSec: boxFactsTimeout})
	if err != nil {
		return BoxFacts{}, fmt.Errorf("sandbox: read the box: %w", err)
	}
	if res.ExitCode != 0 {
		return BoxFacts{}, fmt.Errorf("sandbox: read the box: its shell exited %d", res.ExitCode)
	}
	return parseBoxFacts(res.Stdout)
}

// parseBoxFacts is the facts in out, each kept only where its value is one
// this build knows; the memory is the smaller of the cgroup's limit and the
// machine's.
func parseBoxFacts(out string) (BoxFacts, error) {
	var read BoxFacts
	var cgroup, total int64
	var playwright, said bool
	for line := range strings.Lines(out) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "browser":
			if slices.Contains(coord.BoxBrowsers, value) {
				read.Browser, said = value, true
			}
		case "playwright":
			playwright, said = value == "chromium", said || value == "chromium"
		case "runtime":
			if slices.Contains(coord.BoxContainerRuntimes, value) {
				read.ContainerRuntime, said = value, true
			}
		case "cgroup":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				cgroup, said = n, true
			}
		case "total_kb":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 && n < 1<<53 {
				total, said = n<<10, true
			}
		case "total":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				total, said = n, true
			}
		}
	}
	if !said {
		return BoxFacts{}, errBoxFactsEmpty
	}
	if read.Browser == "" && playwright {
		read.Browser = coord.BoxPlaywrightChromium
	}
	switch {
	case cgroup > 0 && (total == 0 || cgroup < total):
		read.MemoryBytes = cgroup
	default:
		read.MemoryBytes = total
	}
	return read, nil
}

// readFresh is a fresh box's read, nil where it did not answer: the launch
// goes on, and the seat is told what the document says of its box.
func readFresh(ctx context.Context, box Sandbox) *BoxFacts {
	read, err := ReadBoxFacts(ctx, box)
	if err != nil {
		log.WarnContext(ctx, "sandbox_box_unread", "sandbox_id", box.ID(), "error", err.Error(),
			"detail", "the seat is told its box from the document until a run reads it")
		return nil
	}
	return &read
}
