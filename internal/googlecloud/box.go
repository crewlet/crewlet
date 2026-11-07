package googlecloud

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The Google Cloud CLI in an agent's box, which reads the agent's own token
// from a file rather than signing in.
//
// THE FILE, NEVER THE ENVIRONMENT: the token is issued at use for an hour, and
// a run can outlast it, so it is a file the completion poll rewrites as the
// token renews, which gcloud reads afresh at each call. A variable would be
// fixed when the box starts. The launch points gcloud at the file under the
// box's own home ([seataccess.BoxFile.PathEnv]), and the agent's first project
// is a plain setting beside it.
const (
	// TokenFile is the token file's path, relative to the box's home.
	TokenFile = ".config/crewlet/gcloud-token"
	// TokenFileEnv is the setting gcloud reads the file's path from.
	TokenFileEnv = "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"
	// ProjectEnv is the setting gcloud reads its default project from.
	ProjectEnv = "CLOUDSDK_CORE_PROJECT"
)

// BoxDefaults is what an agent on at Google Cloud finds in its box: its token
// file, which the poll keeps current, the setting that points gcloud at it,
// and its first project as gcloud's default. Whether gcloud is installed is
// the box image's, which the engine cannot see.
func (accessVendor) BoxDefaults(_ *config.Company, role *config.Role) seataccess.BoxSet {
	handle := strings.TrimSpace(role.IdentityKey())
	if handle == "" {
		return seataccess.BoxSet{}
	}
	out := seataccess.BoxSet{Files: []seataccess.BoxFile{{
		Path: TokenFile, Ref: "${" + seataccess.SinkName(integration.KindGoogleCloud, AccessTokenField, handle) + "}",
		PathEnv: TokenFileEnv, Rewritten: true,
	}}}
	if seat := role.Integrations.GoogleCloud; seat != nil && len(seat.Projects) > 0 {
		out.Env = map[string]string{ProjectEnv: strings.TrimSpace(seat.Projects[0])}
	}
	return out
}
