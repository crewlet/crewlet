package dictation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/dictation"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/speech"
	"github.com/crewlet/crewlet/internal/tokens"
)

const (
	origin      = "https://crewlet.example.com"
	founder     = "dictation-founder-token-long-enough"
	colleague   = "dictation-colleague-token-long-enough"
	sentence    = "Let's move the launch to next Monday."
	sentenceTwo = "We still need to fix the payment bug."
)

var roomy = dictation.Limits{Session: time.Minute, Idle: 30 * time.Second}

// relay is a running /ws/dictation over a fake recognizer, admitting two
// Tier A tokens: two people.
type relay struct {
	url string
	b   config.Bootstrap
	h   http.Handler
}

func newRelay(t *testing.T, recognizer speech.Streamer, limits dictation.Limits) *relay {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: founder, Grants: []iam.Grant{iam.GrantWorklet}},
		{ID: "colleague", Token: colleague, Grants: []iam.Grant{iam.GrantWorklet}},
	}
	b.API.ExternalURL = origin
	svc, err := stream.NewService(livestate.New(), stream.Options{
		Health:    func() stream.Health { return status("ok") },
		Posture:   func(stream.Health) stream.FramePosture { return stream.FrameLive },
		Seats:     func() tokens.Seats { return tokens.Seats{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return map[string]any{} },
		Tools:     func() any { return nil },
		Schedules: func() any { return []any{} },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Chart:     authz.NoChart{},
		Holders:   noHolders{},
		SeatOf:    func(string) (stream.SeatState, bool) { return stream.SeatState{Human: true}, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	h := dictation.New(auth.New(&b), auth.NewCSRF(&b), svc, recognizer, limits)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &relay{url: "ws" + strings.TrimPrefix(srv.URL, "http"), b: b, h: h}
}

type status string

func (s status) NodeStatus() string { return string(s) }

type noHolders struct{}

func (noHolders) HolderRecord(context.Context, string) (string, error) {
	return "", errors.New("no directory here")
}

// client is one browser tab's dictation socket.
type client struct {
	conn *websocket.Conn
}

func (r *relay) dial(t *testing.T, token string) *client {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), r.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &client{conn: conn}
}

func (c *client) text(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.Write(t.Context(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *client) start(t *testing.T) {
	t.Helper()
	c.text(t, map[string]any{"type": "start", "lang": "en", "rate": 16000, "encoding": "pcm16"})
}

// speak sends d of voiced audio in 100 ms frames.
func (c *client) speak(t *testing.T, d time.Duration) {
	t.Helper()
	pcm := speech.FakeSpeech(d)
	for len(pcm) > 0 {
		n := min(len(pcm), speech.BytesPerSecond/10)
		if err := c.conn.Write(t.Context(), websocket.MessageBinary, pcm[:n]); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		pcm = pcm[n:]
	}
}

type msg struct {
	Type     string   `json:"type"`
	Progress *float64 `json:"progress"`
	Seq      *int     `json:"seq"`
	Text     string   `json:"text"`
	Reason   string   `json:"reason"`
	Code     string   `json:"code"`
}

func (c *client) next(t *testing.T) msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := c.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m msg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

// until reads until a message of kind arrives, answering those before it.
func (c *client) until(t *testing.T, kind string) (msg, []msg) {
	t.Helper()
	var before []msg
	for {
		m := c.next(t)
		if m.Type == kind {
			return m, before
		}
		if m.Type == "error" || m.Type == "busy" || (m.Type == "done" && kind != "done") {
			t.Fatalf("waiting for %s, the dictation ended: %+v", kind, m)
		}
		before = append(before, m)
	}
}

func (c *client) closed(t *testing.T) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := c.conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// A DICTATION IS PARTIALS THAT REPLACE ONE SEGMENT, A FINAL THAT APPENDS IT,
// THEN DONE: the protocol a composer draws provisional words and splices the
// final from. Each partial of a segment carries the segment's seq, the final
// the same seq, and the next segment the next one.
func TestADictationRelaysPartialsThenItsFinal(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence, sentenceTwo), roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 2400*time.Millisecond)
	final, partials := c.until(t, "final")
	if final.Text != sentence || final.Seq == nil || *final.Seq != 0 {
		t.Errorf("final = %+v, want %q as segment 0", final, sentence)
	}
	if len(partials) == 0 {
		t.Fatal("no partial preceded the final")
	}
	for _, p := range partials {
		if p.Type != "partial" || p.Seq == nil || *p.Seq != 0 {
			t.Errorf("a message before the first final is %+v, want a partial of segment 0", p)
		}
	}
	if last := partials[len(partials)-1].Text; !strings.HasPrefix("let's move the launch to next monday", last) {
		t.Errorf("the last partial %q is not the segment's words so far", last)
	}
	c.speak(t, 600*time.Millisecond)
	if p, _ := c.until(t, "partial"); p.Seq == nil || *p.Seq != 1 {
		t.Errorf("the next segment's partial is %+v, want seq 1", p)
	}
	c.text(t, map[string]string{"type": "finish"})
	last, _ := c.until(t, "final")
	if last.Text != "We still need." || *last.Seq != 1 {
		t.Errorf("the finish's final = %+v, want the rest of segment 1", last)
	}
	if done, _ := c.until(t, "done"); done.Reason != "finish" {
		t.Errorf("done = %+v, want reason finish", done)
	}
	if got := c.closed(t); got != websocket.StatusNormalClosure {
		t.Errorf("closed %d, want 1000", got)
	}
}

// AUDIO SENT WHILE THE RECOGNIZER IS PREPARED IS HELD AND HEARD: the first
// words of a dictation started while a warm recognizer loads are not lost, and
// the tab is told the progress meanwhile.
func TestAudioSentWhilePreparingIsHeldAndHeard(t *testing.T) {
	t.Parallel()
	fake := speech.NewFake(sentence)
	release := make(chan struct{})
	fake.PrepareWith(func(ctx context.Context, progress func(float64)) error {
		progress(0.5)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	r := newRelay(t, fake, roomy)
	c := r.dial(t, founder)
	c.start(t)
	p := c.next(t)
	if p.Type != "preparing" || p.Progress == nil || *p.Progress != 0.5 {
		t.Fatalf("first message = %+v, want preparing at 0.5", p)
	}
	c.speak(t, 900*time.Millisecond)
	close(release)
	c.until(t, "ready")
	c.text(t, map[string]string{"type": "finish"})
	final, _ := c.until(t, "final")
	if final.Text != "Let's move the." {
		t.Errorf("final = %q, want the three words the held audio revealed", final.Text)
	}
}

// AND PAST THE HOLD, THE DICTATION ENDS `preparing`: a first download is not
// something a person's speech can wait out, and holding it would grow without
// bound.
func TestPreparingLongerThanTheHoldEndsTheDictation(t *testing.T) {
	t.Parallel()
	fake := speech.NewFake(sentence)
	fake.PrepareWith(func(ctx context.Context, progress func(float64)) error {
		<-ctx.Done()
		return ctx.Err()
	})
	r := newRelay(t, fake, roomy)
	c := r.dial(t, founder)
	c.start(t)
	go func() {
		pcm := speech.FakeSpeech(11 * time.Second)
		for len(pcm) > 0 {
			n := min(len(pcm), speech.BytesPerSecond/10)
			if c.conn.Write(context.Background(), websocket.MessageBinary, pcm[:n]) != nil {
				return
			}
			pcm = pcm[n:]
		}
	}()
	if done, _ := c.until(t, "done"); done.Reason != "preparing" {
		t.Errorf("done = %+v, want reason preparing", done)
	}
}

// ONE LIVE DICTATION PER PERSON PER NODE: a second tab of the same person is
// told busy, and a colleague is not.
func TestASecondDictationOfTheSamePersonIsBusy(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	first := r.dial(t, founder)
	first.start(t)
	first.until(t, "ready")

	second := r.dial(t, founder)
	if busy := second.next(t); busy.Type != "busy" || busy.Reason != "session" {
		t.Errorf("the same person's second dictation got %+v, want busy session", busy)
	}
	if got := second.closed(t); got != websocket.StatusTryAgainLater {
		t.Errorf("closed %d, want 1013", got)
	}
	other := r.dial(t, colleague)
	other.start(t)
	other.until(t, "ready")

	first.text(t, map[string]string{"type": "cancel"})
	first.until(t, "done")
	first.closed(t)
	again := r.dial(t, founder)
	again.start(t)
	again.until(t, "ready")
}

// PAST THE RECOGNIZER'S CAPACITY THE ANSWER IS BUSY, which a screen meets by
// using the browser's own recognition.
func TestPastTheRecognizersCapacityIsBusy(t *testing.T) {
	t.Parallel()
	fake := speech.NewFake(sentence)
	fake.SetCapacity(1)
	r := newRelay(t, fake, roomy)
	first := r.dial(t, founder)
	first.start(t)
	first.until(t, "ready")
	second := r.dial(t, colleague)
	second.start(t)
	if busy := second.next(t); busy.Type != "busy" || busy.Reason != "capacity" {
		t.Errorf("past capacity got %+v, want busy capacity", busy)
	}
}

// A DICTATION THAT SENDS NO AUDIO ENDS `idle`, with what was said kept.
func TestAnIdleDictationEnds(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), dictation.Limits{Session: time.Minute, Idle: 300 * time.Millisecond})
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 600*time.Millisecond)
	final, _ := c.until(t, "final")
	if final.Text != "Let's move." {
		t.Errorf("the idle cut's final = %q, want the words said", final.Text)
	}
	if done, _ := c.until(t, "done"); done.Reason != "idle" {
		t.Errorf("done = %+v, want reason idle", done)
	}
}

// A DICTATION ENDS `length` AT ITS AUDIO CAP, however fast the audio came.
func TestADictationEndsAtItsLength(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), dictation.Limits{Session: time.Second, Idle: time.Second})
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 1200*time.Millisecond)
	if done, _ := c.until(t, "done"); done.Reason != "length" {
		t.Errorf("done = %+v, want reason length", done)
	}
}

// THE RECOGNIZER'S OWN LENGTH LIMIT ENDS THE DICTATION THE SAME WAY.
func TestTheRecognizersLengthLimitEndsTheDictation(t *testing.T) {
	t.Parallel()
	fake := speech.NewFake(sentence)
	fake.Fail(speech.FakeLength, speech.BytesPerSecond/2)
	r := newRelay(t, fake, roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, time.Second)
	if done, _ := c.until(t, "done"); done.Reason != "length" {
		t.Errorf("done = %+v, want reason length", done)
	}
}

// A RECOGNIZER LOST MID-DICTATION IS AN ERROR, never a silent end.
func TestALostRecognizerIsAnError(t *testing.T) {
	t.Parallel()
	fake := speech.NewFake(sentence)
	fake.Fail(speech.FakeDrop, 2)
	r := newRelay(t, fake, roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 2*time.Second)
	m, _ := c.until(t, "error")
	if m.Code != "dictation_unavailable" {
		t.Errorf("error = %+v, want dictation_unavailable", m)
	}
}

// CANCEL ENDS THE DICTATION WITH NO FINAL: nothing is spliced.
func TestCancelEndsWithNoFinal(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 600*time.Millisecond)
	c.text(t, map[string]string{"type": "cancel"})
	done, before := c.until(t, "done")
	if done.Reason != "cancel" {
		t.Errorf("done = %+v, want reason cancel", done)
	}
	for _, m := range before {
		if m.Type == "final" {
			t.Errorf("a cancelled dictation sent a final: %+v", m)
		}
	}
}

// A START THE RECOGNIZER CANNOT SERVE IS REFUSED bad_params: another rate,
// encoding or language, or audio before any start.
func TestAStartTheRecognizerCannotServeIsRefused(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	for name, first := range map[string]any{
		"rate":     map[string]any{"type": "start", "lang": "en", "rate": 8000, "encoding": "pcm16"},
		"encoding": map[string]any{"type": "start", "lang": "en", "rate": 16000, "encoding": "opus"},
		"language": map[string]any{"type": "start", "lang": "de", "rate": 16000, "encoding": "pcm16"},
		"type":     map[string]any{"type": "finish"},
	} {
		c := r.dial(t, founder)
		c.text(t, first)
		if m := c.next(t); m.Type != "error" || m.Code != "bad_params" {
			t.Errorf("%s: got %+v, want error bad_params", name, m)
		}
		if got := c.closed(t); got != websocket.StatusPolicyViolation {
			t.Errorf("%s: closed %d, want 1008", name, got)
		}
	}
}

// A FRAME OVER THE READ LIMIT CLOSES THE SOCKET: one message cannot make the
// engine buffer more than the largest frame a client sends.
func TestAFrameOverTheReadLimitClosesTheSocket(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	if err := c.conn.Write(t.Context(), websocket.MessageBinary, make([]byte, 9<<10)); err != nil {
		t.Fatal(err)
	}
	if got := c.closed(t); got != websocket.StatusMessageTooBig {
		t.Errorf("closed %d, want 1009", got)
	}
}

// DICTATION THIS NODE CANNOT SERVE IS REFUSED BEFORE THE UPGRADE, 503 with the
// reason, where a page's plain re-ask reads it.
func TestUnavailableDictationIsRefusedBeforeTheUpgrade(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.Disabled{}, roomy)
	req := httptest.NewRequest(http.MethodGet, dictation.Path, nil)
	req.Header.Set("Authorization", "Bearer "+founder)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "dictation_unavailable" || body["reason"] != "disabled" {
		t.Errorf("body = %v, want dictation_unavailable with reason disabled", body)
	}
}

// A PAGE ON ANOTHER SITE IS REFUSED THE HANDSHAKE, by the writes' CSRF rule, as
// /ws/stream's is: a browser attaches the session cookie to a cross-site
// page's handshake, and this socket hears the person's microphone.
func TestAHandshakeFromAnotherSiteIsRefused(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	for from, want := range map[string]bool{origin: true, "https://evil.example.com": false} {
		conn, res, err := websocket.Dial(t.Context(), r.url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + founder}, "Origin": {from}}})
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
		if want && err != nil {
			t.Errorf("a handshake from %s was refused: %v", from, err)
		}
		if !want && (err == nil || res == nil || res.StatusCode != http.StatusForbidden) {
			t.Errorf("a handshake from %s: err %v, response %v; want 403", from, err, res)
		}
	}
}

// A PRINCIPAL WHOSE STAGE MAY NOT ACT IS REFUSED, before the upgrade and
// naming the stage. Mutation: admit without the stage check, and the
// suspended person's plain GET is answered 426 rather than 403.
func TestAPrincipalWhoseStageMayNotActIsRefused(t *testing.T) {
	t.Parallel()
	r := newRelay(t, speech.NewFake(sentence), roomy)
	for _, stage := range []iam.Stage{iam.StageActive, iam.StageSuspended, iam.StageRetired} {
		p := iam.Principal{Login: "jo.park", Kind: iam.KindPerson, Seat: "analyst",
			Stage: stage, Grants: []iam.Grant{iam.GrantWorklet}}
		req := httptest.NewRequest(http.MethodGet, dictation.Path, nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		switch {
		case stage.MayAct() && rec.Code != http.StatusUpgradeRequired:
			t.Errorf("an %s person's plain GET answered %d, want 426", stage, rec.Code)
		case !stage.MayAct() && rec.Code != http.StatusForbidden:
			t.Errorf("a %s person's handshake answered %d, want 403", stage, rec.Code)
		}
	}
}

// NOTHING A PERSON SAYS REACHES A LOG. The session's one line carries who,
// how long, how much audio and how it ended; the words never.
func TestNothingSaidReachesALog(t *testing.T) {
	logs := &syncBuffer{}
	logging.Configure(slog.LevelDebug, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	const secret = "Zanzibar quarterly figures are confidential."
	r := newRelay(t, speech.NewFake(secret), roomy)
	c := r.dial(t, founder)
	c.start(t)
	c.until(t, "ready")
	c.speak(t, 2*time.Second)
	c.text(t, map[string]string{"type": "finish"})
	c.until(t, "done")
	c.closed(t)
	logs.waitFor(t, "dictation_session_ended")

	var ended map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		lower := strings.ToLower(line)
		for _, word := range []string{"zanzibar", "quarterly", "confidential"} {
			if strings.Contains(lower, word) {
				t.Errorf("a log line carries %q: %s", word, line)
			}
		}
		if strings.Contains(line, "dictation_session_ended") {
			if err := json.Unmarshal([]byte(line), &ended); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ended == nil {
		t.Fatalf("no dictation_session_ended line:\n%s", logs.String())
	}
	if ended["person"] == "" || ended["person"] == nil || ended["reason"] != "finish" ||
		ended["bytes"] != float64(2*speech.BytesPerSecond) {
		t.Errorf("the session line is %v, want the person, the audio's size and the reason", ended)
	}
}

// syncBuffer is a log sink a test can wait on.
type syncBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wrote == nil {
		b.wrote = make(chan struct{}, 1)
	}
	select {
	case b.wrote <- struct{}{}:
	default:
	}
	return b.buf.Write(p)
}

// waitFor returns once a line holding want was written.
func (b *syncBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	b.mu.Lock()
	if b.wrote == nil {
		b.wrote = make(chan struct{}, 1)
	}
	wrote := b.wrote
	b.mu.Unlock()
	timeout := time.After(10 * time.Second) //fixedwait:liveness
	for !strings.Contains(b.String(), want) {
		select {
		case <-wrote:
		case <-timeout:
			t.Fatalf("no log line holds %q:\n%s", want, b.String())
		}
	}
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
