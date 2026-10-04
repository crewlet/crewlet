package memread

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/learning"
)

// PageLimit bounds each collection of a memory answer, and is the page a
// caller that names none is given.
//
// FIFTY, and every collection asks its own store for that many rather than
// reading everything and cutting: the diary and the episodes are a recency
// feed, where "the most recent fifty" IS the question, and the skills and the
// profiles are ordered listings. Each travels with its TOTAL, counted in the
// store, because a panel that counted the rows it was given reported fifty for
// exactly the seats whose memory had grown past the page.
const PageLimit = 50

// DefaultThreadPage and MaxThreadPage bound the conversation listing.
//
// FIFTY by default, like every per-seat page; TWO HUNDRED at most, which is
// [learning.MaxProfilesListed]' figure for its own reason: the size of the
// company this engine is built for plus the correspondents one seat
// accumulates on the surfaces it works. A page size is not a filter, so one
// asked past the ceiling is served the ceiling rather than refused.
const (
	DefaultThreadPage = 50
	MaxThreadPage     = 200
)

// Page clamps a memory page size a caller asked for: an absent or nonsensical
// one takes [PageLimit], and one past it is served [PageLimit].
func Page(asked int) int {
	if asked <= 0 || asked > PageLimit {
		return PageLimit
	}
	return asked
}

// ThreadPage clamps a conversation page size, in the same idiom.
func ThreadPage(asked int) int {
	switch {
	case asked <= 0:
		return DefaultThreadPage
	case asked > MaxThreadPage:
		return MaxThreadPage
	}
	return asked
}

// Profiles is what a seat learned about the people it works with — the half
// of [learning.Counterparties] a read needs.
type Profiles interface {
	List(ctx context.Context, observer string, limit int) ([]learning.Profile, error)
	Count(ctx context.Context, observer string) (int, error)
}

// Stores is this node's own copy of seats' memory. A nil store answers its
// half empty, which is how a node that keeps no such half says so.
//
// KEYED AS EACH HALF IS WRITTEN: the diary and the onboarding marker on the
// seat's id, and the episodes, skills, profiles and conversation ledger on its
// handle — both off the [Seat] the asker resolved.
type Stores struct {
	Diary         *learning.Diary
	Episodes      *learning.Episodes
	Skills        *learning.Skills
	Profiles      Profiles
	Onboarding    *learning.Onboarding
	Conversations ledgerstore.Conversations
	// Now is the clock a diary entry's lapse is judged against; nil is the
	// wall clock.
	Now func() time.Time
}

func (s *Stores) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// agentID is the key a seat's diary and onboarding marker are written under,
// or "" for a seat with no id — which keeps neither.
func agentID(seat Seat) string {
	if seat.ID == uuid.Nil {
		return ""
	}
	return seat.ID.String()
}

// Memory is one seat's memory, as the `agent_memory` answer sends it.
//
// EVERY KEY ON EVERY ANSWER, as an empty list or a zero rather than an absent
// one: a client cannot tell "this seat has learned nothing" from "this answer
// does not carry that half" if the key is simply not there, and both are
// ordinary states.
type Memory struct {
	// ID is the handle the seat answers to now — the answer is about the
	// seat whatever it has been called.
	ID string `json:"id"`

	// Diary is the newest LIVE entries, newest first; DiaryTotal how many
	// live entries the seat holds.
	Diary      []DiaryRow `json:"diary"`
	DiaryTotal int        `json:"diary_total"`

	// Episodes is the newest episode rows; EpisodesTotal how many there are.
	Episodes      []EpisodeRow `json:"episodes"`
	EpisodesTotal int          `json:"episodes_total"`

	// Skills is a page of the skills it drafted, newest first; SkillsTotal
	// the whole set: archived hidden, stale shown, as the seat loads them.
	Skills      []SkillRow `json:"skills"`
	SkillsTotal int        `json:"skills_total"`

	// Counterparties is the people it learned about, most recently updated
	// first; CounterpartiesTotal how many it holds a profile of.
	Counterparties      []ProfileRow `json:"counterparties"`
	CounterpartiesTotal int          `json:"counterparties_total"`

	// LatestReflection is the newest live diary entry — what the seat most
	// recently concluded about its own work, by its own tool call or by the
	// post-turn reflection — whatever page was asked for; null when the diary
	// holds none. A profile's summary reads this rather than asking for a
	// page to take the first row of.
	LatestReflection *DiaryRow `json:"latest_reflection"`

	// OnboardedAt is when the seat first finished onboarding, or "" when it
	// has not (a pass claimed and not finished is not an onboarding).
	OnboardedAt string `json:"onboarded_at"`

	// HeldBy names the node whose store answered — the seat's holder — or
	// [HolderNone] for a seat no node holds.
	HeldBy string `json:"held_by"`
}

// DiaryRow is one diary entry.
type DiaryRow struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// Retention is `diary_long` or `diary_short` — whether it lapses.
	Retention string `json:"retention"`
	// Source is what wrote it: a learning worker, or the seat's own tool.
	Source    string `json:"source"`
	TurnID    string `json:"turn_id"`
	CreatedAt string `json:"created_at"`
	TTLUntil  string `json:"ttl_until"`
	// Retrievals is how often it has been recalled — the difference between
	// a memory that keeps proving useful and one written once and never read.
	Retrievals int `json:"retrievals"`
}

// EpisodeRow is one episode: a completed turn, summarised.
type EpisodeRow struct {
	ID              string   `json:"id"`
	TurnID          string   `json:"turn_id"`
	AgentHandle     string   `json:"agent_handle"`
	TaskSummary     string   `json:"task_summary"`
	PlanSummary     string   `json:"plan_summary"`
	ReviewOutcome   string   `json:"review_outcome"`
	ToolSequence    []string `json:"tool_sequence"`
	SkillsUsed      []string `json:"skills_used"`
	ConversationKey string   `json:"conversation_key"`
	WorkKey         string   `json:"work_key"`
	CreatedAt       string   `json:"created_at"`
	EndedAt         string   `json:"ended_at"`
	// DurationMs is milliseconds: a time.Duration marshals as NANOSECONDS,
	// which renders as a plausible and wildly wrong number.
	DurationMs int64 `json:"duration_ms"`
	// Compacted is a row that stands for a cluster of turns, Count of them.
	Compacted bool `json:"compacted"`
	Count     int  `json:"count"`
}

// SkillRow is one skill the seat drafted from its own repeated work.
type SkillRow struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Version   int    `json:"version"`
	UpdatedAt string `json:"updated_at"`
	Uses      int    `json:"uses"`
}

// ProfileRow is what one seat learned about one colleague.
//
// A COLLEAGUE IS NAMED BY THEIR HANDLE ([SubjectRow.Handle]), as the profile
// was stored.
//
// BOTH INSTANTS, because they measure different cadences: `last_updated_at`
// moves on every interaction and `last_corroborated_at` only when the traits
// changed, so a colleague seen daily whose profile has not moved in months is
// one this seat has stopped learning about — the state the prefetch demotes
// on, which a screen showing one number would contradict.
type ProfileRow struct {
	Subject            SubjectRow     `json:"subject"`
	Resolved           bool           `json:"resolved"`
	Traits             map[string]any `json:"traits"`
	Interactions       int            `json:"interactions"`
	FirstSeenAt        time.Time      `json:"first_seen_at"`
	LastUpdatedAt      time.Time      `json:"last_updated_at"`
	LastCorroboratedAt time.Time      `json:"last_corroborated_at"`
}

// SubjectRow is who a profile is about: a seat of this company, or an
// unmapped person on a surface. Each identity is sent only for its own kind.
type SubjectRow struct {
	Handle     string `json:"handle,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Platform   string `json:"platform,omitempty"`
	Name       string `json:"name"`
}

// Threads is one seat's conversation ledger, as the `conversations` answer
// sends it.
type Threads struct {
	Handle string `json:"handle"`
	// Conversations is a page of the threads the seat holds entries in,
	// newest activity first; ConversationsTotal how many it holds.
	Conversations      []ThreadRow `json:"conversations"`
	ConversationsTotal int         `json:"conversations_total"`
	// Entries is what the seat recorded in the ONE conversation asked for,
	// oldest first — empty when none was named.
	Entries []ledger.Session `json:"entries"`
	// HeldBy is as [Memory.HeldBy].
	HeldBy string `json:"held_by"`
}

// ThreadRow is one conversation a seat holds entries for.
type ThreadRow struct {
	Key    string    `json:"key"`
	Turns  int       `json:"turns"`
	LastAt time.Time `json:"last_at"`
}

// emptyMemory is the answer about a seat nobody holds, or a node that keeps no
// memory: every key, every list empty.
func emptyMemory(handle, heldBy string) Memory {
	return Memory{
		ID: handle, Diary: []DiaryRow{}, Episodes: []EpisodeRow{}, Skills: []SkillRow{},
		Counterparties: []ProfileRow{}, HeldBy: heldBy,
	}
}

func emptyThreads(handle, heldBy string) Threads {
	return Threads{
		Handle: handle, Conversations: []ThreadRow{}, Entries: []ledger.Session{},
		HeldBy: heldBy,
	}
}

// Memory reads one seat's memory from THIS node's store.
//
// AN ERROR IS NOT AN EMPTY HALF. Everything in `learning` is best effort for a
// TURN, which must not die because a diary was slow — but a screen reporting
// "this seat remembers nothing" when the store could not be reached is the
// collapse every three-valued answer in this engine exists to prevent.
func (s *Stores) Memory(ctx context.Context, seat Seat, limit int) (Memory, error) {
	out := emptyMemory(seat.Handle, "")
	limit = Page(limit)
	if s.Diary != nil {
		if id := agentID(seat); id != "" {
			now := s.now()
			entries, err := s.Diary.Recent(ctx, id, now, limit)
			if err != nil {
				return Memory{}, err
			}
			for _, e := range entries {
				out.Diary = append(out.Diary, diaryRow(e))
			}
			if out.DiaryTotal, err = s.Diary.Count(ctx, id, now); err != nil {
				return Memory{}, err
			}
			// THE NEWEST, whatever the page: a page of one is the newest
			// entry already, and a larger page is newest first.
			if len(out.Diary) > 0 {
				latest := out.Diary[0]
				out.LatestReflection = &latest
			}
		}
	}
	if s.Episodes != nil {
		episodes, err := s.Episodes.Recent(ctx, seat.Handle, limit)
		if err != nil {
			return Memory{}, err
		}
		for _, e := range episodes {
			out.Episodes = append(out.Episodes, episodeRow(e))
		}
		if out.EpisodesTotal, err = s.Episodes.Count(ctx, seat.Handle); err != nil {
			return Memory{}, err
		}
	}
	if s.Skills != nil {
		// ONE options value feeds the count and the page, so the two
		// describe the same set: archived hidden, stale shown — a stale
		// skill still works and still revives on use.
		opts := learning.ListOptions{}
		total, err := s.Skills.Count(ctx, seat.Handle, opts)
		if err != nil {
			return Memory{}, err
		}
		opts.Limit = limit
		skills, err := s.Skills.List(ctx, seat.Handle, opts)
		if err != nil {
			return Memory{}, err
		}
		out.SkillsTotal = total
		for _, sk := range skills {
			out.Skills = append(out.Skills, skillRow(sk))
		}
	}
	if s.Profiles != nil {
		profiles, err := s.Profiles.List(ctx, seat.Handle, limit)
		if err != nil {
			return Memory{}, fmt.Errorf("memread: read what %s learned about its "+
				"colleagues: %w", seat.Handle, err)
		}
		for _, p := range profiles {
			out.Counterparties = append(out.Counterparties, profileRow(p))
		}
		if out.CounterpartiesTotal, err = s.Profiles.Count(ctx, seat.Handle); err != nil {
			return Memory{}, err
		}
	}
	if s.Onboarding != nil {
		if id := agentID(seat); id != "" {
			marker, found, err := s.Onboarding.Get(ctx, id)
			if err != nil {
				return Memory{}, err
			}
			// A ROW WITH NO CHAIN is a pass that was claimed and never
			// finished, which is not an onboarding. Its created_at is when
			// the seat first onboarded: a re-onboarding keeps it.
			if found && marker.ChainHash != "" {
				out.OnboardedAt = iso(marker.CreatedAt)
			}
		}
	}
	return out, nil
}

// Threads reads one seat's conversation ledger from THIS node's store — a
// page of its threads and, when one is named, what it recorded there.
func (s *Stores) Threads(ctx context.Context, seat Seat, conversation string, limit int) (Threads, error) {
	out := emptyThreads(seat.Handle, "")
	if s.Conversations == nil {
		return out, nil
	}
	limit = ThreadPage(limit)
	threads, err := s.Conversations.Threads(ctx, seat.Handle, limit)
	if err != nil {
		return Threads{}, err
	}
	for _, t := range threads {
		out.Conversations = append(out.Conversations,
			ThreadRow{Key: t.Key, Turns: t.Entries, LastAt: t.LastAt})
	}
	if out.ConversationsTotal, err = s.Conversations.ThreadCount(ctx, seat.Handle); err != nil {
		return Threads{}, err
	}
	if conversation != "" {
		entries, err := s.Conversations.History(ctx, seat.Handle, conversation, limit)
		if err != nil {
			return Threads{}, err
		}
		if entries != nil {
			out.Entries = entries
		}
	}
	return out, nil
}

// iso renders an instant, or "" for the zero one — which would print as 1970
// and read as a real answer.
func iso(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func diaryRow(e learning.DiaryEntry) DiaryRow {
	return DiaryRow{
		ID: e.ID, Content: e.Content, Retention: string(e.Kind), Source: e.Source,
		TurnID: e.TurnID, CreatedAt: iso(e.CreatedAt), TTLUntil: iso(e.TTLUntil),
		Retrievals: e.RetrievalCount,
	}
}

func episodeRow(e learning.Episode) EpisodeRow {
	return EpisodeRow{
		ID: e.ID, TurnID: e.TurnID, AgentHandle: e.Handle,
		TaskSummary: e.TaskSummary, PlanSummary: e.PlanSummary,
		ReviewOutcome: e.ReviewOutcome, ToolSequence: e.ToolSequence,
		SkillsUsed: e.SkillsUsed, ConversationKey: e.ConversationKey,
		WorkKey: e.WorkKey, CreatedAt: iso(e.StartedAt), EndedAt: iso(e.EndedAt),
		DurationMs: e.Duration.Milliseconds(), Compacted: e.Kind != learning.KindRaw,
		Count: e.Count,
	}
}

func skillRow(sk learning.Skill) SkillRow {
	return SkillRow{
		ID: sk.ID, Key: sk.Name, Title: sk.Name, Summary: sk.Description,
		Version: sk.Version, UpdatedAt: iso(sk.UpdatedAt), Uses: sk.UseCount,
	}
}

func profileRow(p learning.Profile) ProfileRow {
	subject := SubjectRow{Name: p.Subject.Name, Handle: p.Subject.Handle}
	if p.Subject.ExternalID != "" {
		subject.ExternalID = p.Subject.ExternalID
		subject.Platform = p.Subject.Platform
	}
	traits := p.Traits
	if traits == nil {
		// AN EMPTY MAP, never null: a profile whose traits failed to
		// decode and one that has none read the same to a client that has
		// to guard the field either way.
		traits = map[string]any{}
	}
	return ProfileRow{
		Subject: subject, Resolved: p.Subject.Resolved(), Traits: traits,
		Interactions: p.InteractionCount, FirstSeenAt: p.FirstSeenAt,
		LastUpdatedAt: p.LastUpdatedAt, LastCorroboratedAt: p.LastCorroboratedAt,
	}
}
