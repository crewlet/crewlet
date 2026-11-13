package cloudflare

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// unresolvedLevels is, by level, the permission group names the catalogue
// does not offer; nil where every level resolves.
func (c catalogue) unresolvedLevels() map[string][]string {
	var out map[string][]string
	for _, level := range Levels() {
		names, _ := Groups(level)
		if missing := c.missingGroups(names); len(missing) > 0 {
			if out == nil {
				out = map[string][]string{}
			}
			out[level] = missing
		}
	}
	return out
}

// them is how a sentence refers back to n permission groups.
func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// ownUnheld is the groups the provisioner needs for itself among unheld.
func ownUnheld(unheld []string) []string {
	own := names(provisionerOwnPermissions)
	return slices.DeleteFunc(slices.Clone(unheld), func(name string) bool { return !slices.Contains(own, name) })
}

// levelGaps is each level the account's catalogue or the provisioner's own
// policy cannot grant yet, least access first. A group the provisioner needs
// for itself is not a level's gap: it stops every level, and is reported so.
func levelGaps(unresolved map[string][]string, unheld []string, tokensURL string) []seataccess.LevelGap {
	own := names(provisionerOwnPermissions)
	var out []seataccess.LevelGap
	for _, l := range levels {
		subject := "cloudflare level " + l.id
		if missing := unresolved[l.id]; len(missing) > 0 {
			out = append(out, seataccess.LevelGap{
				Level: l.id, Name: l.name, Missing: missing,
				Reason: fmt.Sprintf("The %s level is unavailable until Cloudflare offers this account "+
					"a permission group named %s", l.name, words.And(missing)),
				Remedy: "Check the account can use the product the group belongs to",
				Blocking: integration.Finding{
					Kind: integration.FindingUnknownTier, Subject: subject, Title: "Permission groups not found",
					Detail: fmt.Sprintf("Cloudflare lists no permission group a token can be granted named %s, "+
						"which the %s level grants, so no agent can be given that level", words.And(missing), l.name),
					Remedy:   "Move the agents at this level to another one on the Agents tab until Cloudflare lists the group again",
					Subjects: missing,
				},
			})
			continue
		}
		granted, _ := Groups(l.id)
		var lacking []string
		for _, name := range unheld {
			if slices.Contains(granted, name) && !slices.Contains(own, name) {
				lacking = append(lacking, name)
			}
		}
		if len(lacking) == 0 {
			continue
		}
		shown := dashboardList(lacking)
		out = append(out, seataccess.LevelGap{
			Level: l.id, Name: l.name, Missing: shown, ActionURL: tokensURL,
			Reason: fmt.Sprintf("The %s level is unavailable until the provisioner token holds %s",
				l.name, words.And(dashboardNames(lacking))),
			Remedy: "Edit the provisioner token at Cloudflare to add " + them(len(lacking)) +
				" over the account and its zones",
			Blocking: integration.Finding{
				Kind: integration.FindingGrantShort, Subject: subject, Title: "Provisioner token is missing permissions",
				Detail: fmt.Sprintf("The provisioner token does not hold %s, which the %s level grants, and "+
					"Crewlet grants an agent nothing the token does not hold itself",
					words.And(dashboardNames(lacking)), l.name),
				Remedy: "Edit the provisioner token at Cloudflare to add " + them(len(lacking)) +
					" over the account and its zones, or move the agents at this level to one it holds",
				ActionURL: tokensURL, Subjects: shown,
			},
		})
	}
	return out
}

// seeMoreLabel is what the suggestion of the atlas's optional permissions is
// filed under, in the Test button's verdict and on the card alike.
const seeMoreLabel = "See more in Atlas"

// seeMore is the suggestion for a token that does not hold some of the
// atlas's optional permission groups: what to add, and what it would show.
// Empty where it holds them all, or where its policy could not be read.
func seeMore(unheld []string) string {
	if len(unheld) == 0 {
		return ""
	}
	var shows []words.Said
	for _, p := range atlasPermissions {
		if slices.Contains(unheld, p.name) {
			shows = append(shows, words.Said(p.shows))
		}
	}
	return "Add " + words.And(dashboardNames(unheld)) + " to the token to show " + words.And(shows) +
		" in Atlas. Optional: everything else works without " + them(len(unheld)) + "."
}

// seeMoreValue is [seeMore] as a check's fact holds it: a value, not a
// sentence. Empty where there is nothing to suggest.
func seeMoreValue(unheld []string) string {
	if len(unheld) == 0 {
		return ""
	}
	return "optional, add " + words.And(dashboardNames(unheld))
}
