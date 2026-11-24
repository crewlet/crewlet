package memory

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// holdRecord refuses v past bound as the KV backend refuses its value at key,
// encoded as that backend stores it ([coord.HoldValue]): the class the
// positions register declares for the key (internal/coord/kv's holding).
func holdRecord(key string, v any, bound int) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("coord/memory: encode %s: %w", key, err)
	}
	return coord.HoldValue(key, len(raw), bound)
}
