package googleplay

import (
	"context"
	"net/http"
	"net/url"
	"slices"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// maxAppPages bounds the apps listing the engine reads whole: 1,000 a page.
const maxAppPages = 20

// App is one app an agent may reach.
type App struct {
	Package string `json:"package"`
	Title   string `json:"title" untrusted:"name"`
}

// Apps is the apps tool's answer: the agent's apps its identity sees, and
// the ones of its list it does not see yet (not invited, or no upload).
type Apps struct {
	Apps     []App    `json:"apps"`
	NotShown []string `json:"not_shown,omitempty"`
}

// listApps lists the apps of the agent's list its identity sees.
func (p *Product) listApps(ctx context.Context, call growth.Call) (any, error) {
	b := p.seat(call.Handle)
	if b == nil {
		return nil, refusal(growth.RefusedForbidden, "this agent is off at Google Play. No vendor was reached.")
	}
	op, _ := p.Catalog().Operation("playdeveloperreporting.apps.search")
	out := Apps{Apps: []App{}}
	seen := map[string]bool{}
	token := ""
	for range maxAppPages {
		q := map[string]string{"pageSize": "1000"}
		if token != "" {
			q["pageToken"] = token
		}
		req, err := newRequest(op, nil, q, nil)
		if err != nil {
			return nil, err
		}
		raw, err := p.client.Send(ctx, call.Handle, req)
		p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Apps []struct {
				PackageName string `json:"packageName"`
				DisplayName string `json:"displayName"`
			} `json:"apps"`
			Next string `json:"nextPageToken"`
		}
		if err := decode(raw, "the apps", &doc); err != nil {
			return nil, err
		}
		for _, a := range doc.Apps {
			if slices.Contains(b.Apps, a.PackageName) && p.company().Reaches(a.PackageName) && !seen[a.PackageName] {
				seen[a.PackageName] = true
				out.Apps = append(out.Apps, App{Package: a.PackageName, Title: a.DisplayName})
			}
		}
		if token = doc.Next; token == "" {
			break
		}
	}
	for _, app := range b.Apps {
		if !seen[app] {
			out.NotShown = append(out.NotShown, app)
		}
	}
	return out, nil
}

// list is every app the token sees, by its package and name, as the
// Developer Reporting API's search lists them.
func list(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var out []googlegrowth.Listed
	token := ""
	for range maxAppPages {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Apps []struct {
				PackageName string `json:"packageName"`
				DisplayName string `json:"displayName"`
			} `json:"apps"`
			Next string `json:"nextPageToken"`
		}
		if err := in.Send.Do(ctx, http.MethodGet, reportingAPI+"/v1beta1/apps:search?"+q.Encode(), in.Token, nil,
			&page); err != nil {
			return nil, err
		}
		for _, a := range page.Apps {
			out = append(out, googlegrowth.Listed{ID: a.PackageName, Name: a.DisplayName})
		}
		if token = page.Next; token == "" {
			break
		}
	}
	return out, nil
}
