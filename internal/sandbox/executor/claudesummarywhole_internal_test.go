package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// THE WHOLE-LINE READ OF A CLAUDE CODE SUMMARY'S LINE is the oracle the
// streamed read ([claudeSummaryLine]) is held to: the line read whole off the
// box's shell (`tail -c +N file | head -c size`), checked by the walk's reader
// and decoded again, as the read was before it streamed.

// claudeSummaryLineWhole is the summary the line of file at offset at, size
// bytes with its break, holds: read in the box alone, checked by the walk's own
// reader to be a summary's record, and its message's content, a string.
func claudeSummaryLineWhole(ctx context.Context, box sandbox.Sandbox, file string, at, size int64) (string, error) {
	if size > claudeSummaryLineMaxBytes {
		return "", errSummaryPast
	}
	res, err := box.Exec(ctx, fmt.Sprintf("tail -c +%d %s | head -c %d", at+1, shellword.Quote(file), size),
		sandbox.ExecOptions{})
	switch {
	case err != nil:
		return "", fmt.Errorf("the summary's line could not be read: %w", err)
	case res.ExitCode != 0:
		return "", fmt.Errorf("the summary's line could not be read: %s", firstLine(res.Stderr,
			transcriptDetailLimit))
	case int64(len(res.Stdout)) != size || !strings.HasSuffix(res.Stdout, "\n"):
		return "", errors.New("the summary's line is not where the walk read it: the thread changed under the read")
	}
	l, err := newThreadReader(strings.NewReader(res.Stdout), size).Next(ctx)
	if err != nil {
		return "", fmt.Errorf("the summary's line no longer reads as a record: %w", err)
	}
	summary, _ := l.IsCompactSummary()
	if meta, _ := l.IsMeta(); l.Head.Type != recordUser || !summary || meta {
		return "", errors.New("the summary's line is no longer a summary's record: the thread changed under the read")
	}
	var rec struct {
		Message json.RawMessage `json:"message"`
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	var text string
	line := []byte(strings.TrimSuffix(res.Stdout, "\n"))
	if wireAlias(line, &rec) != nil || !wireObject(rec.Message) || wireAlias(rec.Message, &msg) != nil ||
		len(msg.Content) == 0 || msg.Content[0] != '"' || wireDecode(msg.Content, &text) != nil {
		return "", errors.New("the summary's content is no text, the one shape this read holds")
	}
	if len(text) > summaryReadMaxBytes {
		return "", errSummaryPast
	}
	return text, nil
}
