package datasetsapi

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// nopBoards admits nothing: the cases here call the surface's halves directly.
type nopBoards struct{}

func (nopBoards) AdmitContent(http.ResponseWriter, *http.Request, int) (artifactsapi.Admission, bool) {
	return artifactsapi.Admission{}, false
}

func (nopBoards) Board(http.ResponseWriter, *http.Request, artifactsapi.Admission, int) (*dashspec.Board, bool) {
	return nil, false
}

type nopData struct{}

func (nopData) Docs(context.Context, artifacts.ID, artifactdata.Reading, string, int64, string, int) (
	artifactdata.DocPage, error) {
	return artifactdata.DocPage{}, artifactdata.ErrNotFound
}

// admission is p admitted to one board.
func admission(p iam.Principal) artifactsapi.Admission {
	return artifactsapi.Admission{Principal: p,
		Head: artifactsdomain.Head{ID: artifacts.IDFor("a board"), Kind: artifacts.KindDashboard}}
}
