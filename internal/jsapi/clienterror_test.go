package jsapi_test

import (
	"fmt"
	"testing"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A RAW READ'S ANSWER IS CLASSIFIED AS THE CLIENT'S IS: a leader read decodes
// the broker's answer itself (*server.ApiError), and jsprovision, which links
// no nats-server, reads only the client's spelling, so ClientError hands both
// over. A group with no leader and an absent stream are told apart; the read's
// own words stay.
//
// Mutation: ClientError hands err back as it is — neither is classified.
func TestARawAnswerIsClassifiedAsTheClientsIs(t *testing.T) {
	t.Parallel()
	leaderless := jsapi.ClientError(fmt.Errorf("jsapi: read S from its leader: %w", server.NewJSClusterNotAvailError()))
	absent := jsapi.ClientError(fmt.Errorf("jsapi: read S from its leader: %w", server.NewJSStreamNotFoundError()))
	if !jsprovision.Leaderless(leaderless) || jsprovision.StreamAbsent(leaderless) {
		t.Errorf("a raw 10008 reads as leaderless %v, absent %v; want leaderless alone",
			jsprovision.Leaderless(leaderless), jsprovision.StreamAbsent(leaderless))
	}
	if !jsprovision.StreamAbsent(absent) || jsprovision.Leaderless(absent) {
		t.Errorf("a raw 10059 reads as absent %v, leaderless %v; want absent alone",
			jsprovision.StreamAbsent(absent), jsprovision.Leaderless(absent))
	}
	if got, want := leaderless.Error(), "jsapi: read S from its leader: "+server.NewJSClusterNotAvailError().Error(); got != want {
		t.Errorf("the answer reads %q; want the read's own words %q", got, want)
	}
}
