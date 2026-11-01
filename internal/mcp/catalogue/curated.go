package catalogue

import "sync"

// Curated is every server the catalogue offers, in the order the add flow
// lists them: the vendors' own remote servers first, then the local servers
// the engine binds an agent's own credential to, then the rest. A copy: the
// table itself is read-only.
func Curated() []Entry {
	table := curated()
	out := make([]Entry, len(table))
	for i := range table {
		out[i] = table[i].clone()
	}
	return out
}

// Each entry's facts are what its vendor's own page said on ReadAt, and a
// local server's what its Release did. The keys a recognised server reads are
// the vendors' credential keys, held to each vendor package's CredentialKeys
// by internal/seataccess's suite.
var (
	githubKeys = []SeatKey{{Field: "APP_TOKEN", Env: "GITHUB_PERSONAL_ACCESS_TOKEN",
		Header: "Authorization", Prefix: "Bearer "}}
	datadogKeys = []SeatKey{{Field: "APP_KEY", Env: "DD_APPLICATION_KEY", Header: "DD_APPLICATION_KEY"}}
	// glab reads GITLAB_TOKEN, the community GitLab servers
	// GITLAB_PERSONAL_ACCESS_TOKEN: one integration, two spellings.
	glabKeys          = []SeatKey{{Field: "TOKEN", Env: "GITLAB_TOKEN", Header: "Private-Token"}}
	gitlabServerKeys  = []SeatKey{{Field: "TOKEN", Env: "GITLAB_PERSONAL_ACCESS_TOKEN", Header: "Private-Token"}}
	slackKeys         = []SeatKey{{Field: "BOT_TOKEN", Env: "SLACK_MCP_XOXB_TOKEN", Header: "Authorization", Prefix: "Bearer "}}
	mattermostKeys    = []SeatKey{{Field: "BOT_TOKEN", Env: "MATTERMOST_TOKEN"}}
	mcpAtlassianKeys  = []SeatKey{{Field: "TOKEN", Env: "JIRA_API_TOKEN"}, {Field: "EMAIL", Env: "JIRA_USERNAME"}, {Field: "TOKEN", Env: "CONFLUENCE_API_TOKEN"}, {Field: "EMAIL", Env: "CONFLUENCE_USERNAME"}}
	tavilyReads       = []string{"tavily_search", "tavily_extract", "tavily_crawl", "tavily_map", "tavily_research"}
	readsTheOpenWorld = Hints{ReadOnly: yes(), OpenWorld: yes()}
)

func yes() *bool { v := true; return &v }

// curated is the table, built once.
var curated = sync.OnceValue(func() []Entry {
	tavily := map[string]Hints{}
	for _, tool := range tavilyReads {
		tavily[tool] = readsTheOpenWorld
	}
	return []Entry{
		{
			ID: "github", Name: "github", Title: "GitHub", Vendor: "github",
			Description: "Repositories, issues, pull requests and Actions, through GitHub's own remote server.",
			Transport:   TransportHTTP, URL: "https://api.githubcopilot.com/mcp/",
			Headers: []Input{{Name: "Authorization", Required: true, Secret: true, FromIntegration: true,
				Format:      "Bearer <token>",
				Description: "A personal access token, or each agent's own app token where the GitHub integration is on."}},
			Auth:        []AuthMethod{AuthIntegration, AuthHeaders},
			Integration: "github",
			Docs:        "https://github.com/github/github-mcp-server",
			ReadAt:      "2026-11-01",
			// One recognition for both of GitHub's servers, so the
			// integration has one binding whichever one a company runs.
			recognise: []recognition{
				{match: either(hostIs("api.githubcopilot.com"), launches("github-mcp-server")), keys: githubKeys},
			},
		},
		{
			ID: "datadog", Name: "datadog", Title: "Datadog", Vendor: "datadog",
			Description: "Logs, metrics, monitors, incidents and traces.",
			Transport:   TransportHTTP, URL: "https://mcp.datadoghq.com/v1/mcp",
			Headers: []Input{
				{Name: "DD_API_KEY", Required: true, Secret: true, Format: "<API key>",
					Description: "The organization's API key, the same for every agent."},
				{Name: "DD_APPLICATION_KEY", Required: true, Secret: true, FromIntegration: true,
					Format:      "<application key>",
					Description: "An application key: each agent's own where the Datadog integration is on."},
			},
			Auth:        []AuthMethod{AuthIntegration, AuthOAuth, AuthHeaders},
			OAuthClient: ClientDCR,
			Integration: "datadog",
			Prerequisite: "The address is your Datadog site's: mcp. before the site and /v1/mcp after it " +
				"(https://mcp.us5.datadoghq.com/v1/mcp on US5). Datadog's government sites do not serve it.",
			Docs:   "https://docs.datadoghq.com/mcp_server/setup/",
			ReadAt: "2026-11-01",
			// datadoghq.com covers every commercial site but EU1's; the
			// suite holds every site datadog.Sites names to this.
			recognise: []recognition{
				{match: hostUnder("datadoghq.com", "datadoghq.eu", "ddog-gov.com"), keys: datadogKeys},
			},
		},
		{
			ID: "rovo", Name: "rovo", Title: "Atlassian Rovo", Vendor: "atlassian",
			Description: "Jira and Confluence through Atlassian's own remote server.",
			Transport:   TransportHTTP, URL: "https://mcp.atlassian.com/v1/mcp",
			Headers: []Input{{Name: "Authorization", Required: true, Secret: true,
				Format: "Basic <base64 of email:API token>",
				Description: "An Atlassian API token with its account's email. Atlassian accepts it only " +
					"after an organization admin turns on API token sign-in for the Rovo MCP server."}},
			Auth:        []AuthMethod{AuthOAuth, AuthHeaders},
			OAuthClient: ClientDCR,
			Prerequisite: "An organization admin allows the engine's domain under Atlassian Administration, " +
				"Rovo, Rovo MCP server before anyone signs in, and every request has to pass the " +
				"organization's IP allowlist.",
			Docs:      "https://support.atlassian.com/atlassian-rovo-mcp-server/docs/getting-started-with-the-atlassian-remote-mcp-server/",
			ReadAt:    "2026-11-01",
			recognise: []recognition{{match: hostIs("mcp.atlassian.com")}},
		},
		{
			ID: "linear", Name: "linear", Title: "Linear", Vendor: "linear",
			Description: "Issues, projects, cycles and documents.",
			Transport:   TransportHTTP, URL: "https://mcp.linear.app/mcp",
			Headers: []Input{{Name: "Authorization", Required: true, Secret: true,
				Format: "Bearer <API key>", Description: "A Linear API key."}},
			Auth:        []AuthMethod{AuthOAuth, AuthHeaders},
			OAuthClient: ClientCIMD,
			Docs:        "https://linear.app/docs/mcp",
			ReadAt:      "2026-11-01",
			recognise:   []recognition{{match: hostIs("mcp.linear.app")}},
		},
		{
			ID: "notion", Name: "notion", Title: "Notion", Vendor: "notion",
			Description: "Pages, databases and comments.",
			Transport:   TransportHTTP, URL: "https://mcp.notion.com/mcp",
			Headers:     []Input{},
			Auth:        []AuthMethod{AuthOAuth},
			OAuthClient: ClientCIMD,
			Docs:        "https://developers.notion.com/docs/mcp",
			ReadAt:      "2026-11-01",
			recognise:   []recognition{{match: hostIs("mcp.notion.com")}},
		},
		{
			ID: "sentry", Name: "sentry", Title: "Sentry", Vendor: "sentry",
			Description: "Issues, events, traces and releases.",
			Transport:   TransportHTTP, URL: "https://mcp.sentry.dev/mcp",
			Headers:     []Input{},
			Auth:        []AuthMethod{AuthOAuth},
			OAuthClient: ClientCIMD,
			Docs:        "https://mcp.sentry.dev/",
			ReadAt:      "2026-11-01",
			recognise:   []recognition{{match: hostIs("mcp.sentry.dev")}},
		},
		{
			ID: "stripe", Name: "stripe", Title: "Stripe", Vendor: "stripe",
			Description: "Customers, payments, subscriptions and the Stripe documentation.",
			Transport:   TransportHTTP, URL: "https://mcp.stripe.com",
			Headers: []Input{{Name: "Authorization", Required: true, Secret: true,
				Format: "Bearer <agent API key>",
				Description: "An agent API key, the kind the Stripe Dashboard badges Agent. From December " +
					"2026 Stripe refuses any other secret or restricted key here."}},
			Auth:        []AuthMethod{AuthOAuth, AuthHeaders},
			OAuthClient: ClientDCR,
			Docs:        "https://docs.stripe.com/mcp",
			ReadAt:      "2026-11-01",
			recognise:   []recognition{{match: hostIs("mcp.stripe.com")}},
		},
		{
			ID: "cloudflare", Name: "cloudflare", Title: "Cloudflare", Vendor: "cloudflare",
			Description: "The Cloudflare API across the company's accounts and zones, through Cloudflare's own server.",
			Transport:   TransportHTTP, URL: "https://mcp.cloudflare.com/mcp",
			Headers: []Input{{Name: "Authorization", Required: true, Secret: true,
				Format:      "Bearer <API token>",
				Description: "A user or account API token holding the permissions the agents need."}},
			Auth:        []AuthMethod{AuthOAuth, AuthHeaders},
			OAuthClient: ClientCIMD,
			Docs:        "https://developers.cloudflare.com/agents/model-context-protocol/mcp-servers-for-cloudflare/",
			ReadAt:      "2026-11-01",
			recognise:   []recognition{{match: hostIs("mcp.cloudflare.com")}},
		},
		{
			ID: "gitlab", Name: "gitlab", Title: "GitLab", Vendor: "gitlab",
			Description: "Merge requests, issues, pipelines and the GitLab API, through the glab CLI's own " +
				"server, one per agent.",
			Transport: TransportStdio, Command: "glab", Args: []string{"mcp", "serve"},
			Env: []Input{
				{Name: "GITLAB_TOKEN", Required: true, Secret: true, FromIntegration: true,
					Description: "Each agent's own token, which the GitLab integration hands it."},
				{Name: "GITLAB_HOST", Format: "gitlab.example.com",
					Description: "The instance's host, for GitLab Self-Managed; leave it out for GitLab.com."},
				{Name: "GLAB_CHECK_UPDATE", Value: "false",
					Description: "Keeps glab's update notice out of the agents' tool results."},
				{Name: "GLAB_SHOW_WHATS_NEW", Value: "false",
					Description: "Keeps glab's banner after an upgrade out of the agents' tool results."},
				{Name: "GLAB_SEND_TELEMETRY", Value: "false",
					Description: "Stops glab sending the instance a usage event on every call."},
			},
			Auth:        []AuthMethod{AuthIntegration},
			Integration: "gitlab",
			Prerequisite: "glab is installed on every engine host that runs agent seats. GitLab marks " +
				"glab mcp serve as an experiment.",
			Docs:   "https://gitlab.com/gitlab-org/cli",
			ReadAt: "2026-10-19",
			recognise: []recognition{
				{match: glabServe, keys: glabKeys},
				{match: launches("server-gitlab", "gitlab-mcp"), keys: gitlabServerKeys},
			},
		},
		{
			ID: "slack", Name: "slack", Title: "Slack", Vendor: "slack",
			Description: "Channels, threads, messages and reactions, as each agent's own Slack bot.",
			Transport:   TransportStdio, Command: "npm",
			Args: []string{"exec", "--yes", "--", "slack-mcp-server@1.3.0", "--transport", "stdio"},
			Env: []Input{
				{Name: "SLACK_MCP_XOXB_TOKEN", Required: true, Secret: true, FromIntegration: true,
					Description: "Each agent's own bot token, which the Slack integration hands it."},
				{Name: "SLACK_MCP_ADD_MESSAGE_TOOL", Value: "true",
					Description: "Registers conversations_add_message: without it an agent reads every " +
						"message and can answer none."},
				{Name: "SLACK_MCP_REACTION_TOOL", Value: "true",
					Description: "Registers reactions_add and reactions_remove."},
			},
			ToolPrefix:  "slack_",
			Auth:        []AuthMethod{AuthIntegration},
			Integration: "slack",
			Docs:        "https://github.com/korotovsky/slack-mcp-server",
			ReadAt:      "2026-10-13",
			// Read at this release: the switches, the channel cache and the
			// rules internal/config's slackRefusal restates.
			Release:   "1.3.0",
			Package:   "slack-mcp-server",
			recognise: []recognition{{match: launches("slack-mcp-server"), keys: slackKeys}},
		},
		{
			ID: "mattermost", Name: "mattermost", Title: "Mattermost", Vendor: "mattermost",
			Description: "Channels, posts and threads, as each agent's own Mattermost bot.",
			Transport:   TransportStdio, Command: "uvx", Args: []string{"mcp-server-mattermost==0.6.1"},
			Env: []Input{
				{Name: "MATTERMOST_URL", Required: true, Format: "https://chat.example.com",
					Description: "The Mattermost server's address."},
				{Name: "MATTERMOST_TOKEN", Required: true, Secret: true, FromIntegration: true,
					Description: "Each agent's own bot token, which the Mattermost integration hands it."},
			},
			ToolPrefix:  "mattermost_",
			Auth:        []AuthMethod{AuthIntegration},
			Integration: "mattermost",
			Docs:        "https://pypi.org/project/mcp-server-mattermost/",
			ReadAt:      "2026-10-13",
			// Read at this release: the tool names the bundled skills trigger
			// on, the variables its page names, and the two tools that reach
			// the engine host's disk.
			Release:   "0.6.1",
			Package:   "mcp-server-mattermost",
			recognise: []recognition{{match: launches("mcp-server-mattermost"), keys: mattermostKeys}},
		},
		{
			ID: "atlassian", Name: "atlassian", Title: "Jira and Confluence", Vendor: "atlassian",
			Description: "Jira issues and Confluence pages as each agent's own Atlassian account, through " +
				"the open-source mcp-atlassian server.",
			Transport: TransportStdio, Command: "uvx", Args: []string{"mcp-atlassian==0.23.1"},
			Env: []Input{
				{Name: "JIRA_URL", Format: "https://example.atlassian.net",
					Description: "The Jira site's address; leave it out for a company on Confluence alone."},
				{Name: "CONFLUENCE_URL", Format: "https://example.atlassian.net/wiki",
					Description: "The Confluence site's address; leave it out for a company on Jira alone."},
				{Name: "JIRA_USERNAME", FromIntegration: true,
					Description: "Each agent's account email, which the Atlassian integration hands it."},
				{Name: "JIRA_API_TOKEN", Secret: true, FromIntegration: true,
					Description: "Each agent's own API token, which the Atlassian integration hands it."},
				{Name: "CONFLUENCE_USERNAME", FromIntegration: true,
					Description: "Each agent's account email, which the Atlassian integration hands it."},
				{Name: "CONFLUENCE_API_TOKEN", Secret: true, FromIntegration: true,
					Description: "Each agent's own API token, which the Atlassian integration hands it."},
			},
			Auth:        []AuthMethod{AuthIntegration},
			Integration: "atlassian",
			Docs:        "https://github.com/sooperset/mcp-atlassian",
			ReadAt:      "2026-10-13",
			// Read at this release: the tools the Jira, Confluence and turn
			// engine pages name, the variables a seat sets, the five tools that
			// read a file on the engine host, what an unset TOOLSETS serves,
			// and the browse_url on an issue it returns.
			Release:   "0.23.1",
			Package:   "mcp-atlassian",
			recognise: []recognition{{match: launches("mcp-atlassian"), keys: mcpAtlassianKeys}},
		},
		{
			ID: "tavily", Name: "tavily", Title: "Tavily",
			Description: "Web search, page extraction, crawling and research.",
			Transport:   TransportStdio, Command: "npm", Args: []string{"exec", "--yes", "--", "tavily-mcp@0.2.22"},
			Env: []Input{{Name: "TAVILY_API_KEY", Required: true, Secret: true,
				Description: "The company's Tavily API key."}},
			// It declares no hint on any of its five tools, each of which
			// only reads the web.
			ToolAnnotations: tavily,
			Auth:            []AuthMethod{AuthNone},
			Docs:            "https://github.com/tavily-ai/tavily-mcp",
			ReadAt:          "2026-10-13",
			// Read at this release: its bin's name and TAVILY_API_KEY, the one
			// variable it is passed, and its five tools, which the annotations
			// above name: that they are the five it lists and that none of
			// them advertises an annotation.
			Release:   "0.2.22",
			Package:   "tavily-mcp",
			recognise: []recognition{{match: launches("tavily-mcp")}},
		},
		{
			ID: "postgres", Name: "postgres", Title: "PostgreSQL", Vendor: "postgresql",
			Description: "Schemas, read-only queries, query plans and index advice for one database, " +
				"through Postgres MCP Pro.",
			Transport: TransportStdio, Command: "uvx",
			Args: []string{"postgres-mcp==0.3.0", "--access-mode=restricted"},
			Env: []Input{{Name: "DATABASE_URI", Required: true, Secret: true,
				Format:      "postgresql://user:password@host:5432/database",
				Description: "The database's connection string, for an account that may only read."}},
			Auth:   []AuthMethod{AuthNone},
			Docs:   "https://github.com/crystaldba/postgres-mcp",
			ReadAt: "2026-11-01",
			// Read at this release, from its package description: its bin's
			// name, DATABASE_URI and the restricted access mode.
			Release:   "0.3.0",
			Package:   "postgres-mcp",
			recognise: []recognition{{match: launches("postgres-mcp")}},
		},
	}
})
