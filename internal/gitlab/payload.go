// Package gitlab is the code-host integration: the forge agents review,
// merge and ship through.
package gitlab

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("gitlab")

// Backend is the transport name.
const Backend = "gitlab"

// Reading a GitLab payload.
//
// # Typed, where the tracker's is not
//
// A parser reads its payloads through map lookups when the vendor's
// serializer shapes are not something it can rely on. GitLab's webhook
// payloads are the opposite: documented, versioned, and stable across
// releases — so they decode into structs, and every field a branch reads is
// declared where the next reader can see it rather than spelled as a string
// at the point of use.
//
// A field GitLab adds is ignored rather than fatal, which is what makes an
// upgrade a non-event: encoding/json drops unknown keys by default and this
// deliberately does not turn that off.

// hook is one delivery, across every object kind this parser handles.
//
// ONE struct rather than one per kind. The kinds overlap heavily — every one
// carries a project, an actor and object_attributes — and a discriminated
// union would need the same fields declared four times, with four chances
// for one of them to be spelled differently.
type hook struct {
	Kind    string `json:"object_kind"`
	Attrs   attrs  `json:"object_attributes"`
	User    user   `json:"user"`
	Project struct {
		ID   int    `json:"id"`
		Path string `json:"path_with_namespace"`
	} `json:"project"`
	// ProjectID is the top-level fallback. The Note hook carries the
	// project id here as well as under project, and older releases carried
	// it ONLY here — a participants lookup keyed on the wrong one silently
	// returns nobody.
	ProjectID int `json:"project_id"`

	Assignees []user `json:"assignees"`
	Reviewers []user `json:"reviewers"`

	// MergeRequest and Issue are the NOTEABLE on a Note hook: the thing
	// commented on, carried alongside the note itself.
	MergeRequest noteable `json:"merge_request"`
	Issue        noteable `json:"issue"`

	Changes changes `json:"changes"`
}

type user struct {
	Username string `json:"username"`
}

type attrs struct {
	Action       string `json:"action"`
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	Status       string `json:"status"`
	Note         string `json:"note"`
	NoteableType string `json:"noteable_type"`
}

type noteable struct {
	IID       int    `json:"iid"`
	Title     string `json:"title"`
	Assignees []user `json:"assignees"`
}

// changes is the before/after diff an update hook carries.
//
// Only the three this parser routes on. GitLab sends a diff entry for every
// field that moved — labels, milestone, due date, updated_at — and none of
// the others names a person to notify.
type changes struct {
	Assignees   userDiff `json:"assignees"`
	Reviewers   userDiff `json:"reviewers"`
	Description textDiff `json:"description"`
}

type userDiff struct {
	Previous []user `json:"previous"`
	Current  []user `json:"current"`
}

type textDiff struct {
	Previous string `json:"previous"`
	Current  string `json:"current"`
}

// decode reads a delivery.
//
// PREFERS THE RAW BYTES, which are the ones the signature was checked
// against and the only fully faithful copy: a payload that made a round trip
// through a map has had every number turned into a float, which is how an
// issue iid of 42 comes back as "42" in one place and "42.000000" in
// another. The map is the fallback for a delivery published from inside the
// engine, where there are no bytes because nothing arrived over a wire.
func decode(w types.RawWebhook) (hook, error) {
	var h hook
	raw := w.BodyRaw
	if len(raw) == 0 {
		encoded, err := json.Marshal(w.Body)
		if err != nil {
			return hook{}, fmt.Errorf("gitlab: re-encode delivery: %w", err)
		}
		raw = encoded
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return hook{}, fmt.Errorf("gitlab: decode delivery: %w", err)
	}
	return h, nil
}

// projectID is the project this delivery belongs to, from either place it
// can appear.
func (h hook) projectID() int {
	if h.Project.ID != 0 {
		return h.Project.ID
	}
	return h.ProjectID
}

// actor is who caused the event, lowercased.
//
// LOWERCASED EVERYWHERE, at every boundary, because GitLab preserves the
// case a username was created with and echoes whatever case a mention was
// typed in — so "@Ana" in a comment and "ana" on an assignee list are one
// person, and a parser comparing them raw wakes her twice or not at all.
func (h hook) actor() string { return strings.ToLower(h.User.Username) }

// usernames reads a users array, lowercased, skipping the empty entries a
// removed member leaves behind.
func usernames(users []user) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if name := strings.ToLower(strings.TrimSpace(u.Username)); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// added is who is newly present in a diff's current list.
//
// THE DIFF, NOT THE LIST. An update hook carries the whole assignee list
// every time any field changes, so routing to the list would ping every
// assignee each time somebody moved a label. Being ADDED is the moment the
// work becomes yours; being removed is not a task.
func added(d userDiff) []string {
	previous := make(map[string]bool, len(d.Previous))
	for _, name := range usernames(d.Previous) {
		previous[name] = true
	}
	var out []string
	for _, name := range usernames(d.Current) {
		if !previous[name] {
			out = append(out, name)
		}
	}
	return out
}
