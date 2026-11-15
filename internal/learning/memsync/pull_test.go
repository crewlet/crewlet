package memsync

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

// A HYDRATION'S PULL IS BOUNDED IN BYTES AS WELL AS IN ROWS
// (crewlet/crewlet#1052).
//
// A replay pull asked for up to fetchBatch rows and was bounded by nothing
// else, and it is held whole until its transaction writes it, while a row is
// bounded only by the transport: 256 rows of up to 8 MiB each on the embedded
// broker, inside seat acquisition. The cases below watch every pull a
// hydration makes (spyCopies), on a broker carrying the max_payload each
// names.

// writeNotesOfSize writes n diary notes for the seat whose content is size
// bytes each.
func writeNotesOfSize(t *testing.T, db *store.DB, n, size int) {
	t.Helper()
	content := strings.Repeat("x", size)
	for i := range n {
		if _, err := db.SQL().ExecContext(t.Context(), `INSERT INTO agent_diary
			(id, agent_id, kind, content, source, created_at)
			VALUES (?, ?, 'diary_long', ?, 'reflect', 1)`,
			fmt.Sprintf("p%04d", i), seat.AgentID, content); err != nil {
			t.Fatalf("write note %d: %v", i, err)
		}
	}
}

// A REPLAY LARGER THAN A PULL TAKES SEVERAL, AND EVERY ROW ARRIVES IN A
// HYDRATION THAT SUCCEEDS.
//
// A pull is bounded in bytes, at one message at the connection's max_payload
// and the room the broker counts beside it, so a table whose rows outweigh
// that takes as many pulls as its bytes need, none holding more; and in rows,
// so a table whose rows outnumber fetchBatch takes as many as its rows need.
// Either way every row arrives, once. The bytes here are six notes, each over
// half a pull's bound so no pull holds two, on a broker carrying 64 KiB; the
// rows are three hundred short notes on one carrying a mebibyte, in fewer
// bytes than a pull holds, so only their count divides them.
//
// Mutation: drop the byte bound, so one pull holds every note; raise it, so a
// pull holds two of them; or ask for every row the copy holds in one pull;
// and this goes red.
func TestAReplayLargerThanAPullTakesSeveralAndDeliversEveryRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// limit is the broker's max_payload; 0 is nats-server's default,
		// a mebibyte.
		limit int32
		// notes is how many the seat holds, of size bytes of content each.
		notes, size int
		// bytesBind says the notes' bytes divide them into pulls, rather
		// than their count.
		bytesBind bool
	}{
		{"rows that outweigh a pull", 64 << 10, 6, (64 << 10) * 3 / 5, true},
		{"rows that outnumber a pull", 0, fetchBatch + 44, 64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := brokerCarrying(t, tc.limit)
			holder := openStore(t)
			writeNotesOfSize(t, holder, tc.notes, tc.size)
			if published, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil ||
				published.Rows != tc.notes {
				t.Fatalf("the publish carried %d rows (%v), want the %d notes", published.Rows, err, tc.notes)
			}

			fresh := openStore(t)
			syncer := syncerOn(t, fresh, conn)
			spy := newSpyCopies(syncer)
			carried, err := syncer.Hydrate(t.Context(), seat.Handle)
			if err != nil {
				t.Fatalf("a replay larger than a pull refused the seat: %v", err)
			}
			if held := countRows(t, fresh, "agent_diary"); carried != tc.notes || held != tc.notes {
				t.Fatalf("the hydration carried %d rows and the store holds %d notes, want all %d",
					carried, held, tc.notes)
			}

			// THE BOUND A PULL'S BYTES ARE HELD TO: one message at the
			// transport's limit, and the room the broker counts beside it.
			bound := int(conn.MaxPayload()) + deliveryOverhead
			pulls := spy.of(tableFilter(seat.AgentID, "agent_diary"))
			delivered, largest, total := 0, 0, 0
			for i, p := range pulls {
				if p.headed {
					t.Fatalf("pull %d delivered a row with headers, whose bytes this case does not count", i)
				}
				delivered += len(p.sizes)
				total += p.bytes()
				for _, size := range p.sizes {
					largest = max(largest, size)
				}
				if len(p.sizes) > fetchBatch {
					t.Errorf("pull %d held %d rows, past the %d a pull asks for at most", i, len(p.sizes), fetchBatch)
				}
				if p.bytes() > bound {
					t.Errorf("pull %d held %d rows in %d bytes, past a message at the transport's limit "+
						"and the room the broker counts beside it (%d)", i, len(p.sizes), p.bytes(), bound)
				}
			}
			if delivered != tc.notes {
				t.Errorf("the pulls delivered %d rows, want each of the %d notes once", delivered, tc.notes)
			}
			if len(pulls) < 2 {
				t.Errorf("the %d notes arrived in %d pull(s), want several", tc.notes, len(pulls))
			}

			// THE CASE'S OWN PRECONDITIONS, read off what the broker
			// delivered: notes whose bytes divide them hold no two to a
			// pull, and notes whose count divides them fit one pull's bytes.
			switch {
			case tc.bytesBind && 2*largest <= bound:
				t.Fatalf("a note is %d bytes and a pull's bound %d: this case needs every note over "+
					"half the bound, so no pull holds two", largest, bound)
			case !tc.bytesBind && (total > bound || tc.notes <= fetchBatch):
				t.Fatalf("the %d notes are %d bytes and a pull's bound %d: this case needs more notes "+
					"than a pull asks for, in fewer bytes than it holds", tc.notes, total, bound)
			}
		})
	}
}

// A ROW AT THE TRANSPORT'S LIMIT STILL HYDRATES.
//
// The broker sends a pull nothing past what is left of its bytes, so a pull
// bounded below the next row ends with no row, and so does every pull after
// it, and the seat is refused, on every node at every sweep. A pull's bound
// therefore holds the largest row the changelog can hold: a message at the
// connection's max_payload, its headers and payload together, and the subject
// and the acknowledgement subject the broker counts beside it. The row here is
// a note whose message is exactly the limit, on brokers carrying three: a
// small one; the 8 MiB the embedded broker carries (queue.MaxPayloadBytes);
// and more, as an operator may raise an external broker's, which the engine
// refuses only below the 8 MiB, so a row that large is published there and a
// bound fixed at the 8 MiB would refuse its seat everywhere.
//
// Mutation: bound a pull by the max_payload alone, without the room for the
// subjects beside it, or by the embedded broker's 8 MiB rather than the
// connection's limit, and this goes red.
func TestARowAtTheTransportsLimitStillHydrates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		limit int32
	}{
		{"on a broker carrying 64 KiB", 64 << 10},
		{"on the embedded broker's 8 MiB", queue.MaxPayloadBytes},
		{"on an external broker raised past it", queue.MaxPayloadBytes + 1<<20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := brokerCarrying(t, tc.limit)
			if got := conn.MaxPayload(); got != int64(tc.limit) {
				t.Fatalf("the broker carries %d bytes a message, and this case needs %d", got, tc.limit)
			}
			note := noteOfSize(t, "at-the-limit", int(tc.limit))
			publishOn(t, conn, registered(t, "agent_diary").subject(seat.AgentID, note.Values), note)

			fresh := openStore(t)
			carried, err := syncerOn(t, fresh, conn).Hydrate(t.Context(), seat.Handle)
			if err != nil {
				t.Fatalf("a row at the transport's limit refused the seat: %v", err)
			}
			if carried != 1 {
				t.Fatalf("the hydration carried %d rows, want the 1 note", carried)
			}
			var length int
			if err := fresh.SQL().QueryRowContext(t.Context(),
				"SELECT length(content) FROM agent_diary WHERE id = 'at-the-limit'").Scan(&length); err != nil {
				t.Fatalf("the note did not arrive: %v", err)
			}
			if want := len(note.Values["content"].(string)); length != want {
				t.Errorf("the note arrived with %d bytes of content, want its %d", length, want)
			}
		})
	}
}
