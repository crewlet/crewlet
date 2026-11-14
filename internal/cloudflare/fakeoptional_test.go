package cloudflare_test

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/cloudflare"
)

// optionalRoutes is each optional listing's route key, by its path under the
// account, and optionalGroups the permission group each takes.
var (
	optionalRoutes = map[string]string{
		"/workers/scripts": "workers", "/pages/projects": "pages", "/r2/buckets": "r2", "/cfd_tunnel": "tunnels",
	}
	optionalGroups = map[string]string{
		"workers": "Workers Scripts Read", "pages": "Pages Read",
		"r2": "Workers R2 Storage Read", "tunnels": "Cloudflare Tunnel Read",
	}
)

// holds reports whether the provisioner token's own policy allows a group.
func (f *fakeCloudflare) holds(group string) bool {
	return slices.ContainsFunc(f.provisionerOwn, func(p cloudflare.Policy) bool {
		return p.Effect == "allow" && slices.ContainsFunc(p.PermissionGroups, func(g cloudflare.GroupRef) bool {
			return g.ID == groupID(group)
		})
	})
}

// without takes permission groups out of the provisioner token's own policy,
// as a token made before the atlas could use them, or one somebody narrowed.
func (f *fakeCloudflare) without(groups ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.provisionerOwn {
		f.provisionerOwn[i].PermissionGroups = slices.DeleteFunc(slices.Clone(f.provisionerOwn[i].PermissionGroups),
			func(g cloudflare.GroupRef) bool {
				return slices.ContainsFunc(groups, func(name string) bool { return groupID(name) == g.ID })
			})
	}
}

// serveOptional answers one optional listing: Cloudflare's 403 and code
// 10000 to a token whose policy lacks the listing's group, and otherwise the
// rows the test planted, each in the paging its own route has.
func (f *fakeCloudflare) serveOptional(w http.ResponseWriter, r *http.Request, route string) {
	if !f.holds(optionalGroups[route]) {
		f.fail(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	rows := func(in []map[string]any) []any {
		out := make([]any, 0, len(in))
		for _, row := range in {
			out = append(out, row)
		}
		return out
	}
	q := r.URL.Query()
	switch route {
	case "workers":
		// EVERY SCRIPT IN ONE ANSWER, with no paging.
		f.ok(w, rows(f.workers), nil)
	case "pages":
		if size, _ := strconv.Atoi(q.Get("per_page")); size > 10 {
			f.fail(w, http.StatusBadRequest, 8000024, "Invalid list options provided. Review the page or per_page parameter.")
			return
		}
		f.page(w, r, rows(f.pages))
	case "r2":
		// A CURSOR, never a page number: the cursor is where the next page starts.
		size, _ := strconv.Atoi(q.Get("per_page"))
		if f.r2PageSize > 0 {
			size = min(size, f.r2PageSize)
		}
		start, _ := strconv.Atoi(q.Get("cursor"))
		all := rows(f.buckets)
		end := min(start+max(size, 1), len(all))
		info := map[string]any{"per_page": size}
		if end < len(all) {
			info["cursor"] = strconv.Itoa(end)
		}
		f.ok(w, map[string]any{"buckets": all[min(start, len(all)):end]}, info)
	case "tunnels":
		kept := f.tunnels
		if q.Get("is_deleted") == "false" {
			kept = slices.DeleteFunc(slices.Clone(kept), func(row map[string]any) bool { return row["deleted_at"] != nil })
		}
		f.page(w, r, rows(kept))
	}
}
