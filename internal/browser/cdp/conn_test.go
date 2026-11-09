package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// pageWords is what pads out the large messages the cases send: the words of
// a page, which no line the connection logs and no error it returns may carry.
const pageWords = "page-words-sentinel "

// sized is a message of exactly size bytes, as the transport carries it (the
// pipe's NUL aside): start, then pageWords over and over, then end.
func sized(size int, start, end string) []byte {
	fill := size - len(start) - len(end)
	b := make([]byte, 0, size)
	b = append(b, start...)
	b = append(b, bytes.Repeat([]byte(pageWords), fill/len(pageWords)+1)[:fill]...)
	return append(b, end...)
}

// A MESSAGE PAST THE READ LIMIT ENDS ONLY WHAT IT IS FOR, on both transports,
// and never the connection but for an event of the browser's own session
// (TestAnEventPastTheReadLimitOnTheBrowsersSessionEndsItsConnection's). A
// message of exactly ReadLimit bytes is delivered whole. Past it, an answer
// fails its own call with ErrMessageTooBig, naming the method, and the next
// call on that session is answered; an event no listener asked for is dropped,
// and its session goes on; an event a listener asked for ends that session
// alone with ErrMessageTooBig — its call in flight fails with it and its
// listener reports it — while a sibling session answers; and a message whose
// start names neither an id nor a method is no CDP message, and ends the
// connection as a whole one would. Each is logged at DEBUG by its id or method,
// its session and its size, never by its words. Not parallel: each transport
// holds a few copies of a 32 MiB message at once, which is enough for one case.
//
// Mutations: end the connection at a message past ReadLimit, and the large
// answer's call fails with the connection's end rather than its own — red;
// drop the socket's SetReadLimit(-1), and the library's own 32 KiB ends the
// connection at the message of exactly ReadLimit — red; set the library's
// limit at ReadLimit instead, and it closes the socket at the large answer —
// red; end the session of an event nobody asked for, and its listener reads
// that end where the mark after it was due — red; drop an event a listener
// asked for, and its session's call in flight waits to the case's bound — red;
// read the session off the message's start alone, and the event a listener
// asked for reaches the browser's session, which asked for nothing, so its own
// goes on — red.
func TestAMessagePastTheReadLimitEndsOnlyWhatItIsFor(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			ctx := bounded(t)
			logs := newRecords()
			c, p := tr.pair(t, logs.logger())
			held := announce(ctx, t, c, p, attachment{"big", "target-big"}, attachment{"sibling", "target-sibling"})
			big, sibling := held[0], held[1]
			sent := make(chan error, 1)
			write := func(b []byte) { go func() { sent <- p.write(b) }() }
			wrote := func() {
				t.Helper()
				if err := <-sent; err != nil {
					t.Fatalf("writing a large message: %v", err)
				}
			}
			// answers is a call on s the peer answers, as a check that s and
			// the connection are still open.
			answers := func(s *Session) {
				t.Helper()
				calls := make(chan error, 1)
				go func() {
					_, err := Call(ctx, s, testCall, testParams{})
					calls <- err
				}()
				if err := p.answer(p.next(), testResult{}); err != nil {
					t.Fatal(err)
				}
				if err := <-calls; err != nil {
					t.Fatalf("a call on %s after a message past ReadLimit: %v", sessionName(s.ID()), err)
				}
			}

			// Exactly ReadLimit: delivered whole.
			l := c.Browser().Listen(testEvent)
			write(sized(ReadLimit, `{"method":"Test.event","params":{"n":1,"text":"`, `"}}`))
			m, err := l.Next(ctx)
			if err != nil {
				t.Fatalf("a message of exactly ReadLimit bytes: %v", err)
			}
			if got := messageSize(m.Method, m.Params); got < ReadLimit-64 {
				t.Fatalf("a message of exactly ReadLimit bytes arrived as %d bytes", got)
			}
			wrote()
			l.Close()

			// An answer past it fails its own call alone.
			calls := make(chan error, 1)
			go func() {
				_, err := Call(ctx, big, testCall, testParams{})
				calls <- err
			}()
			r := p.next()
			write(sized(ReadLimit+1, fmt.Sprintf(`{"id":%d,"result":{"text":"`, r.ID), `"},"sessionId":"big"}`))
			if err := <-calls; !errors.Is(err, ErrMessageTooBig) || !strings.Contains(err.Error(), testCall.name) {
				t.Fatalf("the call answered past ReadLimit failed with %v, want ErrMessageTooBig naming %s", err, testCall.name)
			}
			wrote()
			answers(big)

			// An event past it nobody asked for is dropped. The mark after it
			// is routed after it, so by the time the mark is taken it has
			// been read through.
			lb := big.Listen(testEvent)
			write(sized(ReadLimit+1, `{"method":"Test.other","params":{"text":"`, `"},"sessionId":"big"}`))
			wrote()
			if err := p.event("big", testEvent.name, testEventParams{N: 2}); err != nil {
				t.Fatal(err)
			}
			if m, err := lb.Next(ctx); err != nil {
				t.Fatalf("after an event past ReadLimit nobody asked for, its session's listener read %v", err)
			} else if e, _, _ := testEvent.Of(m); e.N != 2 {
				t.Fatalf("the mark read as %+v", e)
			}
			answers(big)

			// An event past it a listener asked for ends its own session.
			go func() {
				_, err := Call(ctx, big, testCall, testParams{})
				calls <- err
			}()
			p.next() // in flight, and never answered
			write(sized(ReadLimit+1, `{"method":"Test.event","params":{"text":"`, `"},"sessionId":"big"}`))
			wrote()
			if err := <-calls; !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("the call in flight on the session whose event passed ReadLimit failed with %v, want ErrMessageTooBig", err)
			}
			if _, err := lb.Next(ctx); !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("the listener that asked for the event read %v, want ErrMessageTooBig", err)
			}
			select {
			case <-big.Done():
			default:
				t.Fatal("the session whose event passed ReadLimit is open")
			}
			answers(sibling)
			answers(c.Browser())
			if err := c.Err(); err != nil {
				t.Fatalf("the connection ended with %v", err)
			}

			// A message past it that names neither an id nor a method.
			write(sized(ReadLimit+1, `{"params":{"text":"`, `"}}`))
			select {
			case <-c.Done():
			case <-ctx.Done():
				t.Fatal("a message past ReadLimit that is no CDP message left the connection open")
			}
			if err := c.Err(); !errors.Is(err, ErrClosed) || !strings.Contains(err.Error(), fmt.Sprint(ReadLimit+1)) {
				t.Fatalf("the connection ended with %v, want ErrClosed naming the message's size", err)
			}

			logged := logs.text()
			for _, want := range []string{
				fmt.Sprintf("cdp_message_too_big id=%d method= session=big bytes=%d", r.ID, ReadLimit+1),
				fmt.Sprintf("cdp_message_too_big id=0 method=Test.other session=big bytes=%d", ReadLimit+1),
				fmt.Sprintf("cdp_message_too_big id=0 method=Test.event session=big bytes=%d", ReadLimit+1),
			} {
				if !strings.Contains(logged, want) {
					t.Errorf("the connection logged no %q:\n%s", want, logged)
				}
			}
			if strings.Contains(logged, pageWords) || strings.Contains(c.Err().Error(), pageWords) {
				t.Errorf("a line or an error carries the message's words:\n%s\n%v", logged, c.Err())
			}
		})
	}
}

// THE READER ROUTES A MESSAGE PAST THE READ LIMIT BY ITS START AND ITS END,
// the envelopes it keeps of it: the id or the method among the members its
// start holds whole, in any order, and its session from a member there or
// from the last member of the object, which is where Chromium writes it —
// never from a member nested in the parameters, nor from words inside a
// string that look like one. (How the reader keeps the two envelopes as the
// message streams past is TestAMessagePastTheReadLimitEndsOnlyWhatItIsFor's.)
//
// Mutations: take the session from the last "sessionId" member the end holds,
// wherever it sits, and the one nested in the parameters is read as the
// message's own — red; drop the check that a ',' or a '{' comes before the
// member's name, and a name ending in an escaped quote and sessionId is read
// as the member — red.
func TestAMessagePastTheReadLimitIsRoutedByItsStartAndItsEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		start, end string
		id         int64
		method     string
		session    SessionID
	}{
		{"an answer, as Chromium writes it", `{"id":7,"result":{"data":"`, `"},"sessionId":"A1"}`, 7, "", "A1"},
		{"an event of the browser's own session", `{"method":"Fetch.requestPaused","params":{"x":"`, `"}}`, 0, "Fetch.requestPaused", ""},
		{"an event of a child's", `{"method":"Runtime.bindingCalled","params":{"payload":"`, `"},"sessionId":"B2"}`, 0, "Runtime.bindingCalled", "B2"},
		{"members in another order", `{"sessionId":"C3","error":{"code":1},"id":9,"result":{"x":"`, `"}}`, 9, "", "C3"},
		{"white space", "{ \"method\" : \"Test.event\" , \"params\" : {\"x\":\"", "\"} ,\n \"sessionId\" : \"D4\" }\n", 0, "Test.event", "D4"},
		{"a session nested in the parameters", `{"method":"Target.detachedFromTarget","params":{"x":"`, `","sessionId":"E5"}}`, 0, "Target.detachedFromTarget", ""},
		{"a session inside a string", `{"method":"Test.event","params":{"x":"`, `\",\"sessionId\":\"F6\"}"}}`, 0, "Test.event", ""},
		{"a session inside the last member's string", `{"method":"Test.event","params":{},"note":"`, `\",\"sessionId\":\"G7"}`, 0, "Test.event", ""},
		{"a name that ends as the session's does", `{"method":"Test.event","params":{"x":"`, `"},"x\"sessionId":"H8"}`, 0, "Test.event", ""},
		{"no CDP message", `<html>`, `</html>`, 0, "", ""},
	} {
		// The envelopes of a message past ReadLimit: its first and its last
		// envelopeBytes, the page's words between them.
		fill := strings.Repeat(pageWords, envelopeBytes/len(pageWords)+1)
		start, end := tc.start+fill, fill+tc.end
		h := readHead([]byte(start[:envelopeBytes]))
		session := h.session
		if !h.named {
			session = tailSession([]byte(end[len(end)-envelopeBytes:]))
		}
		if h.id != tc.id || h.method != tc.method || session != tc.session {
			t.Errorf("%s: read as id %d, method %q, session %q; want %d, %q, %q",
				tc.name, h.id, h.method, session, tc.id, tc.method, tc.session)
		}
	}
}

// A MESSAGE THAT IS NO CDP MESSAGE ENDS ITS CONNECTION, on both transports:
// one that does not parse as JSON, and an object naming neither an id nor a
// method, are what a peer speaking something else sends — a page, an error of
// a pool's own — and the connection that reads one ends with ErrClosed, naming
// the message's size and none of its words. A mark sent after it is never
// delivered.
//
// Mutation: let the reader pass over such a message, and the mark after it is
// delivered on a connection still open — red.
func TestAMessageThatIsNoCDPMessageEndsItsConnection(t *testing.T) {
	t.Parallel()
	for _, tr := range transports {
		for _, tc := range []struct {
			name, message string
		}{
			{"a page", "<html>" + pageWords + "</html>"},
			{"an object naming neither an id nor a method", `{"` + pageWords + `":1}`},
		} {
			t.Run(tr.name+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				ctx := bounded(t)
				c, p := tr.pair(t, nil)
				marks := c.Browser().Listen(testEvent)
				if err := p.write([]byte(tc.message)); err != nil {
					t.Fatal(err)
				}
				// The peer's end may be closed by then: the mark's fate is
				// read off the listener.
				_ = p.event("", testEvent.name, testEventParams{N: 1})
				if m, err := marks.Next(ctx); err == nil {
					t.Fatalf("the mark sent after %q was delivered (%s): the connection read past it", tc.message, m.Params)
				}
				err := c.Err()
				if !errors.Is(err, ErrClosed) || !strings.Contains(err.Error(), fmt.Sprintf("of %d bytes", len(tc.message))) {
					t.Fatalf("the connection ended with %v, want ErrClosed naming the message's size", err)
				}
				if strings.Contains(err.Error(), pageWords) {
					t.Fatalf("the connection's end carries the message's words: %v", err)
				}
			})
		}
	}
}

// AN ANSWER AND AN EVENT REACH ONLY THEIR OWN SESSION: answers are matched by
// id across sessions, whatever order they come in, and an event goes to the
// listeners of the session it names and no other; an event for a session the
// connection holds no record of is dropped.
//
// Mutation: route events ignoring their sessionId (to the browser's
// session), and the child's event never reaches the child — red.
func TestAnAnswerAndAnEventReachOnlyTheirOwnSession(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	held := announce(ctx, t, c, p, attachment{"A", "target-A"}, attachment{"B", "target-B"})
	a, b := held[0], held[1]
	la, lb, lbrowser := a.Listen(testEvent), b.Listen(testEvent), c.Browser().Listen(testEvent)

	// Two calls in flight, answered in the reverse order.
	results := make(chan string, 2)
	for _, s := range []*Session{a, b} {
		go func() {
			got, err := Call(ctx, s, testCall, testParams{Text: string(s.ID())})
			if err != nil {
				results <- err.Error()
				return
			}
			results <- string(s.ID()) + "=" + got.Text
		}()
	}
	first, second := p.next(), p.next()
	for _, r := range []request{second, first} {
		var params testParams
		_ = json.Unmarshal(r.Params, &params)
		if err := p.answer(r, testResult{Text: "for " + params.Text}); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{<-results: true, <-results: true}
	if !got["A=for A"] || !got["B=for B"] {
		t.Fatalf("answers reached %v, want each its own session's", got)
	}

	// Events by session: A's to A, then one for a session nobody holds,
	// then the browser's and B's own, which close the window.
	for _, e := range []struct {
		session SessionID
		n       int
	}{{"A", 1}, {"nobody", 2}, {"", 3}, {"B", 4}} {
		if err := p.event(e.session, testEvent.name, testEventParams{N: e.n}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []struct {
		l *Listener
		n int
	}{{la, 1}, {lbrowser, 3}, {lb, 4}} {
		m, err := want.l.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e, _, _ := testEvent.Of(m); e.N != want.n {
			t.Fatalf("a listener read event %d, want %d: an event reached a session it does not name", e.N, want.n)
		}
	}
}

// A DETACH ENDS ONLY ITS SESSION AND FAILS ITS CALLS IN FLIGHT: a call in
// flight on a session Chromium detaches fails with a *DetachedError naming
// the session, a later call on it fails at once with the same, its listener
// hands over what it held and then reports the end, and a sibling session's
// calls are answered. Both ways Chromium detaches: Target.detachedFromTarget
// on the parent, and Inspector.detached on the session itself.
//
// Mutation: fail the calls of every session on a detach, and the sibling's
// call fails — red.
func TestADetachEndsOnlyItsSessionAndFailsItsCallsInFlight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		detach func(p *peer, s SessionID) error
		reason string
	}{
		{"Target.detachedFromTarget", func(p *peer, s SessionID) error {
			return p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": s})
		}, "detached from its target"},
		{"Inspector.detached", func(p *peer, s SessionID) error {
			return p.event(s, InspectorDetached.name, map[string]any{"reason": "target_closed"})
		}, "target_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := bounded(t)
			c, p := pipePair(t, nil)
			held := announce(ctx, t, c, p, attachment{"gone", "target-gone"}, attachment{"sibling", "target-sibling"})
			gone, sibling := held[0], held[1]
			l := gone.Listen(testEvent)
			if err := p.event("gone", testEvent.name, testEventParams{N: 7}); err != nil {
				t.Fatal(err)
			}
			inFlight := make(chan error, 1)
			go func() {
				_, err := Call(ctx, gone, testCall, testParams{})
				inFlight <- err
			}()
			p.next() // the call reached the browser, and is never answered
			siblings := make(chan error, 1)
			go func() {
				got, err := Call(ctx, sibling, testCall, testParams{})
				if err == nil && got.Text != "answered" {
					err = fmt.Errorf("answered %q", got.Text)
				}
				siblings <- err
			}()
			siblingCall := p.next() // in flight across the detach, answered after it
			if err := tc.detach(p, "gone"); err != nil {
				t.Fatal(err)
			}

			var detached *DetachedError
			if err := <-inFlight; !errors.As(err, &detached) || detached.SessionID != "gone" || detached.Reason != tc.reason {
				t.Fatalf("the call in flight failed with %v, want a *DetachedError naming session gone, %q", err, tc.reason)
			}
			if _, err := Call(ctx, gone, testCall, testParams{}); !errors.As(err, &detached) {
				t.Fatalf("a later call answered %v, want a *DetachedError", err)
			}
			if m, err := l.Next(ctx); err != nil {
				t.Fatalf("the listener did not hand over what it held first: %v", err)
			} else if e, _, _ := testEvent.Of(m); e.N != 7 {
				t.Fatalf("the listener handed over %+v", e)
			}
			if _, err := l.Next(ctx); !errors.As(err, &detached) {
				t.Fatalf("the drained listener reported %v, want the *DetachedError", err)
			}
			select {
			case <-gone.Done():
			default:
				t.Fatal("the detached session is not done")
			}

			if err := p.answer(siblingCall, testResult{Text: "answered"}); err != nil {
				t.Fatal(err)
			}
			if err := <-siblings; err != nil {
				t.Fatalf("the sibling's call in flight across the detach: %v", err)
			}
		})
	}
}

// A CRASH ENDS THE CRASHED TARGET'S SESSIONS, and no other's: Chromium's
// Target.targetCrashed at the browser level ends every session attached to
// the target it names with a *CrashedError naming it, and
// Inspector.targetCrashed on a session ends every session of that session's
// target. Their calls in flight, which Chromium would never answer, fail with
// it.
//
// Mutation: ignore Inspector.targetCrashed, and the second target's call in
// flight is never answered — red at the case's bound.
func TestACrashEndsTheCrashedTargetsSessions(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	// Two sessions on the first target, one on the second.
	held := announce(ctx, t, c, p, attachment{"one", "first"}, attachment{"two", "first"}, attachment{"three", "second"})
	one, two, three := held[0], held[1], held[2]
	calls := map[*Session]chan error{}
	for _, s := range []*Session{one, two, three} {
		done := make(chan error, 1)
		calls[s] = done
		go func() {
			_, err := Call(ctx, s, testCall, testParams{})
			done <- err
		}()
		p.next()
	}
	if err := p.event("", TargetTargetCrashed.name, map[string]any{
		"targetId": "first", "status": "crashed", "errorCode": 133,
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{one, two} {
		var crashed *CrashedError
		if err := <-calls[s]; !errors.As(err, &crashed) || crashed.TargetID != "first" || crashed.Status != "crashed" {
			t.Fatalf("session %s's call failed with %v, want a *CrashedError naming target first", s.ID(), err)
		}
	}
	select {
	case err := <-calls[three]:
		t.Fatalf("the second target's call ended with %v when the first target crashed", err)
	default:
	}

	if err := p.event("three", InspectorTargetCrashed.name, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var crashed *CrashedError
	if err := <-calls[three]; !errors.As(err, &crashed) || crashed.TargetID != "second" {
		t.Fatalf("the second target's call failed with %v, want a *CrashedError naming target second", err)
	}
	if _, err := Call(ctx, three, testCall, testParams{}); !errors.As(err, &crashed) {
		t.Fatalf("a call on the crashed session answered %v, want the *CrashedError", err)
	}
}

// A CLOSED TRANSPORT FAILS EVERY CALL AND ENDS EVERY LISTENER, on both
// transports: when the browser's end goes away, every call in flight on every
// session fails with ErrClosed, every listener reports it, the connection is
// done, and a later call fails at once — never a hang.
//
// Mutation: leave the calls in flight waiting when the reader ends, and they
// hang — red at the case's bound.
func TestAClosedTransportFailsEveryCallAndEndsEveryListener(t *testing.T) {
	t.Parallel()
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			t.Parallel()
			ctx := bounded(t)
			c, p := tr.pair(t, nil)
			child := announce(ctx, t, c, p, attachment{"child", "target"})[0]
			l := child.Listen(testEvent)
			calls := make(chan error, 2)
			for _, s := range []*Session{c.Browser(), child} {
				go func() {
					_, err := Call(ctx, s, testCall, testParams{})
					calls <- err
				}()
				p.next()
			}
			p.stop()
			for range 2 {
				if err := <-calls; !errors.Is(err, ErrClosed) {
					t.Fatalf("a call in flight failed with %v, want ErrClosed", err)
				}
			}
			if _, err := l.Next(ctx); !errors.Is(err, ErrClosed) {
				t.Fatalf("the listener reported %v, want ErrClosed", err)
			}
			select {
			case <-c.Done():
			default:
				t.Fatal("the connection is not done")
			}
			if _, err := Call(ctx, child, testCall, testParams{}); !errors.Is(err, ErrClosed) {
				t.Fatalf("a later call answered %v, want ErrClosed", err)
			}
		})
	}
}

// backlogs is what c counts as held for its listeners: by every session's
// together, and by s's.
func backlogs(c *Conn, s *Session) (conn, session int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backlog, s.backlog
}

// A READER THE PAGE OUTRUNS ENDS ONLY ITS SESSION: the events a session's
// listeners have not taken count against MaxBacklog, each once however many of
// them hold it, and past it that session ends with ErrBacklog and lets go of
// what its listeners held — they report the end at once, handing nothing over
// — while a sibling session and the connection go on. The browser's own
// session ends only with its connection, so a listener of its that does not
// keep up ends the connection, while a second connection goes on. An event
// nobody listens for counts nothing, and an event taken counts no more — so a
// listener that keeps up passes any amount — nor does one a listener held when
// it was closed, its session ended or not. An event of just under ReadLimit,
// which two listeners of a page's session hold while another tab's listener
// holds events of its own, ends nothing, and the page's next event ends the
// page's session alone.
//
// Mutations: stop subtracting what a listener takes, and the listener that
// keeps up ends its connection; stop subtracting what a closed listener held,
// and the events a listener opened after it takes end the connection; let
// Close do nothing once the listener's session has ended, and the ended
// session's events stay counted; count an event once for each listener that
// holds it, and the two listeners' events end their session; let go of an
// event at the first of its holders to take it, and what the second still
// holds counts nothing; count every session's events against one MaxBacklog,
// and the event just under ReadLimit ends the connection; end the browser's
// own session alone, and its listener's flood leaves the connection open; hand
// a session's listener what it held after its backlog's end, and it reads the
// events it was let go of — red.
func TestAReaderThePageOutrunsEndsOnlyItsSession(t *testing.T) {
	t.Parallel()
	const chunk = 1 << 20
	text := strings.Repeat("x", chunk)
	over := MaxBacklog/chunk + 2   // events that sum past one session's bound
	half := MaxBacklog/chunk/2 + 2 // two of these sum past it, one does not
	// event is one event of about a mebibyte, written as it is rather than
	// marshalled, which under the race detector costs more than the claim.
	event := func(p *peer, method string, n int) error {
		return p.write([]byte(fmt.Sprintf(`{"method":%q,"params":{"n":%d,"text":"%s"}}`, method, n, text)))
	}
	// sessionEvent is event on a session's own listeners.
	sessionEvent := func(p *peer, session SessionID, n int) error {
		return p.write([]byte(fmt.Sprintf(`{"method":%q,"sessionId":%q,"params":{"n":%d,"text":"%s"}}`,
			testEvent.name, session, n, text)))
	}
	// marked sends a small event of its own on the browser's session and
	// takes it from marks: the reader routes a transport's messages in
	// order, so every message sent before it has been routed by the time it
	// arrives.
	marked := func(ctx context.Context, t *testing.T, p *peer, marks *Listener, n int) {
		t.Helper()
		if err := p.event("", testOther.name, testEventParams{N: n}); err != nil {
			t.Fatalf("mark %d: %v", n, err)
		}
		m, err := marks.Next(ctx)
		if err != nil {
			t.Fatalf("mark %d: the connection ended: %v", n, err)
		}
		if e, _, _ := testOther.Of(m); e.N != n {
			t.Fatalf("mark %d read as %d", n, e.N)
		}
	}
	// answers is a call on s the peer answers, as a check that s is open.
	answers := func(ctx context.Context, t *testing.T, p *peer, s *Session) {
		t.Helper()
		calls := make(chan error, 1)
		go func() {
			_, err := Call(ctx, s, testCall, testParams{})
			calls <- err
		}()
		if err := p.answer(p.next(), testResult{}); err != nil {
			t.Fatal(err)
		}
		if err := <-calls; err != nil {
			t.Fatalf("a call on %s: %v", sessionName(s.ID()), err)
		}
	}

	t.Run("an event nobody listens for counts nothing", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		l := c.Browser().Listen(testEvent)
		for i := range over {
			if err := event(p, testOther.name, i); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.event("", testEvent.name, testEventParams{N: -1}); err != nil {
			t.Fatal(err)
		}
		if m, err := l.Next(ctx); err != nil {
			t.Fatalf("after %d MiB nobody listened for, the connection ended: %v", over, err)
		} else if e, _, _ := testEvent.Of(m); e.N != -1 {
			t.Fatalf("read %+v", e)
		}
	})

	t.Run("a listener that keeps up", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		l := c.Browser().Listen(testEvent)
		for i := range over {
			if err := event(p, testEvent.name, i); err != nil {
				t.Fatal(err)
			}
			m, err := l.Next(ctx)
			if err != nil {
				t.Fatalf("event %d of a listener that keeps up: %v", i, err)
			}
			if e, _, _ := testEvent.Of(m); e.N != i {
				t.Fatalf("event %d read as %d", i, e.N)
			}
		}
	})

	t.Run("a listener closed with what it held", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		marks := c.Browser().Listen(testOther)
		held := c.Browser().Listen(testEvent)
		for i := range half {
			if err := event(p, testEvent.name, i); err != nil {
				t.Fatal(err)
			}
		}
		marked(ctx, t, p, marks, 1)
		held.Close()
		after := c.Browser().Listen(testEvent)
		for i := range half {
			if err := event(p, testEvent.name, i); err != nil {
				t.Fatalf("event %d after the close: the connection ended: %v", i, err)
			}
		}
		marked(ctx, t, p, marks, 2)
		for i := range half {
			m, err := after.Next(ctx)
			if err != nil {
				t.Fatalf("event %d after the close: %v", i, err)
			}
			if e, _, _ := testEvent.Of(m); e.N != i {
				t.Fatalf("event %d after the close read as %d", i, e.N)
			}
		}
		if err := c.Err(); err != nil {
			t.Fatalf("the connection ended with %v", err)
		}
	})

	t.Run("an ended session's listener, closed", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		ended := announce(ctx, t, c, p, attachment{"ended", "target-ended"})[0]
		held := ended.Listen(testEvent)
		for i := range half {
			if err := sessionEvent(p, "ended", i); err != nil {
				t.Fatal(err)
			}
		}
		// Detached after its events, which the reader therefore queued first.
		if err := p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "ended"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ended.Done():
		case <-ctx.Done():
			t.Fatal("the detached session never ended")
		}
		if conn, _ := backlogs(c, ended); conn < half*chunk {
			t.Fatalf("the ended session's listener holds its %d MiB, and the connection counts %d bytes", half, conn)
		}
		held.Close()
		if conn, session := backlogs(c, ended); conn != 0 || session != 0 {
			t.Fatalf("once its listener closed, the ended session counts %d bytes and the connection %d", session, conn)
		}
	})

	t.Run("an event several listeners hold counts once", func(t *testing.T) {
		t.Parallel()
		if half*chunk >= MaxBacklog || 2*half*chunk <= MaxBacklog {
			t.Fatalf("%d events of %d bytes are not less than MaxBacklog once and more twice", half, chunk)
		}
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		page := announce(ctx, t, c, p, attachment{"page", "target-page"})[0]
		marks := c.Browser().Listen(testOther)
		// Two listeners for one event, neither drained.
		first, second := page.Listen(testEvent), page.Listen(testEvent)
		defer first.Close()
		defer second.Close()
		for i := range half {
			if err := sessionEvent(p, "page", i); err != nil {
				t.Fatal(err)
			}
		}
		marked(ctx, t, p, marks, 1)
		if err := page.Err(); err != nil {
			t.Fatalf("%d MiB held by each of two listeners ended their session: %v", half, err)
		}
		conn, held := backlogs(c, page)
		if held < half*chunk || held > half*(chunk+64) || conn != held {
			t.Fatalf("%d MiB two listeners hold counts %d bytes for their session and %d for the connection, "+
				"want each event once", half, held, conn)
		}
		for i := range half {
			if _, err := first.Next(ctx); err != nil {
				t.Fatalf("event %d of the first listener: %v", i, err)
			}
		}
		if _, after := backlogs(c, page); after != held {
			t.Fatalf("once the first listener took every event, its session counts %d bytes, want the %d the second still holds",
				after, held)
		}
		for i := range half {
			if _, err := second.Next(ctx); err != nil {
				t.Fatalf("event %d of the second listener: %v", i, err)
			}
		}
		if conn, session := backlogs(c, page); conn != 0 || session != 0 {
			t.Fatalf("once both listeners took every event, the session counts %d bytes and the connection %d", session, conn)
		}
	})

	t.Run("a session's listener that does not keep up", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		held := announce(ctx, t, c, p, attachment{"page", "target-page"}, attachment{"sibling", "target-sibling"})
		page, sibling := held[0], held[1]
		marks := c.Browser().Listen(testOther)
		l := page.Listen(testEvent)
		for i := range over {
			if err := sessionEvent(p, "page", i); err != nil {
				t.Fatal(err)
			}
		}
		marked(ctx, t, p, marks, 1)
		if err := page.Err(); !errors.Is(err, ErrBacklog) {
			t.Fatalf("%d MiB untaken ended the page's session with %v, want ErrBacklog", over, err)
		}
		// What it held was let go of with its end: none of it is handed over.
		if m, err := l.Next(ctx); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the page's listener read %s, %v; want ErrBacklog, its events let go of", m.Params, err)
		}
		if conn, session := backlogs(c, page); conn != 0 || session != 0 {
			t.Fatalf("the ended session counts %d bytes and the connection %d", session, conn)
		}
		if c.Session("page") != page {
			t.Fatal("the page's session, ended by its backlog, is no longer the one the connection holds for its id")
		}
		answers(ctx, t, p, sibling)
		answers(ctx, t, p, c.Browser())
		if err := c.Err(); err != nil {
			t.Fatalf("the connection ended with %v", err)
		}
	})

	t.Run("the browser's own listener that does not keep up", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		other, otherPeer := pipePair(t, nil)
		l := c.Browser().Listen(testEvent)
		sent := 0
		for ; sent < over; sent++ {
			if err := event(p, testEvent.name, sent); err != nil {
				break // the connection ended, and closed its end
			}
		}
		select {
		case <-c.Done():
		case <-ctx.Done():
			t.Fatalf("%d MiB untaken by the browser's own listener did not end the connection", sent)
		}
		if !errors.Is(c.Err(), ErrBacklog) {
			t.Fatalf("the connection ended with %v, want ErrBacklog", c.Err())
		}
		if m, err := l.Next(ctx); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the listener read %s, %v; want ErrBacklog, its events let go of", m.Params, err)
		}
		go func() {
			if r, err := otherPeer.recv(); err == nil {
				_ = otherPeer.answer(r, testResult{Text: "still here"})
			}
		}()
		if got, err := Call(ctx, other.Browser(), testCall, testParams{}); err != nil || got.Text != "still here" {
			t.Fatalf("the other connection answered %+v, %v", got, err)
		}
	})

	t.Run("an event just under the read limit", func(t *testing.T) {
		t.Parallel()
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		held := announce(ctx, t, c, p, attachment{"page", "target-page"}, attachment{"other", "target-other"})
		page, other := held[0], held[1]
		marks := c.Browser().Listen(testOther)
		// The other tab's listener holds a few events of its own, untaken.
		tab := other.Listen(testEvent)
		defer tab.Close()
		for i := range 2 {
			if err := p.event("other", testEvent.name, testEventParams{N: i, Text: strings.Repeat("o", 900)}); err != nil {
				t.Fatal(err)
			}
		}
		first, second := page.Listen(testEvent), page.Listen(testEvent)
		defer first.Close()
		defer second.Close()
		if err := p.write(sized(ReadLimit-64, `{"method":"Test.event","params":{"text":"`, `"},"sessionId":"page"}`)); err != nil {
			t.Fatal(err)
		}
		marked(ctx, t, p, marks, 1)
		for name, s := range map[string]*Session{"page": page, "other": other} {
			if err := s.Err(); err != nil {
				t.Fatalf("an event of %d bytes, just under ReadLimit, that two listeners of the page hold ended the %s's session: %v",
					ReadLimit-64, name, err)
			}
		}
		if err := c.Err(); err != nil {
			t.Fatalf("an event just under ReadLimit ended the connection: %v", err)
		}
		// The page's next event passes its own bound: its session ends, and
		// nothing else does.
		if err := p.event("page", testEvent.name, testEventParams{N: 1, Text: strings.Repeat("p", 200)}); err != nil {
			t.Fatal(err)
		}
		marked(ctx, t, p, marks, 2)
		if err := page.Err(); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the page's session ended with %v, want ErrBacklog", err)
		}
		for i, l := range []*Listener{first, second} {
			if m, err := l.Next(ctx); !errors.Is(err, ErrBacklog) {
				t.Fatalf("the page's listener %d read %d bytes, %v; want ErrBacklog", i, len(m.Params), err)
			}
		}
		for i := range 2 {
			m, err := tab.Next(ctx)
			if err != nil {
				t.Fatalf("the other tab's event %d: %v", i, err)
			}
			if e, _, _ := testEvent.Of(m); e.N != i {
				t.Fatalf("the other tab's event %d read as %d", i, e.N)
			}
		}
		answers(ctx, t, p, other)
		if err := c.Err(); err != nil {
			t.Fatalf("the connection ended with %v", err)
		}
	})
}

// THE CONNECTION'S BACKSTOP ENDS THE SESSION THAT HOLDS THE MOST: once the
// events every session's listeners hold would pass MaxConnBacklog, the session
// holding the most ends with ErrBacklog and lets go of them, and the event that
// would have passed the backstop is delivered — its own session is never ended
// for another's flood. A session that has ended already, its listener not yet
// drained, lets go of what it holds the same way, its listener reporting the
// session's own end with ErrBacklog beside it; and the browser's own session,
// holding the most, ends the connection. Not parallel: each case holds nearly
// MaxConnBacklog at once.
//
// Mutations: end the event's own session at the backstop, and the small
// session ends while the large ones go on — red; look for the most among the
// sessions the connection still holds rather than every one whose listeners
// hold events, and an open session ends in the detached one's place — red;
// end the browser's own session alone, and the connection stays open — red.
func TestTheConnectionsBackstopEndsTheSessionThatHoldsTheMost(t *testing.T) {
	const chunk = 1 << 20
	text := strings.Repeat("x", chunk)
	// One session holds at most this many of the cases' events, and every
	// session together this many: each event is a mebibyte and a few dozen
	// bytes.
	const perSession, perConn = MaxBacklog/chunk - 1, MaxConnBacklog/chunk - 1
	if perSession+perSession+1 != perConn {
		t.Fatalf("MaxConnBacklog is %d and MaxBacklog %d: the cases fill the backstop with two sessions", MaxConnBacklog, MaxBacklog)
	}
	// fill writes n events on session s, the browser's for "".
	fill := func(t *testing.T, p *peer, s SessionID, n int) {
		t.Helper()
		for i := range n {
			m := fmt.Sprintf(`{"method":%q,"params":{"n":%d,"text":"%s"}`, testEvent.name, i, text)
			if s != "" {
				m += fmt.Sprintf(`,"sessionId":%q`, s)
			}
			if err := p.write([]byte(m + "}")); err != nil {
				t.Fatalf("event %d of %q: %v", i, s, err)
			}
		}
	}
	// marked is TestAReaderThePageOutrunsEndsOnlyItsSession's.
	marked := func(ctx context.Context, t *testing.T, p *peer, marks *Listener, n int) {
		t.Helper()
		if err := p.event("", testOther.name, testEventParams{N: n}); err != nil {
			t.Fatalf("mark %d: %v", n, err)
		}
		if _, err := marks.Next(ctx); err != nil {
			t.Fatalf("mark %d: the connection ended: %v", n, err)
		}
	}

	t.Run("an open session holding the most", func(t *testing.T) {
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		held := announce(ctx, t, c, p, attachment{"most", "target-most"}, attachment{"next", "target-next"},
			attachment{"small", "target-small"})
		most, next, small := held[0], held[1], held[2]
		marks := c.Browser().Listen(testOther)
		listeners := map[*Session]*Listener{}
		for _, s := range held {
			listeners[s] = s.Listen(testEvent)
		}
		fill(t, p, "most", perSession-1)
		fill(t, p, "next", perSession-2)
		fill(t, p, "small", 4)
		marked(ctx, t, p, marks, 1)
		for _, s := range held {
			if err := s.Err(); err != nil {
				t.Fatalf("%s ended under the backstop: %v", sessionName(s.ID()), err)
			}
		}
		// One more of small's passes the backstop.
		fill(t, p, "small", 1)
		marked(ctx, t, p, marks, 2)
		if err := most.Err(); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the session holding the most ended with %v, want ErrBacklog", err)
		}
		if _, err := listeners[most].Next(ctx); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the listener of the session holding the most read %v, want ErrBacklog", err)
		}
		for s, want := range map[*Session]int{next: perSession - 2, small: 5} {
			if err := s.Err(); err != nil {
				t.Fatalf("%s ended with %v", sessionName(s.ID()), err)
			}
			for i := range want {
				if _, err := listeners[s].Next(ctx); err != nil {
					t.Fatalf("event %d of %s: %v", i, sessionName(s.ID()), err)
				}
			}
		}
		if err := c.Err(); err != nil {
			t.Fatalf("the connection ended with %v", err)
		}
	})

	t.Run("a session that has ended", func(t *testing.T) {
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		held := announce(ctx, t, c, p, attachment{"gone", "target-gone"}, attachment{"next", "target-next"},
			attachment{"small", "target-small"})
		gone, next, small := held[0], held[1], held[2]
		marks := c.Browser().Listen(testOther)
		listeners := map[*Session]*Listener{}
		for _, s := range held {
			listeners[s] = s.Listen(testEvent)
		}
		fill(t, p, "gone", perSession-1)
		if err := p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "gone"}); err != nil {
			t.Fatal(err)
		}
		fill(t, p, "next", perSession-2)
		fill(t, p, "small", 5)
		marked(ctx, t, p, marks, 1)
		var detached *DetachedError
		err := gone.Err()
		if !errors.As(err, &detached) {
			t.Fatalf("the detached session ended with %v", err)
		}
		_, err = listeners[gone].Next(ctx)
		if !errors.As(err, &detached) || !errors.Is(err, ErrBacklog) {
			t.Fatalf("the detached session's listener read %v, want its *DetachedError with ErrBacklog beside it", err)
		}
		for _, s := range []*Session{next, small} {
			if err := s.Err(); err != nil {
				t.Fatalf("%s ended with %v", sessionName(s.ID()), err)
			}
		}
		if err := c.Err(); err != nil {
			t.Fatalf("the connection ended with %v", err)
		}
	})

	t.Run("the browser's own session holding the most", func(t *testing.T) {
		ctx := bounded(t)
		c, p := pipePair(t, nil)
		held := announce(ctx, t, c, p, attachment{"next", "target-next"}, attachment{"small", "target-small"})
		next := held[0]
		marks := c.Browser().Listen(testOther)
		own := c.Browser().Listen(testEvent)
		nexts := next.Listen(testEvent)
		held[1].Listen(testEvent)
		fill(t, p, "", perSession)
		fill(t, p, "next", perSession-1)
		fill(t, p, "small", 2)
		marked(ctx, t, p, marks, 1)
		// One more of small's passes the backstop. The connection may close
		// its end before the peer's write is done.
		_ = p.write([]byte(fmt.Sprintf(`{"method":%q,"params":{"text":"%s"},"sessionId":"small"}`, testEvent.name, text)))
		select {
		case <-c.Done():
		case <-ctx.Done():
			t.Fatal("the browser's own session held the most past the backstop, and the connection is open")
		}
		if err := c.Err(); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the connection ended with %v, want ErrBacklog", err)
		}
		if _, err := own.Next(ctx); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the browser's own listener read %v, want ErrBacklog", err)
		}
		// Another session's listener drains what it holds, then reports
		// the connection's end.
		for i := range perSession - 1 {
			if _, err := nexts.Next(ctx); err != nil {
				t.Fatalf("event %d of the other session: %v", i, err)
			}
		}
		if _, err := nexts.Next(ctx); !errors.Is(err, ErrBacklog) {
			t.Fatalf("the other session's listener read %v once it had drained, want ErrBacklog", err)
		}
	})
}

// A SESSION THAT HAS ENDED STILL ENDS THE CHILDREN IT ANNOUNCED. A session
// ended by the connection rather than by Chromium — here an event past
// ReadLimit its listener asked for — is still attached at Chromium's end, and
// the detach of a child it announced arrives on it and nowhere else: the
// connection holds it, ended, so that detach still ends the child. Its
// listeners are handed nothing after its end. Once Chromium detaches it, the
// connection forgets it.
//
// Mutation: forget a session at its end, whatever ended it, and the child's
// detach is unrouted: the child stays open — red.
func TestASessionThatHasEndedStillEndsTheChildrenItAnnounced(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			ctx := bounded(t)
			c, p := tr.pair(t, nil)
			parent := announce(ctx, t, c, p, attachment{"parent", "target-parent"})[0]
			l := parent.Listen(testEvent, TargetAttachedToTarget)
			defer l.Close()
			// The parent announces a child of its own, as a page announces
			// a frame.
			if err := p.event("parent", TargetAttachedToTarget.name, map[string]any{
				"sessionId":          "child",
				"targetInfo":         map[string]any{"targetId": "target-child", "type": "iframe"},
				"waitingForDebugger": false,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Next(ctx); err != nil {
				t.Fatal(err)
			}
			child := c.Session("child")
			if err := child.Err(); err != nil {
				t.Fatalf("the announced child has ended: %v", err)
			}
			if err := p.write(sized(ReadLimit+1, `{"method":"Test.event","params":{"text":"`, `"},"sessionId":"parent"}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-parent.Done():
			case <-ctx.Done():
				t.Fatal("the parent whose event passed ReadLimit is open")
			}
			if err := p.event("parent", testEvent.name, testEventParams{N: 1}); err != nil {
				t.Fatal(err)
			}
			if err := p.event("parent", TargetDetachedFromTarget.name, map[string]any{"sessionId": "child"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-child.Done():
			case <-ctx.Done():
				t.Fatal("the child's detach, on its parent that had ended, left the child open")
			}
			var detached *DetachedError
			if err := child.Err(); !errors.As(err, &detached) || detached.SessionID != "child" {
				t.Fatalf("the child ended with %v, want a *DetachedError naming it", err)
			}
			if m, err := l.Next(ctx); !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("the parent's listener read %s, %v after its end; want its end alone", m.Method, err)
			}
			if c.Session("parent") != parent {
				t.Fatal("the parent, ended by the connection, is no longer the session the connection holds for its id")
			}
			// Chromium's detach of the parent itself: forgotten from then on.
			detaches := c.Browser().Listen(TargetDetachedFromTarget)
			defer detaches.Close()
			if err := p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "parent"}); err != nil {
				t.Fatal(err)
			}
			if _, err := detaches.Next(ctx); err != nil {
				t.Fatal(err)
			}
			if c.Session("parent") == parent {
				t.Fatal("the parent Chromium detached is still the session the connection holds for its id")
			}
			if err := c.Err(); err != nil {
				t.Fatalf("the connection ended with %v", err)
			}
		})
	}
}

// AN EVENT PAST THE READ LIMIT ON THE BROWSER'S OWN SESSION ENDS ITS
// CONNECTION, on both transports, when a listener of that session asked for
// it: the browser's session ends only with its connection, since without it
// the connection could create, attach and resume nothing. The connection ends
// with ErrMessageTooBig, the listener reports that end and nothing after it,
// and an announcement Chromium sent after it registers nothing: the session it
// names is one that ended with the connection.
//
// Mutation: end the browser's own session alone, and the connection stays open
// — red.
func TestAnEventPastTheReadLimitOnTheBrowsersSessionEndsItsConnection(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			ctx := bounded(t)
			c, p := tr.pair(t, nil)
			l := c.Browser().Listen(testEvent, TargetAttachedToTarget)
			sent := make(chan error, 1)
			go func() {
				sent <- p.write(sized(ReadLimit+1, `{"method":"Test.event","params":{"text":"`, `"}}`))
			}()
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			// What Chromium sends next: the connection may have closed its
			// end by then.
			_ = p.event("", testEvent.name, testEventParams{N: 42})
			_ = p.attach("late", "target-late", "page")
			select {
			case <-c.Done():
			case <-ctx.Done():
				t.Fatal("an event past ReadLimit the browser's own listener asked for left the connection open")
			}
			if err := c.Err(); !errors.Is(err, ErrMessageTooBig) || !strings.Contains(err.Error(), "the connection") {
				t.Fatalf("the connection ended with %v, want ErrMessageTooBig naming the connection", err)
			}
			if m, err := l.Next(ctx); !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("the browser's listener read %s %s, %v; want the end alone", m.Method, m.Params, err)
			}
			if err := c.Session("late").Err(); !errors.Is(err, ErrMessageTooBig) {
				t.Fatalf("the session announced after the end ended with %v, want the connection's end", err)
			}
		})
	}
}

// THE CLIENT NEVER LOGS OR REPORTS PARAMETERS OR RESULTS: a sentinel sits in a
// command's parameters, in its result, in the data of an error Chromium
// answers with, in an event for a session nobody holds and in an answer that
// arrives after its call gave up. Nothing the connection logs, at any level,
// and no error a call returns carries it — only methods and ids.
//
// Mutation: log the result on cdp_answer_unmatched, and the late answer's
// sentinel is logged — red.
func TestTheClientNeverLogsOrReportsParametersOrResults(t *testing.T) {
	t.Parallel()
	const sentinel = "hunter2-sentinel"
	ctx := bounded(t)
	logs := newRecords()
	c, p := pipePair(t, logs.logger())
	var errs []string

	// A refusal whose data echoes the parameters.
	refused := make(chan error, 1)
	go func() {
		_, err := Call(ctx, c.Browser(), testCall, testParams{Text: sentinel})
		refused <- err
	}()
	r := p.next()
	if !strings.Contains(string(r.Params), sentinel) {
		t.Fatalf("the parameters were not sent: %s", r.Params)
	}
	if err := p.refuse(r, -32602, "Invalid parameters", map[string]any{"text": sentinel}); err != nil {
		t.Fatal(err)
	}
	err := <-refused
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Method != testCall.name || protocolErr.Code != -32602 {
		t.Fatalf("the refusal answered %v, want a *ProtocolError for %s", err, testCall.name)
	}
	errs = append(errs, err.Error())

	// A result carrying it, which only the caller reads.
	answered := make(chan error, 1)
	go func() {
		got, err := Call(ctx, c.Browser(), testCall, testParams{})
		if err == nil && got.Text != sentinel {
			err = fmt.Errorf("read %q", got.Text)
		}
		answered <- err
	}()
	if err := p.answer(p.next(), testResult{Text: sentinel}); err != nil {
		t.Fatal(err)
	}
	if err := <-answered; err != nil {
		t.Fatal(err)
	}

	// A call that gives up, and its answer after.
	late, cancel := context.WithCancel(ctx)
	gaveUp := make(chan error, 1)
	go func() {
		_, err := Call(late, c.Browser(), testCall, testParams{Text: sentinel})
		gaveUp <- err
	}()
	r = p.next()
	cancel()
	err = <-gaveUp
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the call that gave up answered %v", err)
	}
	errs = append(errs, err.Error())
	if err := p.answer(r, testResult{Text: sentinel}); err != nil {
		t.Fatal(err)
	}
	if err := logs.await(ctx, "cdp_answer_unmatched"); err != nil {
		t.Fatal(err)
	}

	// An event for a session the connection holds no record of.
	if err := p.event("nobody", testEvent.name, testEventParams{Text: sentinel}); err != nil {
		t.Fatal(err)
	}
	if err := logs.await(ctx, "cdp_event_unrouted"); err != nil {
		t.Fatal(err)
	}

	logged := logs.text()
	if strings.Contains(logged, sentinel) {
		t.Errorf("the connection logged a parameter or a result:\n%s", logged)
	}
	if !strings.Contains(logged, "method=Test.event") || !strings.Contains(logged, "session=nobody") {
		t.Errorf("the unrouted event's line names neither its method nor its session:\n%s", logged)
	}
	for _, e := range errs {
		if strings.Contains(e, sentinel) {
			t.Errorf("an error carries a parameter or a result: %s", e)
		}
	}
}

// A SESSION THE CONNECTION DOES NOT HOLD IS ONE THAT HAS ENDED: looked up after
// its detach, a session is handed back ended — Done closed, Err a
// *DetachedError naming it — and so is an id the connection never held, and
// one announced naming no target, which a crash could never name; never a
// session left open that nothing would end. A call on any of them fails at
// once and sends nothing, the browser's next frame being a live call made
// after them. On a connection that has ended, any id is a session that ended
// with it.
//
// Mutations: register an id the connection does not hold, and the session
// looked up after its detach is open — red; register an announcement that
// names no target, and that session is open — red; drop Session's case of an
// ended connection, and a session looked up after the end reports a detach —
// red.
func TestASessionTheConnectionDoesNotHoldHasEnded(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	gone := announce(ctx, t, c, p, attachment{"gone", "target-gone"})[0]
	if err := p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "gone"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone.Done():
	case <-ctx.Done():
		t.Fatal("the detached session never ended")
	}
	announced := c.Browser().Listen(TargetAttachedToTarget)
	if err := p.event("", TargetAttachedToTarget.name, map[string]any{
		"sessionId": "untargeted", "targetInfo": map[string]any{"type": "page"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := announced.Next(ctx); err != nil {
		t.Fatal(err)
	}
	announced.Close()
	for _, id := range []SessionID{"gone", "never", "untargeted"} {
		s := c.Session(id)
		select {
		case <-s.Done():
		default:
			t.Fatalf("session %s, looked up where the connection holds none, is open", id)
		}
		var detached *DetachedError
		if err := s.Err(); !errors.As(err, &detached) || detached.SessionID != id {
			t.Fatalf("session %s ended with %v, want a *DetachedError naming it", id, err)
		}
		if _, err := Call(ctx, s, testCall, testParams{Text: string(id)}); !errors.As(err, &detached) {
			t.Fatalf("a call on session %s answered %v, want its *DetachedError", id, err)
		}
	}
	live := make(chan error, 1)
	go func() {
		_, err := Call(ctx, c.Browser(), testCall, testParams{Text: "live"})
		live <- err
	}()
	r := p.next()
	var params testParams
	if json.Unmarshal(r.Params, &params) != nil || params.Text != "live" || r.SessionID != "" {
		t.Fatalf("the browser read %s on session %q first: a call on a session that had ended was sent", r.Params, r.SessionID)
	}
	if err := p.answer(r, testResult{}); err != nil {
		t.Fatal(err)
	}
	if err := <-live; err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	after := c.Session("after")
	select {
	case <-after.Done():
	default:
		t.Fatal("a session looked up on an ended connection is open")
	}
	if err := after.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("a session looked up on an ended connection ended with %v, want the connection's ErrClosed", err)
	}
}

// A COMMAND UNDER A CONTEXT THAT HAS ENDED SENDS NOTHING: a call, or a send,
// fails with the context's cause — the send's Pending holds it — and its
// command never reaches the browser, whose next frame is a live call made
// after fifty of each.
//
// Mutation: drop send's check of an ended context, and a command offered to
// an idle writer is sent one time in two — red.
func TestACommandUnderAnEndedContextSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	ended, cancel := context.WithCancel(ctx)
	cancel()
	for i := range 50 {
		if _, err := Call(ended, c.Browser(), testCall, testParams{Text: "called off"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("call %d under an ended context answered %v, want context.Canceled", i, err)
		}
		sent := Send(ended, c.Browser(), testCall, testParams{Text: "sent off"})
		if _, err := sent.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("send %d under an ended context answered %v, want context.Canceled", i, err)
		}
	}
	live := make(chan error, 1)
	go func() {
		_, err := Call(ctx, c.Browser(), testCall, testParams{Text: "live"})
		live <- err
	}()
	r := p.next()
	var params testParams
	if json.Unmarshal(r.Params, &params) != nil || params.Text != "live" {
		t.Fatalf("the browser read %s first: a call made under an ended context was sent", r.Params)
	}
	if err := p.answer(r, testResult{}); err != nil {
		t.Fatal(err)
	}
	if err := <-live; err != nil {
		t.Fatal(err)
	}
}

// A LISTENER ITS READER CLOSED SAYS SO: Next after Close answers
// ErrListenerClosed — though the listener held an event when it was closed,
// and its session goes on — rather than wait for events nothing will queue on
// it again; and a second Close changes nothing.
//
// Mutation: drop Next's case of a closed listener, and Next waits to the
// case's bound — red.
func TestAClosedListenerAnswersErrListenerClosed(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	l := c.Browser().Listen(testEvent)
	marks := c.Browser().Listen(testOther)
	// The mark is routed after the event, so the event is queued by then.
	if err := p.event("", testEvent.name, testEventParams{N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.event("", testOther.name, testEventParams{N: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := marks.Next(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		l.Close()
		if _, err := l.Next(ctx); !errors.Is(err, ErrListenerClosed) {
			t.Fatalf("Next after Close answered %v, want ErrListenerClosed", err)
		}
	}
	if err := c.Err(); err != nil {
		t.Fatalf("the connection ended with %v", err)
	}
}

// A WRITE THAT FAILS ENDS THE CONNECTION: when the browser stops taking frames
// — its read end closed alone, its writes still open — the write of the next
// command fails, the connection ends with ErrClosed, and that call fails with
// it at once, never at its deadline.
//
// Mutation: let a failed write leave the connection open (drop writeLoop's
// end), and the call waits to the case's bound — red.
func TestAFailedWriteEndsTheConnection(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	clientReads, peerWrites := io.Pipe()
	peerReads, clientWrites := io.Pipe()
	c := NewPipe(clientReads, clientWrites, Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = peerWrites.Close()
	})
	_ = peerReads.Close()
	if _, err := Call(ctx, c.Browser(), testCall, testParams{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("the call whose write failed answered %v, want ErrClosed", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("the connection whose write failed is open")
	}
	if err := c.Err(); !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("the connection ended with %v, want ErrClosed wrapping the write's own error", err)
	}
}
