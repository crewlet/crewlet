package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// THE CODEX RENEW ADAPTER hands a continued Codex session this launch's
// standing instructions, by rewriting the item its rollout replays them from.
// Measured on 0.171.0 through the app-server and `exec --remote … resume`,
// with a stand-in model: config.toml rewritten with the new text, and
// thread/resume with developerInstructions set, each left the resumed request
// carrying the first launch's text alone; renewed here, it carried the new
// text alone, the history kept. The package doc's "A continued Codex
// session's standing instructions" says why.

// renewConfig is what -config names for codex-renew: this launch's standing
// instructions, as the run's config.toml hands them to Codex.
type renewConfig struct {
	Instructions string `json:"instructions"`
}

// codexInstructionsKind is the content kind Codex 0.171.0 records the
// client's developer_instructions under, in the message's
// internal_chat_message_metadata_passthrough.content_item_kinds, one kind per
// content item. A requirements policy's are `managed_config.developer_
// instructions`, Codex's own to replace, and are left alone.
const codexInstructionsKind = "generic.developer_instructions"

// runCodexRenew renews the rollout -rollout names to the instructions -config
// holds. Exit 0 once renewed, 1 where it could not be, the file left as it was.
func runCodexRenew(_ context.Context, f flags, _ io.Reader, _, stderr io.Writer, _ func(string) string) int {
	err := errors.New("-rollout names no rollout")
	if f.rollout != "" {
		err = codexRenew(f.config, f.rollout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: renewing the session's standing instructions: %v\n", err)
		return exitError
	}
	return exitDone
}

// codexRenew rewrites rollout's standing instructions to config's, line for
// line, by a rename over it, so a box that holds the rollout holds it whole.
func codexRenew(config, rollout string) error {
	raw, err := os.ReadFile(config) //nolint:gosec // the run's own config, written by its launch
	if err != nil {
		return err
	}
	var c renewConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("the renew config: %w", err)
	}
	info, err := os.Stat(rollout)
	if err != nil {
		return err
	}
	in, err := os.Open(rollout) //nolint:gosec // the session's own rollout
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(rollout), ".renew-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(out.Name()) }()
	renewed, err := renewLines(bufio.NewReader(in), out, c.Instructions)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case renewed == 0:
		return errors.New("the rollout records no standing instructions of the client's to renew, and resumed " +
			"it would hand the model none")
	}
	if err = os.Chmod(out.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(out.Name(), rollout)
}

// renewLines copies r to w, each line holding a standing-instructions item
// rewritten to text and every other byte for byte, and is how many items it
// renewed. A line that does not decode is copied as it is: Codex passes a
// fragment over, so its words reach no model.
func renewLines(r *bufio.Reader, w io.Writer, text string) (int, error) {
	kind := []byte(`"` + codexInstructionsKind + `"`)
	total := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			body, end := bytes.CutSuffix(line, []byte("\n"))
			if bytes.Contains(body, kind) {
				var renewed int
				body, renewed, err = renewLine(body, text)
				if err != nil {
					return total, err
				}
				total += renewed
			}
			if _, werr := w.Write(body); werr != nil {
				return total, werr
			}
			if end {
				if _, werr := w.Write([]byte("\n")); werr != nil {
					return total, werr
				}
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return total, nil
		case err != nil:
			return total, err
		}
	}
}

// renewLine is line with every standing-instructions item in it renewed to
// text, and how many it renewed; line itself where it does not decode or
// holds none. Numbers stay as written (UseNumber) and <, > and & unescaped,
// so only the renewed words change.
func renewLine(line []byte, text string) ([]byte, int, error) {
	v, ok := decodeLine(line)
	if !ok {
		return line, 0, nil
	}
	renewed, err := renewIn(v, text)
	if err != nil || renewed == 0 {
		return line, 0, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(v); err != nil {
		return line, 0, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), renewed, nil
}

// decodeLine is line's JSON value, numbers as written, and false where it
// does not decode: not a failure of the renew, since Codex passes such a
// fragment over and its words reach no model ([renewLines]).
func decodeLine(line []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// renewIn renews every developer message within v, at any depth: a rollout
// line's payload, and a compaction's replacement history.
func renewIn(v any, text string) (int, error) {
	total := 0
	switch v := v.(type) {
	case map[string]any:
		n, err := renewMessage(v, text)
		if err != nil {
			return 0, err
		}
		total += n
		for _, child := range v {
			n, err = renewIn(child, text)
			if err != nil {
				return 0, err
			}
			total += n
		}
	case []any:
		for _, child := range v {
			n, err := renewIn(child, text)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// renewMessage renews m where it is a developer message: each content item
// its metadata's content_item_kinds names [codexInstructionsKind] gets text,
// or, for a run with none, is removed with its kind and metadata. A message
// whose kinds do not pair with its content one for one is refused: which item
// is which cannot be told.
func renewMessage(m map[string]any, text string) (int, error) {
	if m["type"] != "message" || m["role"] != "developer" {
		return 0, nil
	}
	meta, _ := m["internal_chat_message_metadata_passthrough"].(map[string]any)
	kinds, _ := meta["content_item_kinds"].([]any)
	content, _ := m["content"].([]any)
	if !containsKind(kinds) {
		return 0, nil
	}
	if len(kinds) != len(content) {
		return 0, fmt.Errorf("a developer message names %d content kinds for %d items", len(kinds), len(content))
	}
	itemMeta, _ := meta["content_item_metadata"].([]any)
	pairedMeta := len(itemMeta) == len(content)
	renewed := 0
	var keptContent, keptKinds, keptMeta []any
	for i, item := range content {
		if kinds[i] != codexInstructionsKind {
			keptContent, keptKinds = append(keptContent, item), append(keptKinds, kinds[i])
			if pairedMeta {
				keptMeta = append(keptMeta, itemMeta[i])
			}
			continue
		}
		renewed++
		if text == "" {
			continue
		}
		block, ok := item.(map[string]any)
		if _, isText := block["text"].(string); !ok || !isText {
			return 0, errors.New("a standing-instructions item holds no text")
		}
		block["text"] = text
		keptContent, keptKinds = append(keptContent, block), append(keptKinds, kinds[i])
		if pairedMeta {
			keptMeta = append(keptMeta, itemMeta[i])
		}
	}
	if len(keptContent) == 0 {
		return 0, errors.New("removing the standing instructions would leave a developer message empty")
	}
	m["content"] = keptContent
	meta["content_item_kinds"] = keptKinds
	if pairedMeta {
		meta["content_item_metadata"] = keptMeta
	}
	return renewed, nil
}

// containsKind reports whether kinds names [codexInstructionsKind].
func containsKind(kinds []any) bool {
	for _, k := range kinds {
		if k == codexInstructionsKind {
			return true
		}
	}
	return false
}
