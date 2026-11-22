package executor

import (
	"context"
	"encoding/json"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// writeJSON renders v, indented, into file.
func writeJSON(ctx context.Context, box sandbox.Sandbox, file string, v any) error {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return box.WriteFile(ctx, file, blob)
}
