package googleplay

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/growth"
)

const opTracksPatch = "androidpublisher.edits.tracks.patch"

// releaseAct is google_play_release's act: one release of a track, staged
// and committed in one edit. A release reaching users is public; Expo or a
// build uploaded made its version codes, which this never uploads.
func (p *Product) releaseAct(call growth.Call) (act, error) {
	app, _ := call.Args["app"].(string)
	track, _ := call.Args["track"].(string)
	status, _ := call.Args["status"].(string)
	codes := stringList(call.Args["version_codes"])
	if len(codes) == 0 || !slices.Contains([]string{"inProgress", "completed"}, status) {
		return act{}, refusal(growth.RefusedInvalid, "a release names its version_codes and a status, inProgress "+
			"or completed; a draft is google_play_operate's androidpublisher.edits.tracks.patch. No vendor was reached.")
	}
	release := map[string]any{"versionCodes": codes, "status": status}
	if f, ok := call.Args["user_fraction"].(float64); ok {
		if status != "inProgress" || f <= 0 || f >= 1 {
			return act{}, refusal(growth.RefusedInvalid, "user_fraction is above 0 and below 1, for an inProgress "+
				"release. No vendor was reached.")
		}
		release["userFraction"] = f
	} else if status == "inProgress" {
		return act{}, refusal(growth.RefusedInvalid, "an inProgress release names its user_fraction. No vendor "+
			"was reached.")
	}
	if name, _ := call.Args["name"].(string); name != "" {
		release["name"] = name
	}
	if notes, _ := call.Args["release_notes"].([]any); len(notes) > 0 {
		var out []any
		for i, n := range notes {
			obj, _ := n.(map[string]any)
			lang, _ := obj["language"].(string)
			text, _ := obj["text"].(string)
			if lang == "" || text == "" || utf8.RuneCountInString(text) > notesLimit {
				return act{}, refusal(growth.RefusedInvalid, fmt.Sprintf("release_notes[%d] names a language and "+
					"1 to %d characters. No vendor was reached.", i, notesLimit))
			}
			out = append(out, map[string]any{"language": lang, "text": text})
		}
		release["releaseNotes"] = out
	}
	if countries := stringList(call.Args["countries"]); len(countries) > 0 {
		release["countryTargeting"] = map[string]any{"countries": countries}
	}
	if pr, ok := call.Args["in_app_update_priority"].(float64); ok {
		release["inAppUpdatePriority"] = int(pr)
	}
	return p.trackAct(app, track, []any{release}, "release "+strings.Join(codes, ", ")+" on "+track+" as "+status)
}

// haltAct is google_play_halt_release's act: the track's release rolling out
// (or the one its version codes name) halted, its other fields kept. It is
// read as the edit reads the track, so the halt changes nothing else.
func (p *Product) haltAct(call growth.Call) (act, error) {
	app, _ := call.Args["app"].(string)
	track, _ := call.Args["track"].(string)
	codes := stringList(call.Args["version_codes"])
	a, err := p.trackAct(app, track, nil, "halt the rollout on "+track)
	if err != nil {
		return a, err
	}
	a.halting, a.halt = true, codes
	return a, nil
}

// shapeHalt writes a halt's track from the track as its edit reads it: every
// release kept, the one rolling out (or the one the call names) halted.
func shapeHalt(a *act) error {
	if !a.halting {
		return nil
	}
	var current map[string]any
	_ = json.Unmarshal(a.steps[0].before, &current)
	releases, _ := current["releases"].([]any)
	var out []any
	halted := 0
	for _, r := range releases {
		rel, _ := r.(map[string]any)
		copied := normalCopy(rel)
		named := len(a.halt) > 0 && codeKey(rel["versionCodes"]) == codeKey(toAny(a.halt))
		if (named || len(a.halt) == 0) && rel["status"] == "inProgress" {
			copied["status"] = "halted"
			halted++
		}
		out = append(out, copied)
	}
	if halted != 1 {
		return refusal(growth.RefusedInvalid, "the track holds no single release rolling out to halt; name its "+
			"version_codes (google_play_read_tracks lists them). Nothing was changed.")
	}
	a.steps[0].req.Body = map[string]any{"track": a.steps[0].req.Params["track"], "releases": out}
	return nil
}

// normalCopy is a deep copy of a release as JSON reads it.
func normalCopy(m map[string]any) map[string]any {
	data, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

// toAny is a string list as JSON decodes one.
func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// trackAct is a patch of track's releases, committed in one edit.
func (p *Product) trackAct(app, track string, releases []any, summary string) (act, error) {
	op, _ := p.Catalog().Operation(opTracksPatch)
	req, err := newRequest(op, map[string]string{"packageName": app, "track": track}, nil,
		map[string]any{"track": track, "releases": releases})
	if err != nil {
		return act{}, err
	}
	return act{app: app, edit: true, steps: []step{{req: req}}, summary: summary}, nil
}

// draftsOrHalts reports whether a track change only drafts or halts: every
// release it sets is a draft, a halt of a release now rolling out or halted,
// or one the track holds unchanged. A release it leaves out is kept by Google
// unless a new live release replaces it, which is public itself.
func draftsOrHalts(current, body map[string]any) bool {
	now, _ := current["releases"].([]any)
	next, _ := body["releases"].([]any)
	for _, r := range next {
		rel, _ := r.(map[string]any)
		status, _ := rel["status"].(string)
		match := releaseOf(now, rel["versionCodes"])
		switch {
		case status == "draft":
		case status == "halted" && match != nil && slices.Contains([]string{"inProgress", "halted"},
			fmt.Sprint(match["status"])) && sameExcept(match, rel, "status"):
		case match != nil && reflect.DeepEqual(normal(match), normal(rel)):
		default:
			return false
		}
	}
	return len(next) > 0
}

// releaseOf is the release of releases whose version codes are codes.
func releaseOf(releases []any, codes any) map[string]any {
	want := codeKey(codes)
	for _, r := range releases {
		rel, _ := r.(map[string]any)
		if want != "" && codeKey(rel["versionCodes"]) == want {
			return rel
		}
	}
	return nil
}

// codeKey is a list of version codes as one sorted key.
func codeKey(v any) string {
	list, _ := v.([]any)
	var out []string
	for _, c := range list {
		out = append(out, fmt.Sprint(c))
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// sameExcept reports whether two releases differ in field alone.
func sameExcept(a, b map[string]any, field string) bool {
	x, y := normal(a), normal(b)
	delete(x, field)
	delete(y, field)
	return reflect.DeepEqual(x, y)
}

// normal is a release as JSON reads it, its version codes one key, for
// comparing.
func normal(m map[string]any) map[string]any {
	out := normalCopy(m)
	if codes, ok := out["versionCodes"]; ok {
		out["versionCodes"] = codeKey(codes)
	}
	return out
}

// stringList is an argument array of strings, empty ones dropped.
func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
