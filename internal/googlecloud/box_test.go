package googlecloud_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// AN ON SEAT'S BOX HOLDS ITS GCLOUD TOKEN FILE AND THE SETTINGS BESIDE IT: the
// file holds the seat's issued variable and the poll keeps it, gcloud is
// pointed at it and at the seat's first project, and the Agents tab lists each
// as Crewlet's. An off seat's box holds none of it. The file is no tool: the
// engine cannot see whether gcloud is installed, so without a bound server the
// tool reads no_server.
func TestAnOnSeatsBoxHoldsItsGcloudTokenFile(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb, projectData)
	box := seataccess.BoxOf(w.company, integration.KindGoogleCloud, lane, "sre")
	if box == nil {
		t.Fatal("an on seat's box holds nothing of Google Cloud's")
	}
	wantEnv := []seataccess.BoxLine{
		{Name: googlecloud.TokenFileEnv, Value: "~/" + googlecloud.TokenFile, By: seataccess.ByCrewlet},
		{Name: googlecloud.ProjectEnv, Value: projectWeb, By: seataccess.ByCrewlet},
	}
	wantFiles := []seataccess.BoxFileLine{{Path: googlecloud.TokenFile, Value: "${GOOGLE_CLOUD_ACCESS_TOKEN_SRE}",
		Rewritten: true, By: seataccess.ByCrewlet}}
	if len(box.Env) != len(wantEnv) || box.Env[0] != wantEnv[0] || box.Env[1] != wantEnv[1] ||
		len(box.Files) != 1 || box.Files[0] != wantFiles[0] {
		t.Fatalf("the box is %+v, want gcloud's file %+v and settings %+v", box, wantFiles, wantEnv)
	}
	defaults := seataccess.BoxDefaults(w.company, "sre")
	var file *seataccess.BoxFile
	for _, d := range defaults {
		if d.File != nil {
			file = d.File
		}
	}
	if file == nil || file.PathEnv != googlecloud.TokenFileEnv || !file.Rewritten {
		t.Errorf("the box's defaults are %+v, want the token file named by %s and kept by the poll",
			defaults, googlecloud.TokenFileEnv)
	}

	want, _ := seataccess.DesiredOf(&w.company.Roles[0], lane)
	have := seataccess.Observed{Exists: true, Enabled: true}
	have.Credential = seataccess.IssuedCredential(integration.KindGoogleCloud, lane, "sre", want, have)
	if got := seataccess.ToolStateOf(w.company, "sre", lane, want, have); got != seataccess.ToolNoServer {
		t.Errorf("with the file and no server the tool reads %s, want no_server", got)
	}
	w.withMonitoring()
	if got := seataccess.ToolStateOf(w.company, "sre", lane, want, have); got != seataccess.ToolLive {
		t.Errorf("with Monitoring bound the tool reads %s, want live", got)
	}

	off := false
	w.seat().Enabled = &off
	if box := seataccess.BoxOf(w.company, integration.KindGoogleCloud, lane, "sre"); box != nil {
		t.Errorf("an off seat's box holds %+v", box)
	}
	if defaults := seataccess.BoxDefaults(w.company, "sre"); len(defaults) != 0 {
		t.Errorf("an off seat is handed %+v", defaults)
	}
}
