package sandbox

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// sweID is the fixture seat every case here runs a job for.
//
// A UUID rather than "a-1", because the control subject a completion travels
// on is built from a run's AGENT ID: the id is what a seat's mailbox is named
// by, and a row carrying anything else names no routable seat at all. It rides
// PendingRun as text — the record is a JSON blob the framework never decodes —
// so a case that wrote a handle there would exercise the unroutable path while
// looking like the ordinary one.
const sweID = "0f1d4c07-6d2a-4e2b-9a3c-5b8e17d04c21"

// sweControl is that seat's sandbox-control subject.
func sweControl() string { return topics.AgentControl(uuid.MustParse(sweID)) }
