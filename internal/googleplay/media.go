package googleplay

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Upload limits Google sets per method (its discovery document's maxSize),
// for the ones below the artifact store's own bound.
var uploadLimits = map[string]int64{
	"androidpublisher.edits.images.upload":                                       15 << 20,
	"androidpublisher.appstoreappsreview.uploadimage":                            15 << 20,
	"androidpublisher.appstoreappsreview.uploadappstoreapppolicydeclarationfile": 10 << 20,
}

// defaultUploadLimit bounds every other upload: an artifact's file is far
// smaller, and a build larger than this is Expo's to submit.
const defaultUploadLimit = 64 << 20

// mediaOf is the file an upload step names, read through the node's
// artifacts as handle's turn may read it; nil for a step that uploads nothing.
func (p *Product) mediaOf(ctx context.Context, handle string, op growth.Operation, ref any) (*File, error) {
	name, _ := ref.(string)
	name = strings.TrimSpace(name)
	switch {
	case !uploads(op) && name == "":
		return nil, nil
	case !uploads(op):
		return nil, refusal(growth.RefusedInvalid, op.ID+" uploads nothing, so it takes no `media`. No vendor was "+
			"reached.")
	case name == "":
		return nil, refusal(growth.RefusedInvalid, op.ID+" uploads a file: name it as `media`, "+
			"artifact:<artifact id>/<path in it>. No vendor was reached.")
	case p.deps.Media == nil:
		return nil, refusal(growth.RefusedUnavailable, "this node reads no artifact for an upload. No vendor was "+
			"reached.")
	}
	limit, ok := uploadLimits[op.ID]
	if !ok {
		limit = defaultUploadLimit
	}
	f, err := p.deps.Media.Open(ctx, handle, name, limit)
	if err != nil {
		return nil, err
	}
	if f.ContentType == "" {
		f.ContentType = "application/octet-stream"
	}
	if strings.Contains(op.ID, "images.upload") || strings.HasSuffix(op.ID, "uploadimage") {
		if !strings.HasPrefix(f.ContentType, "image/") {
			return nil, refusal(growth.RefusedInvalid, name+" is "+f.ContentType+", and "+op.ID+" takes an image. "+
				"No vendor was reached.")
		}
	}
	return &f, nil
}
