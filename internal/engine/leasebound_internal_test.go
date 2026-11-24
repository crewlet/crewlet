package engine

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/schedule"
)

// EVERY LEASE THIS BUILD TAKES NAMES ITSELF WITHIN WHAT A LEASE'S RECORD
// HOLDS, at its widest: each writer's resource (a seat, a node's presence, a
// taker, a browser control, a capacity window, each duty this engine claims,
// a surface's provisioning, an OAuth renewal, a growth act, the object store's
// raise), owner (an incarnation, a node's hold, a person's control) and
// preferred node, built from the bounds of what names it, each within
// coord.MaxLeaseNameBytes. The widest is an OAuth renewal's resource, 90
// bytes, and a browser control's owner, 118.
//
// Mutation: name the growth act's lease by its account, or lower
// coord.MaxLeaseNameBytes to 100, and this is red.
func TestEveryLeaseThisBuildTakesNamesItselfWithinItsRecord(t *testing.T) {
	t.Parallel()
	node := strings.Repeat("n", coord.MaxNodeIDBytes)
	widest := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	incarnation := config.NewIncarnation(node)
	names := map[string]string{
		"a seat":                 coord.SeatResource(widest),
		"a node's presence":      coord.NodeResource(node),
		"a taker":                coord.TakerResource(incarnation),
		"a browser control":      coord.BrowserControlResource(widest),
		"a capacity window":      coord.MaintenanceResource(strings.Repeat("S", coord.MaxStreamNameBytes)),
		"a growth act":           growthActResource("write/search_console/" + strings.Repeat("p", 10<<10)),
		"the object store raise": objectsRaiseClass.Resource(natsobj.Stream),
		"an incarnation":         incarnation,
		"a node's hold":          node + "/" + widest.String(),
		"a person's control":     coord.BrowserControlOwner{Person: widest, Credential: iam.SessionPrefix + widest.String(), Take: widest}.Key(),
		"a preferred node":       node,
	}
	for _, duty := range []string{advisorDutyName, atlasCollectDutyName, atlasWakeDutyName, chatPruneDutyName,
		codeRetentionDutyName, embedDutyName, flowDutyName, identitySweepDuty, integrationDutyName,
		learningDutyName, maintenanceDutyName, objectCollectorDuty, retentionDutyName, waiterDutyName,
		schedule.DutyName} {
		names["the duty "+duty] = coord.WorkerResource(duty)
	}
	for _, kind := range integration.Kinds {
		names["provisioning "+string(kind)] = coord.WorkerResource(setupDutyName(kind))
	}
	for _, family := range coord.GrantFamilies {
		names["a "+family.Owner+" renewal"] = family.Renewal.Resource(strings.Repeat("g", coord.MaxGrantNameBytes))
	}
	longest := ""
	for what, name := range names {
		if err := coord.HoldLeaseName(what, name); err != nil {
			t.Errorf("%s: %v", what, err)
		}
		if len(name) > len(names[longest]) {
			longest = what
		}
	}
	t.Logf("the widest name is %s's, %d bytes", longest, len(names[longest]))
}
