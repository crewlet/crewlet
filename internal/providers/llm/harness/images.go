package harness

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// imageMessage is the structured input that hands a harness its prompt and
// images in format: for Claude Code's stream-json, one user message whose
// content is the prompt's text block and then one base64 block per image.
func imageMessage(format harnessprofile.ImageFormat, prompt string, images []llm.Image) (string, error) {
	if format != harnessprofile.ImageClaudeStreamJSON {
		return "", fmt.Errorf("harness: image_input format %q is not one this build writes", format)
	}
	type source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type block struct {
		Type   string  `json:"type"`
		Text   string  `json:"text,omitempty"`
		Source *source `json:"source,omitempty"`
	}
	content := make([]block, 0, len(images)+1)
	content = append(content, block{Type: "text", Text: prompt})
	for _, image := range images {
		content = append(content, block{Type: "image", Source: &source{Type: "base64",
			MediaType: image.MediaType, Data: base64.StdEncoding.EncodeToString(image.Data)}})
	}
	line, err := json.Marshal(map[string]any{"type": "user",
		"message": map[string]any{"role": "user", "content": content}})
	if err != nil {
		return "", fmt.Errorf("harness: the image message: %w", err)
	}
	return string(line) + "\n", nil
}
