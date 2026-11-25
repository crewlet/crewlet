package worklet

import (
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// A credential acting as no seat files and watches items but holds no Inbox
// and no turn, so the writer leaves it out of a snapshot's recipients and the
// applier writes it no notice; the item still names it as reporter.

// withoutCredentials is n with no credential among the handles its snapshot
// offers as recipients; nil stays nil.
func (n *Notify) withoutCredentials() *Notify {
	if n == nil {
		return nil
	}
	out := *n
	s := &out.Snapshot
	for _, handle := range []*string{&s.Assignee, &s.Reporter, &s.PrevAssignee, &s.CommentAsk,
		&s.AnsweredAuthor, &s.ParentAssignee, &s.RoutedTo, &s.DepartureLead} {
		if isCredential(*handle) {
			*handle = ""
		}
	}
	for _, list := range []*[]string{&s.Watchers, &s.Collaborators, &s.ThreadParticipants, &s.ThreadOwing,
		&s.ThreadAwaiting, &s.RemovedWatchers, &s.ChecklistAssignees, &out.Mentions} {
		if slices.ContainsFunc(*list, isCredential) {
			*list = slices.DeleteFunc(slices.Clone(*list), isCredential)
		}
	}
	for _, parties := range []*[]TaskParty{&s.Unblocked, &s.Dependents} {
		if slices.ContainsFunc(*parties, func(p TaskParty) bool { return isCredential(p.Assignee) }) {
			*parties = slices.Clone(*parties)
			for i := range *parties {
				if isCredential((*parties)[i].Assignee) {
					(*parties)[i].Assignee = ""
				}
			}
		}
	}
	return &out
}

// isCredential reports whether handle names a credential rather than a seat:
// a machine handle, which no seat handle and no person's login can be.
func isCredential(handle string) bool { return iam.ValidMachineHandle(handle) }
