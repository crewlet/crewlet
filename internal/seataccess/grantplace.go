package seataccess

// GrantPlace is where an administrator sets a seat's grant on lane, as a
// sentence names the dashboard's path to it: the integration's own card, then
// its Agents tab. Jira and Confluence are set on the one Atlassian card.
func GrantPlace(lane Lane) string {
	return "Settings, Integrations, " + KindName(lane.Kind()) + ", Agents"
}
