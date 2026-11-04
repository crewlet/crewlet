package scaleway

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// levelGaps is each level the organization's catalogue cannot grant yet: one
// naming a permission set Scaleway does not list at the scope the level grants
// it at.
//
// NOTHING IS A GAP FOR WHAT THE PROVISIONER HOLDS ITSELF: Scaleway checks no
// grant against its granter, so IAMPolicyManager writes any listed set, and
// the provisioner's own sets are the connect check's finding instead.
func levelGaps(catalogue []PermissionSet) []seataccess.LevelGap {
	missing := unlisted(catalogue)
	var out []seataccess.LevelGap
	for _, l := range levels {
		sets := missing[l.id]
		if len(sets) == 0 {
			continue
		}
		slices.Sort(sets)
		out = append(out, seataccess.LevelGap{
			Level: l.id, Name: l.name, Missing: sets, ActionURL: PoliciesURL,
			Reason: fmt.Sprintf("The %s level is unavailable until Scaleway offers this Organization "+
				"%s at the scope the level grants %s at", l.name, words.And(sets), pronoun(len(sets))),
			Remedy: "Check the Organization can use the products these permission sets belong to",
			Blocking: integration.Finding{
				Kind: integration.FindingUnknownTier, Subject: "scaleway level " + l.id,
				Title: "Permission sets not found",
				Detail: fmt.Sprintf("Scaleway lists no permission set named %s at the scope the %s level "+
					"grants it at, so no agent can be given that level", words.And(sets), l.name),
				Remedy:    "Move the agents at this level to another one on the Agents tab until Scaleway lists the set again",
				ActionURL: PoliciesURL, Subjects: sets,
			},
		})
	}
	return out
}

// pronoun is "it" for one set and "them" for more.
func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
