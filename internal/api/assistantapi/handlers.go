package assistantapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// maxListLimit is the most conversations one listing answers, the domain's.
const maxListLimit = 50

// list serves GET /assistant/conversations.
func (s *Surface) list(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				httpjson.Detail{"detail": fmt.Sprintf("limit takes 1 to %d conversations",
					maxListLimit)})
			return
		}
		limit = n
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	list, err := s.svc.Conversations(r.Context(), caller, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, list)
}

// create serves POST /assistant/conversations. Its Idempotency-Key, where one
// is sent, names the conversation, so a retried create is one conversation.
func (s *Surface) create(w http.ResponseWriter, r *http.Request) {
	var body wire.Versioned
	if !decode(w, r, &body, func() int { return body.ProtocolVersion }) {
		return
	}
	key, ok := opkey.Key(w, r, s.now())
	if !ok {
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	created, err := s.svc.Create(r.Context(), caller, key)
	if err != nil {
		s.failWrite(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, wire.ConversationCreated{Conversation: created})
}

// transcript serves GET /assistant/conversations/{id}.
func (s *Surface) transcript(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	t, err := s.svc.Transcript(r.Context(), caller, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, t)
}

// clear serves DELETE /assistant/conversations/{id}: the Clear gesture.
func (s *Surface) clear(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key, ok := opkey.Key(w, r, s.now())
	if !ok {
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	answer, err := s.svc.Clear(r.Context(), caller, id, key)
	if err != nil {
		s.failWrite(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// ask serves POST /assistant/conversations/{id}/turns: one question, under
// the Idempotency-Key the dashboard mints for it.
func (s *Surface) ask(w http.ResponseWriter, r *http.Request) {
	conversation, ok := pathID(w, r)
	if !ok {
		return
	}
	var body wire.TurnRequest
	if !decode(w, r, &body, func() int { return body.ProtocolVersion }) {
		return
	}
	if detail := attachmentsRefusal(body.Attachments); detail != "" {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": detail})
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	if !s.eligible(w, r, p) {
		return
	}
	grant, err := s.grants.Read(r.Context(), conversation, assistant.TurnID(key))
	if err != nil {
		s.fail(w, r, &assistant.NotAvailable{Reason: wire.ViewerNotAPerson,
			Detail: err.Error()})
		return
	}
	accepted, err := s.svc.Start(r.Context(), assistant.TurnStart{Person: p,
		Conversation: conversation, Key: key, Input: body.Input.Text, Page: body.PageContext,
		Read: grant, Attachments: body.Attachments, Mode: body.Mode})
	if err != nil {
		s.failWrite(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, accepted)
}

// attachmentsRefusal is why a question's attachments are refused as written,
// or "": past wire.AttachmentsPerTurn, a nil id, or one file named twice.
// Whether each is a file of this conversation the person uploaded is the
// orchestrator's to check, against the store, before the turn starts.
func attachmentsRefusal(files []uuid.UUID) string {
	if len(files) > wire.AttachmentsPerTurn {
		return fmt.Sprintf("a question carries at most %d attachments, and this one "+
			"names %d", wire.AttachmentsPerTurn, len(files))
	}
	seen := make(map[uuid.UUID]bool, len(files))
	for _, id := range files {
		switch {
		case id == uuid.Nil:
			return "an attachment names no file"
		case seen[id]:
			return "file " + id.String() + " is attached twice"
		}
		seen[id] = true
	}
	return ""
}

// stop serves POST /assistant/turns/{id}/stop.
func (s *Surface) stop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body wire.Versioned
	if !decode(w, r, &body, func() int { return body.ProtocolVersion }) {
		return
	}
	key, ok := opkey.Key(w, r, s.now())
	if !ok {
		return
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	if !s.eligible(w, r, p) {
		return
	}
	answer, err := s.svc.Stop(r.Context(), p.ID, id, key)
	if err != nil {
		s.failWrite(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, answer)
}

// stream serves GET /assistant/conversations/{id}/stream?turn=<id>, resuming
// after the Last-Event-ID a client sends.
func (s *Surface) stream(w http.ResponseWriter, r *http.Request) {
	conversation, ok := pathID(w, r)
	if !ok {
		return
	}
	turn, err := uuid.Parse(r.URL.Query().Get("turn"))
	if err != nil || turn == uuid.Nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			httpjson.Detail{"detail": "turn names the turn to stream, by its id"})
		return
	}
	var after uint64
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		lastTurn, seq, err := wire.ParseEventID(last)
		if err != nil || lastTurn != turn {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				httpjson.Detail{"detail": "Last-Event-ID is not an event of this turn"})
			return
		}
		after = seq
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	if !s.eligible(w, r, p) {
		return
	}
	out := &sseWriter{w: w, rc: http.NewResponseController(w)}
	streamErr := s.svc.Stream(r.Context(), p.ID, conversation, turn, after, out)
	switch {
	case streamErr == nil:
	case out.started:
		s.log.InfoContext(r.Context(), "assistant_stream_ended", "turn", turn,
			"error", streamErr)
	case errors.Is(streamErr, assistant.ErrCaughtUp):
		// NOTHING FOLLOWS, AND NOTHING WILL: the client already holds the
		// segment's `done`. 204 is the event-stream answer that says so, and
		// the one a client does not dial again after.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, streamErr)
	}
}

// exportCSV and exportXLSX serve a table card's download.
func (s *Surface) exportCSV(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, "csv", "text/csv; charset=utf-8", s.tables.CSV)
}

func (s *Surface) exportXLSX(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, "xlsx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", s.tables.XLSX)
}

// export writes one of the caller's table cards from the snapshot the
// conversation stored, never a fresh read.
func (s *Surface) export(w http.ResponseWriter, r *http.Request, ext, contentType string,
	write func(io.Writer, wire.TableCard) error) {

	id, ok := pathID(w, r)
	if !ok {
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	table, err := s.svc.Table(r.Context(), caller, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`,
		exportName(table.Title), ext))
	w.Header().Set("Cache-Control", "no-store")
	if err := write(w, table); err != nil {
		s.log.ErrorContext(r.Context(), "assistant_export_failed", "card", id, "error", err)
	}
}

// exportName is a download's file name from a table's title: letters, digits
// and dashes only, at most 64 of them, so nothing a title holds can reach the
// header's syntax.
func exportName(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if b.Len() >= 64 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "assistant-table"
	}
	return name
}
