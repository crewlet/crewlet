package expo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
)

// A PASTED TOKEN IS A SECRET IN TRANSIT: refused or accepted, it reaches the
// sealed store and nothing else, no log line, no ledger record, no finding, no
// sentence a person reads. Not parallel: it captures the process's logs.
func TestAPastedExpoTokenIsKeptOnlyInTheSealedStore(t *testing.T) {
	var logs bytes.Buffer
	logging.Configure(slog.LevelDebug, logging.FormatText, &logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })

	w := newSyncWorld(t, "")
	var seen []string
	note := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, string(b))
	}
	person := w.expo.person("Avery", expo.RoleViewOnly)
	_, refusedErr := w.paste(person.value)
	if refusedErr == nil {
		t.Fatal("a person's token was accepted")
	}
	seen = append(seen, refusedErr.Error())

	robot, tok := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	handed, err := w.paste(tok.value)
	if err != nil {
		t.Fatal(err)
	}
	note(handed)
	w.expo.mint(robot.id)
	note(w.sync())
	off := false
	w.seat().Enabled = &off
	note(w.sync())
	records, err := w.ledger.Records(context.Background(), integration.KindExpo)
	if err != nil {
		t.Fatal(err)
	}
	note(records)
	note(w.drift)
	seen = append(seen, logs.String())

	for _, value := range []string{person.value, tok.value} {
		for _, text := range seen {
			if strings.Contains(text, value) {
				t.Errorf("a pasted token appears outside the sealed store: %s", text)
			}
		}
	}
	if retired, _ := w.sink.Held(retiredQA); retired != tok.value {
		t.Error("the sealed store does not hold the turned-off token, so this test saw nothing")
	}
	if handed.Label == "" || !strings.HasPrefix(tok.value, strings.TrimSuffix(handed.Label, "…")) {
		t.Errorf("the audit label %q is not what Expo shows of the token", handed.Label)
	}
}
