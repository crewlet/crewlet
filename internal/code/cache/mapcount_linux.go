package cache

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// mapCountFile is where Linux says how many mappings a process may hold.
const mapCountFile = "/proc/sys/vm/max_map_count"

// readMapCount is how many mappings this process may hold — every one it
// makes, the runtime's and every library's among them (runmaps.go) — as the
// running kernel says: vm.max_map_count, one value for the whole node.
func readMapCount() (int, error) {
	raw, err := os.ReadFile(mapCountFile)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", mapCountFile, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s holds %q, which is no count of mappings", mapCountFile, strings.TrimSpace(string(raw)))
	}
	return n, nil
}
