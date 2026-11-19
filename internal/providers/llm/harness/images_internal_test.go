package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// imageInput is a profile override that takes images the way Claude Code's
// headless run does.
var imageInput = map[string]any{"image_input": map[string]any{
	"args": []any{"--input-format", "stream-json"}, "format": "claude-stream-json",
	"note": "the test's structured input"}}

// AN IMAGE REACHES THE HARNESS BESIDE ITS PROMPT: the call adds the profile's
// input arguments and writes one user message whose content is the prompt's
// text and then each image's bytes, in order, as a base64 block. A harness
// whose profile takes no image refuses the request before anything runs, so
// an image is never silently dropped. Mutation: write the plain transcript
// when images are present, and the echo holds no image block.
func TestAnImageReachesTheHarnessBesideItsPrompt(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1", "FAKE_RECORD_ARGV": rec}, imageInput)
	if !p.ReadsImages() {
		t.Fatal("a profile with image_input does not read images")
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 16)...)
	comp, err := ask(t, p, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "what does the chart show"}},
		Images:   []llm.Image{{MediaType: "image/png", Data: png}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(comp.Content))
	if err != nil {
		t.Fatalf("the echo is not the fake's base64: %v", err)
	}
	var message struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("stdin is not one stream-json message: %v\n%s", err, raw)
	}
	content := message.Message.Content
	if message.Type != "user" || message.Message.Role != "user" || len(content) != 2 ||
		content[0].Type != "text" || !strings.Contains(content[0].Text, "what does the chart show") {
		t.Fatalf("stdin = %s, want the prompt's text then the image", raw)
	}
	if image := content[1]; image.Type != "image" || image.Source.Type != "base64" ||
		image.Source.MediaType != "image/png" ||
		image.Source.Data != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("the image block = %+v", image)
	}
	argvs := recordedArgv(t, rec)
	if len(argvs) != 1 || !slices.Contains(argvs[0], "--input-format") ||
		!slices.Contains(argvs[0], "stream-json") {
		t.Errorf("argv = %q, want the profile's input arguments", argvs)
	}

	none := t.TempDir()
	plain := fakeProvider(t, map[string]string{"FAKE_RECORD_ARGV": none}, nil)
	if plain.ReadsImages() {
		t.Error("a profile with no image_input reads images")
	}
	_, err = ask(t, plain, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "what does the chart show"}},
		Images:   []llm.Image{{MediaType: "image/png", Data: png}},
	})
	if !errors.Is(err, llm.ErrNoImages) {
		t.Errorf("a harness taking no image answered %v, want ErrNoImages", err)
	}
	if got := recordedArgv(t, none); len(got) != 0 {
		t.Errorf("a refused request ran the harness: %q", got)
	}
}
