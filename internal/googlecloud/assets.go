package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
)

// What each project runs, searched in Cloud Asset Inventory as the atlas's
// own service account (ADR-0149): crewlet-atlas in the identity project,
// which the identity block makes where the company asks for it, impersonated
// with the federated token itself, so Google's audit logs name the atlas and
// never the provisioner or an agent.
//
// THE ACCOUNT HOLDS ONE PERMISSION, cloudasset.assets.searchAllResources, in
// a role of its own on each project, never Cloud Asset Viewer: that role also
// lists and exports assets whole, and a whole asset is a function's
// environment or an instance's startup script. The search names an explicit
// read mask, so the one call that could return a resource's configuration
// (versionedResources) is never asked for it.

// AtlasAccountID is the atlas's service account's id in the identity project.
const AtlasAccountID = "crewlet-atlas"

// AtlasAccountIn is the atlas's service account in the identity project.
func AtlasAccountIn(identity string) string {
	return AtlasAccountID + "@" + identity + ".iam.gserviceaccount.com"
}

// AtlasRoleID is the custom role the atlas's account holds on each project:
// cloudasset.assets.searchAllResources and nothing else.
const AtlasRoleID = "crewletAtlasReader"

// AtlasPermission is the one permission the atlas's role holds.
const AtlasPermission = "cloudasset.assets.searchAllResources"

// The asset types a search asks for, each read as one kind of entry.
const (
	AssetInstance = "compute.googleapis.com/Instance"
	assetSQL      = "sqladmin.googleapis.com/Instance"
	assetRedis    = "redis.googleapis.com/Instance"
	assetAlloyDB  = "alloydb.googleapis.com/Cluster"
	assetSpanner  = "spanner.googleapis.com/Instance"
	assetRun      = "run.googleapis.com/Service"
	assetFunction = "cloudfunctions.googleapis.com/Function"
	assetBucket   = "storage.googleapis.com/Bucket"
	assetTopic    = "pubsub.googleapis.com/Topic"
)

// The parts of a search a read can be refused: one project's resources, or
// the atlas's account itself.
const (
	partAssets    = "assets"
	partAtlasRole = "atlas_reader"
)

// bucketKeyStart starts a Cloud Storage bucket's key, whose name is global as
// an S3 bucket's is (s3/<name>).
const bucketKeyStart = "gcs/"

// assetShape is what one asset type is read as: its kind and, for a store,
// the atlas's word for its engine ("" where the type does not say one).
type assetShape struct {
	kind   atlas.Kind
	engine string
}

// shapeOf is the shape of an asset type, and false for a type outside the
// closed list. A switch, so the table costs nothing at init.
func shapeOf(assetType string) (assetShape, bool) {
	switch assetType {
	case AssetInstance:
		return assetShape{kind: atlas.KindHost}, true
	case assetSQL:
		// CLOUD SQL'S ENGINE IS IN ITS CONFIGURATION, which is not read.
		return assetShape{kind: atlas.KindDatastore}, true
	case assetRedis:
		return assetShape{kind: atlas.KindDatastore, engine: "redis"}, true
	case assetAlloyDB:
		return assetShape{kind: atlas.KindDatastore, engine: "postgres"}, true
	case assetSpanner:
		return assetShape{kind: atlas.KindDatastore, engine: "other"}, true
	case assetRun, assetFunction:
		return assetShape{kind: atlas.KindWorkload}, true
	case assetBucket:
		return assetShape{kind: atlas.KindBucket}, true
	case assetTopic:
		return assetShape{kind: atlas.KindQueue}, true
	}
	return assetShape{}, false
}

// assetTypes is every type a search asks for, in a fixed order.
func assetTypes() []string {
	return []string{AssetInstance, assetSQL, assetRedis, assetAlloyDB, assetSpanner, assetRun, assetFunction,
		assetBucket, assetTopic}
}

// readAssets searches each active project for what it runs.
func (r *atlasRead) readAssets(ctx context.Context, projects []Project) error {
	for _, p := range projects {
		found, err := r.client.SearchAssets(ctx, p.ProjectID, assetTypes())
		switch {
		case err == nil:
			r.projectAssets(p.ProjectID, found)
		case atlasRefused(err) || serviceDisabled(err):
			// THE ATLAS'S ACCOUNT CANNOT BE REACHED AT ALL: not made yet, or
			// the federation may not act as it. One step, said once, and
			// every asset an earlier read recorded stands.
			r.b.Unread(collect.Unread{Part: partAtlasRole, Holds: isAsset,
				Detail: "Atlas's service account " + r.client.AtlasAccount() + " cannot be reached yet, or the " +
					"Cloud Asset API is off in the identity project. Run the Cloud Shell block on the Google " +
					"Cloud connection again."})
			return nil
		case refused(err):
			r.assetsUnread[p.ProjectID] = true
		default:
			return fmt.Errorf("googlecloud: search the resources of a project: %w", err)
		}
	}
	if n := len(r.assetsUnread); n > 0 {
		unread := r.assetsUnread
		r.b.Unread(collect.Unread{Part: partAssets,
			Detail: "The resources of " + strconv.Itoa(n) + " projects were not read: Google refused Atlas's " +
				"service account. Run the Prepare a project block on each, which grants it " + AtlasRoleID + " there.",
			Holds: func(c atlas.Claim) bool { return heldByProjects(c, unread) }})
	}
	return nil
}

// atlasRefused reports Google refusing to mint the atlas's token: the account
// does not exist, or the federated principal may not act as it.
func atlasRefused(err error) bool {
	st, ok := errors.AsType[*stageError](err)
	if !ok || st.stage != stageImpersonation {
		return false
	}
	status := Status(st.err)
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// serviceDisabled reports Google refusing a search because the Cloud Asset API
// is off in the project the call is billed to, the identity project: every
// project's search is refused alike, and the fix is the identity block.
func serviceDisabled(err error) bool {
	api, ok := errors.AsType[*APIError](err)
	return ok && slices.Contains(api.Reasons, "SERVICE_DISABLED")
}

// isAsset reports a claim about an entry a search reads.
func isAsset(c atlas.Claim) bool {
	switch c.Subject.Kind {
	case atlas.KindHost, atlas.KindDatastore, atlas.KindWorkload, atlas.KindQueue:
		return strings.HasPrefix(c.Subject.Key, accountKeyPrefix)
	case atlas.KindBucket:
		return strings.HasPrefix(c.Subject.Key, bucketKeyStart)
	case atlas.KindAccount:
		return isFactOf(c, atlas.FactCapability, atlas.CapabilityObjectStorage)
	}
	return false
}

// heldByProjects reports a claim one of the refused projects' searches made.
// A BUCKET IS KEYED BY ITS NAME ALONE, which says no project, so every
// bucket's claims are carried while any project is refused: a bucket gone
// from a project that was read is retired once the refusal clears.
func heldByProjects(c atlas.Claim, refusedProjects map[string]bool) bool {
	switch c.Subject.Kind {
	case atlas.KindBucket:
		return isAsset(c)
	case atlas.KindAccount:
		project := strings.TrimPrefix(c.Subject.Key, accountKeyPrefix)
		return refusedProjects[project] && isAsset(c)
	}
	if !isAsset(c) {
		return false
	}
	project, _, _ := strings.Cut(strings.TrimPrefix(c.Subject.Key, accountKeyPrefix), "/")
	return refusedProjects[project]
}

// isFactOf reports a claim that fact f holds the capability v.
func isFactOf(c atlas.Claim, f atlas.Fact, v atlas.Capability) bool {
	return c.Facet.Kind == atlas.FacetFact && c.Facet.Fact == f && c.Value.Text == string(v)
}

// projectAssets claims what one project's search found.
func (r *atlasRead) projectAssets(project string, found []Asset) {
	b := r.b
	account, ok := b.Names("projects["+project+"]", atlas.KindAccount, accountKeyPrefix+project)
	if !ok {
		return
	}
	slices.SortFunc(found, func(a, z Asset) int { return strings.Compare(a.Name, z.Name) })
	for _, a := range found {
		shape, known := shapeOf(a.Type)
		// A RESOURCE ON ITS WAY OUT IS NOT LISTED, so it leaves as any that is gone.
		if !known || strings.EqualFold(a.State, "DELETING") {
			continue
		}
		where := "projects[" + project + "].assets[" + a.Type + "]"
		// THE WHOLE NAME FIRST: its last segment alone could be a piece of a
		// credential no rule knows any more.
		if _, fine := b.Segment(where+".name", shape.kind, a.Name); !fine {
			continue
		}
		id := a.Name[strings.LastIndexByte(a.Name, '/')+1:]
		var entry atlas.Ref
		if shape.kind == atlas.KindBucket {
			// A BUCKET'S NAME IS GLOBAL, so it keys the bucket as S3's does.
			segment, fine := b.Segment(where+".name", atlas.KindBucket, id)
			if !fine {
				continue
			}
			if entry, ok = b.Ref(where+".name", atlas.KindBucket, bucketKeyStart+segment); !ok {
				continue
			}
			b.Says(where, account, atlas.FactCapability, string(atlas.CapabilityObjectStorage))
		} else {
			location, fine := b.Segment(where+".location", shape.kind, strings.ToLower(a.Location))
			if !fine {
				continue
			}
			segment, fine := b.Segment(where+".name", shape.kind, id)
			if !fine {
				continue
			}
			if entry, ok = b.Ref(where+".name", shape.kind, accountKeyPrefix+project+"/"+location+"/"+segment); !ok {
				continue
			}
		}
		name := collect.PlainLabel(a.DisplayName)
		if strings.TrimSpace(name) == "" {
			name = id
		}
		b.Says(where+".displayName", entry, atlas.FactName, name)
		if region := regionOf(a.Location); region != "" && atlas.FactRegion.AppliesTo(shape.kind) {
			b.Says(where+".location", entry, atlas.FactRegion, region)
		}
		if atlas.RelationBelongsTo.Joins(shape.kind, atlas.KindAccount) {
			b.Relates(where, entry, atlas.RelationBelongsTo, account)
		}
		if atlas.RelationRunsOn.Joins(shape.kind, atlas.KindAccount) {
			b.Relates(where, entry, atlas.RelationRunsOn, account)
		}
		if shape.engine != "" {
			b.Says(where, entry, atlas.FactEngine, shape.engine)
		}
		for i, address := range a.Addresses {
			b.Says(where+".additionalAttributes.ips["+strconv.Itoa(i)+"]", entry, atlas.FactAddress, address)
		}
	}
}

// regionOf is the Region a location is in: a zone's Region (europe-west1-b is
// in europe-west1), a Region or a multi-region as it is, and "" for global.
func regionOf(location string) string {
	location = strings.ToLower(strings.TrimSpace(location))
	if location == "" || location == "global" {
		return ""
	}
	parts := strings.Split(location, "-")
	if len(parts) == 3 && len(parts[2]) == 1 {
		return parts[0] + "-" + parts[1]
	}
	return location
}
