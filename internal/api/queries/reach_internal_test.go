package queries

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/usage"
)

// questionReach is EVERY QUESTION this package answers and the reach ADR-0031's
// withholding rule gives it — members read what the company published, admins
// also what the machine processed and how it is run, and `viewer` alone is
// open, because a sign-in page asks it of somebody who has not signed in.
//
// A REVIEWED LIST, held against what [Register] actually registers rather than
// derived from it, in the idiom of the API's published-routes list: a new
// question has to be classified in a diff somebody reads, and one whose reach
// moves has to move here too. Either half alone — a list nobody checks, or a
// registration nobody lists — is how a transcript reaches a member screen.
var questionReach = map[string]auth.Reach{
	"viewer": auth.ReachOpen,

	"work_items": auth.ReachMember, "work_item": auth.ReachMember,
	"work_comments": auth.ReachMember, "work_views": auth.ReachMember,
	"work_saved_views": auth.ReachMember, "work_projects": auth.ReachMember,
	"work_project": auth.ReachMember, "work_workload": auth.ReachMember,
	"work_activity": auth.ReachMember, "work_routing": auth.ReachMember,
	"work_catalogue": auth.ReachMember, "work_flow": auth.ReachMember,
	"work_files": auth.ReachMember, "work_search": auth.ReachMember,
	"company_feed": auth.ReachMember, "pages": auth.ReachMember,
	"page": auth.ReachMember, "page_activity": auth.ReachMember,
	"page_revision": auth.ReachMember, "containers": auth.ReachMember,
	"knowledge": auth.ReachMember, "colleague": auth.ReachMember,
	"seat_activity": auth.ReachMember, "schedules": auth.ReachMember,
	"work_my_work": auth.ReachMember, "work_inbox": auth.ReachMember,
	"work_person": auth.ReachMember, "decisions": auth.ReachMember,

	"agent": auth.ReachAdmin, "live_call": auth.ReachAdmin, "turn": auth.ReachAdmin,
	"turns": auth.ReachAdmin, "phases": auth.ReachAdmin, "events": auth.ReachAdmin,
	"event": auth.ReachAdmin, "event_series": auth.ReachAdmin, "trace": auth.ReachAdmin,
	"tokens": auth.ReachAdmin, "token_series": auth.ReachAdmin, "budgets": auth.ReachAdmin,
	"schedule_runs": auth.ReachAdmin, "sandbox_runs": auth.ReachAdmin,
	"sandbox_tail": auth.ReachAdmin, "page_reads": auth.ReachAdmin,
	"agent_memory": auth.ReachAdmin, "agent_episode": auth.ReachAdmin,
	"memory_overview": auth.ReachAdmin, "conversations": auth.ReachAdmin,
	"work_item_turns": auth.ReachAdmin, "access": auth.ReachAdmin,
	"backups": auth.ReachAdmin, "config": auth.ReachAdmin, "config_audit": auth.ReachAdmin,
	"config_diff": auth.ReachAdmin, "config_entities": auth.ReachAdmin,
	"credential_pool": auth.ReachAdmin, "fleet": auth.ReachAdmin,
	"fleet_broker": auth.ReachAdmin, "integrations": auth.ReachAdmin,
	"mcp_servers_status": auth.ReachAdmin, "retention": auth.ReachAdmin,
	// Which seat asked which, and how much they said: the machine's own
	// traffic, whose content is the event log's.
	"a2a_channels": auth.ReachAdmin,
}

// Present-but-inert implementations of every source, so [Register] registers
// every question it can. None is ever called: the case reads the registry's
// declarations and asks nothing.
type (
	channelLister interface {
		OpenChannels(ctx context.Context) ([]coord.Channel, error)
		AllChannels(ctx context.Context) ([]coord.Channel, error)
	}
	budgetReader interface {
		Usage(ctx context.Context, windows coord.Windows) ([]coord.Usage, error)
	}
	cooldownReader interface {
		Since(ctx context.Context, now time.Time) (map[string]time.Time, error)
	}
	backupReader interface {
		BackupPoints(ctx context.Context) ([]coord.BackupPoint, error)
	}
)

// everySource is a Sources with every source present.
func everySource() Sources {
	return Sources{
		State:        livestate.New(),
		Events:       struct{ FleetEvents }{},
		Usage:        struct{ usage.Estate }{},
		Company:      func() *config.Company { return nil },
		OperatorActs: func() []string { return nil },
		Coord:        struct{ coord.Backend }{},
		Plane:        struct{ coord.Plane }{},
		Objects:      struct{ engine.CollectionRecords }{},
		FleetBroker:  struct{ BrokerLister }{},
		Runs:         struct{ ScheduleRuns }{},
		Memory:       struct{ SeatMemory }{},
		Channels:     struct{ channelLister }{},
		Knowledge:    func() knowledge.Searcher { return nil },
		Budget:       struct{ budgetReader }{},
		Sandbox:      struct{ PendingRuns }{},
		SandboxTail:  struct{ SandboxTails }{},
		Config:       &configapi.Service{},
		Routed:       func(context.Context) []string { return nil },
		Verifiable:   func(context.Context) []string { return nil },
		Reconciles:   func(context.Context) []integration.State { return nil },
		Converges:    func(integration.Kind) bool { return false },
		Work:         struct{ WorkReader }{},
		Pages:        struct{ PageReader }{},
		Files:        struct{ FileReader }{},
		Backlinks:    struct{ PageBacklinks }{},
		WorkSearch:   struct{ WorkSearcher }{},
		PublicBase:   func() string { return "" },
		Retention:    func(context.Context) any { return nil },
		Access:       &AccessPosture{},
		CredentialPools: func() []engine.CredentialPool {
			return nil
		},
		Cooldowns: struct{ cooldownReader }{},
		Backups:   struct{ backupReader }{},
		NodeID:    "node-a",
		Now:       time.Now,
	}
}

// EVERY QUESTION IS REGISTERED AT THE REACH THE WITHHOLDING RULE GIVES IT, and
// the reviewed list names every question there is — in both directions, so
// neither a new question nor a moved reach can land unread.
func TestEveryQuestionIsRegisteredAtTheReachTheWithholdingRuleGivesIt(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	Register(r, everySource())
	names := r.Names()
	for _, name := range names {
		want, listed := questionReach[name]
		if !listed {
			t.Errorf("%q is registered at %q and is not in questionReach: classify it "+
				"by ADR-0031's withholding rule and list it", name, r.ReachOf(name))
			continue
		}
		if got := r.ReachOf(name); got != want {
			t.Errorf("%q is registered at %q, want %q", name, got, want)
		}
	}
	for name := range questionReach {
		if !slices.Contains(names, name) {
			t.Errorf("%q is listed and nothing registers it, even with every source present", name)
		}
	}
}
