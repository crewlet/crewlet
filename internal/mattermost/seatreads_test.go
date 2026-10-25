package mattermost_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// recordingSource answers nothing and remembers every name it was asked.
type recordingSource struct{ asked []string }

func (r *recordingSource) Lookup(name string) (string, bool) {
	r.asked = append(r.asked, name)
	return "", false
}

// THE DOCTOR'S READS ARE THE TRANSPORT'S. SeatTokenReads is every variable
// SeatTokens reads for the seats the doctor checks: a bot token that is one
// whole ${VAR} reads it, a literal — or a reference inside other text, which
// the transport sends as written — reads none, and a person's seat and a seat
// with no bot are not checked at all. A running node is asked for exactly
// these, each a reveal logged by name.
func TestSeatTokenReadsAreWhatTheDoctorReads(t *testing.T) {
	t.Parallel()
	bot := func(name, handle, token string) *org.Role {
		return &org.Role{Name: name, DeclaredHandle: handle,
			Mattermost: org.MattermostIdentity{BotToken: token}}
	}
	founder := bot("Founder", "founder", "${FOUNDER_BOT}")
	founder.Kind = org.KindHuman
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		bot("CEO", "ceo", "${BOT_CEO}"),
		bot("CTO", "cto", "a-literal-token"),
		bot("CFO", "cfo", "prefix-${NOT_WHOLE}"),
		founder,
		{Name: "Silent", DeclaredHandle: "silent"},
	}, Units: []*org.Unit{{Name: "Platform", Roles: []*org.Role{bot("SWE", "swe", " ${BOT_SWE} ")}}}}

	want := []string{"BOT_CEO", "BOT_SWE"}
	if got := mattermost.SeatTokenReads(o); !slices.Equal(got, want) {
		t.Errorf("SeatTokenReads = %v, want %v", got, want)
	}
	recorder := &recordingSource{}
	read := mattermost.SeatTokens(config.NewResolver(recorder))
	for seat := range o.AllRoles() {
		if seat.IsAgent() && !seat.Mattermost.IsZero() {
			read(seat)
		}
	}
	slices.Sort(recorder.asked)
	if got := slices.Compact(recorder.asked); !slices.Equal(got, want) {
		t.Errorf("SeatTokens read %v, want what SeatTokenReads names, %v", got, want)
	}
	if got := mattermost.SeatTokenReads(nil); got != nil {
		t.Errorf("SeatTokenReads(nil) = %v, want none", got)
	}
}
