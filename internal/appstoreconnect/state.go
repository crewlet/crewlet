package appstoreconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// The states a write's class is lowered in are read from the target, or the
// container it belongs to, just before the call; a state the engine cannot
// read keeps the stricter class (growth.StateRule).

// draftStates are the states of a container customers do not see yet: an
// editable version, a draft event, page or experiment, a product never
// approved.
var draftStates = []string{"PREPARE_FOR_SUBMISSION", "READY_FOR_REVIEW", "DEVELOPER_REJECTED", "REJECTED",
	"METADATA_REJECTED", "INVALID_BINARY", "DRAFT", "MISSING_METADATA", "WAITING_FOR_UPLOAD", "READY_TO_SUBMIT",
	"DEVELOPER_ACTION_NEEDED"}

// stateFields are the attributes Apple names a resource's state by.
var stateFields = []string{"appVersionState", "appStoreState", "eventState", "state"}

// maxStateReads bounds the reads one state check makes: the target and up to
// three containers above it.
const maxStateReads = 4

// needState refuses m unless its target is in the state that lowers its
// operation to the class m is called with.
func (p *Product) needState(ctx context.Context, m *made) error {
	i := slices.IndexFunc(m.op.States, func(s growth.StateRule) bool { return s.Class == m.class })
	if i < 0 {
		return invalid("%s is %s-class. %s", m.op.ID, m.op.Class, Catalog().WayTo(m.op.Class, m.level))
	}
	rule := m.op.States[i]
	holds, err := p.inState(ctx, m, rule.When)
	if err != nil || !holds {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s is %s here: what it changes is not %s (or "+
			"its state could not be read), so customers or external testers would see it. %s No call was made.",
			m.op.ID, m.op.Class, stateWords[rule.When], Catalog().WayTo(m.op.Class, m.level)))
	}
	return nil
}

var stateWords = map[string]string{stateDraft: "a draft", stateInternal: "internal testing only",
	stateLive: "live"}

// inState reports whether m's target is in state.
func (p *Product) inState(ctx context.Context, m *made, state string) (bool, error) {
	switch state {
	case stateInternal:
		return p.internalOnly(ctx, m)
	case stateDraft, stateLive:
		s, found, err := p.stateOf(ctx, m)
		if err != nil || !found {
			return false, err
		}
		draft := slices.Contains(draftStates, s)
		if state == stateDraft {
			return draft, nil
		}
		return !draft, nil
	}
	return false, nil
}

// stateOf is the first state attribute on m's target or the containers its
// relationships name, read through m's key.
func (p *Product) stateOf(ctx context.Context, m *made) (string, bool, error) {
	var queue []string
	if typ, id := typeAndID(m.path); id != "" {
		queue = append(queue, "/v1/"+typ+"/"+url.PathEscape(id))
	} else if ref, ok := bodyTarget(m.body); ok {
		queue = append(queue, "/v1/"+ref.Type+"/"+url.PathEscape(ref.ID))
	}
	for reads := 0; len(queue) > 0 && reads < maxStateReads; reads++ {
		path := queue[0]
		queue = queue[1:]
		if err := m.reader(path); err != nil {
			return "", false, err
		}
		resp, err := m.do(ctx, Request{Method: "GET", Path: path, Read: true, Reserved: true})
		if err != nil {
			return "", false, err
		}
		var doc struct {
			Data struct {
				Attributes    map[string]any `json:"attributes"`
				Relationships map[string]struct {
					Links struct {
						Related string `json:"related"`
					} `json:"links"`
				} `json:"relationships"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &doc); err != nil {
			return "", false, fmt.Errorf("appstoreconnect: read %s: %w", path, err)
		}
		for _, f := range stateFields {
			if s, ok := doc.Data.Attributes[f].(string); ok && s != "" {
				return s, true, nil
			}
		}
		for _, name := range containerRelations {
			if rel, ok := doc.Data.Relationships[name]; ok && rel.Links.Related != "" {
				if u, err := url.Parse(rel.Links.Related); err == nil && strings.HasPrefix(u.Path, "/v") {
					queue = append(queue, u.Path)
				}
			}
		}
	}
	return "", false, nil
}

// containerRelations are the relationships that lead from a resource to the
// container whose state decides what customers see, nearest first.
var containerRelations = []string{"appStoreVersion", "appCustomProductPageVersion", "appStoreVersionExperiment",
	"appStoreVersionExperimentTreatment", "appStoreVersionLocalization", "appCustomProductPageLocalization",
	"appEvent", "appEventLocalization", "appInfo", "inAppPurchaseV2", "inAppPurchase", "subscription",
	"subscriptionGroup", "retentionMessageVersion", "appScreenshotSet", "appPreviewSet"}

// internalOnly reports whether every beta group m names is internal.
func (p *Product) internalOnly(ctx context.Context, m *made) (bool, error) {
	var groups []string
	if typ, id := typeAndID(m.path); typ == "betaGroups" && id != "" {
		groups = append(groups, id)
	}
	data, _ := m.body["data"].([]any)
	for _, d := range data {
		r, _ := d.(map[string]any)
		if t, _ := r["type"].(string); t == "betaGroups" {
			if id, _ := r["id"].(string); id != "" {
				groups = append(groups, id)
			}
		}
	}
	if len(groups) == 0 {
		return false, nil
	}
	for _, g := range groups {
		path := "/v1/betaGroups/" + url.PathEscape(g)
		if err := m.reader(path); err != nil {
			return false, err
		}
		resp, err := m.do(ctx, Request{Method: "GET", Path: path,
			Query: url.Values{"fields[betaGroups]": {"isInternalGroup"}}, Read: true, Reserved: true})
		if err != nil {
			return false, err
		}
		var doc struct {
			Data struct {
				Attributes struct {
					IsInternalGroup bool `json:"isInternalGroup"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &doc); err != nil {
			return false, fmt.Errorf("appstoreconnect: read beta group %s: %w", g, err)
		}
		if !doc.Data.Attributes.IsInternalGroup {
			return false, nil
		}
	}
	return true, nil
}

// reader refuses an engine read of path for m that the agent's key's role
// may not make: Crewlet reads with no other key ([ErrRole]).
func (m made) reader(path string) error {
	if mayRead(m.key.role, path) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRole, path)
}
