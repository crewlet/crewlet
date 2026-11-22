package appleads

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Every object a request names is held to the seat's ad accounts, not only
// the one its path names: Apple does not document refusing another account's
// object under an X-AP-Context, so the engine reads each one and checks the ad
// account it answers (package doc, "Roles and scope").

// refPaths are the body fields naming one object of an ad account, and where
// it is read; a field ending in Ids names several.
var refPaths = map[string]string{"campaignId": "/v1/campaigns/", "adGroupId": "/v1/adgroups/",
	"adId": "/v1/ads/", "creativeId": "/v1/creatives/", "keywordId": "/v1/keywords/",
	"negativeKeywordId": "/v1/negative-keywords/", "productPageId": "/v1/product-pages/",
	"locationGroupId": "/v1/location-groups/", "assetId": "/v1/assets/", "budgetId": "/v1/shared-budgets/"}

// bulkIDs are where a bulk operation's items name their object by `id`.
var bulkIDs = map[string]string{"keywordsBulkUpdatePost": "/v1/keywords/", "keywordsBulkDeletePost": "/v1/keywords/",
	"negativeKeywordsBulkUpdatePost": "/v1/negative-keywords/",
	"negativeKeywordsBulkDeletePost": "/v1/negative-keywords/"}

// maxRefs bounds the objects one request may name: each is read once before
// the request is made.
const maxRefs = 200

// refsOf are the objects t names beyond its path, as the paths they read at,
// or a refusal of an ad account ID other than m's.
func refsOf(m made, t target) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(prefix string, v any) {
		id := strings.TrimSpace(idText(v))
		if v == nil || id == "" || seen[prefix+id] {
			return
		}
		seen[prefix+id] = true
		out = append(out, prefix+id)
	}
	var bad error
	var walk func(parent string, v any)
	walk = func(parent string, v any) {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				walk(parent, item)
			}
		case map[string]any:
			for k, child := range x {
				switch {
				case k == "adAccountId" && m.account != "" && idText(child) != m.account:
					bad = fmt.Errorf("%w: the body names ad account %v, not %s", ErrOtherAccount, child, m.account)
				case refPaths[k] != "":
					add(refPaths[k], child)
				case strings.HasSuffix(k, "Ids") && refPaths[strings.TrimSuffix(k, "s")] != "":
					ids, _ := child.([]any)
					for _, id := range ids {
						add(refPaths[strings.TrimSuffix(k, "s")], id)
					}
				case k == "include" && parent == "locationGroup":
					ids, _ := child.([]any)
					for _, id := range ids {
						add("/v1/location-groups/", id)
					}
				case k == "id" && parent == "data" && bulkIDs[t.op.ID] != "":
					add(bulkIDs[t.op.ID], child)
				default:
					walk(k, child)
				}
			}
		}
	}
	walk("", t.body)
	if bad != nil {
		return nil, bad
	}
	if len(out) > maxRefs {
		return nil, invalid("the request names %d objects, and at most %d are checked before it is made.", len(out), maxRefs)
	}
	return out, nil
}

// parentOf is the object a sub-resource's path names (a campaign's legacy
// status details), "" for a path that names none or names it whole.
func parentOf(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 {
		return ""
	}
	prefix := "/v1/" + parts[1] + "/"
	for _, known := range refPaths {
		if known == prefix {
			return prefix + parts[2]
		}
	}
	return ""
}

// checkRefs reads every object t names beyond the target its path names and
// refuses one of an ad account other than m's, or a budget order naming an ad
// account outside the seat's.
func (p *Product) checkRefs(ctx context.Context, m made, t target) error {
	refs, err := refsOf(m, t)
	if err != nil {
		return err
	}
	if parent := parentOf(t.path); parent != "" {
		refs = append(refs, parent)
	}
	for _, path := range refs {
		account := m.account
		if strings.HasPrefix(path, "/v1/shared-budgets/") {
			// A BUDGET ORDER IS THE ORGANIZATION'S, read with no X-AP-Context.
			account = ""
		}
		resp, err := m.conn.client.Do(ctx, Request{Method: http.MethodGet, Path: path, Account: account, Read: true,
			Reserved: true})
		if err != nil {
			return err
		}
		var doc struct {
			Result map[string]any `json:"result"`
		}
		if err := decode(resp.Body, &doc); err != nil || doc.Result == nil {
			return growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("%s could not be read, so the request is not "+
				"made. No call was made.", path))
		}
		if err := m.owns(doc.Result); err != nil {
			return err
		}
	}
	return nil
}
