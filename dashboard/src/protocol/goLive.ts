/**
 * `GET /advisor/review`: the `go_live_review` question, Settings › Advisor.
 * Each agent's access is the engine's `capability.Snapshot`, as its next turn
 * is told it; the fields the review draws are declared here, and lists are
 * always lists.
 */

/** One vendor lane an agent is connected to, at its level. */
export interface ToldIntegration {
  kind: string;
  lane: string;
  on: boolean;
  level?: string;
  level_name?: string;
  /** Each permission the agent's app holds, by its name, and the vendor's words for it. */
  permissions?: Record<string, string>;
  permission_names?: Record<string, string>;
  /** Where it acts (repositories, projects, hosts), each by its label where it has one,
   *  and how many past the shown ones. */
  scope?: string[];
  scope_labels?: Record<string, string>;
  scope_more?: number;
  /** For SSH, the read commands each host admits. */
  commands?: { host: string; run: string[]; more?: number }[];
}

/** One MCP server the agent may call, with its tools at each level. */
export interface ToldToolServer {
  server: string;
  allow: string[];
  approve: string[];
  blocked: string[];
  /** The level of every tool the lists do not name; absent where each runs at its server's hints. */
  default?: "allow" | "approve" | "blocked";
  /** How many named tools past the shown ones the lists hold. */
  more?: number;
}

/** One box the agent's runs use. */
export interface ToldBox {
  /** `direct`, `container` or `e2b`. */
  cell: string;
  memory_bytes?: number;
  /** Whether a run has read the box since it was last made. */
  observed?: boolean;
  browser?: string;
  container_runtime?: string;
}

export interface ToldAccess {
  integrations: ToldIntegration[];
  mcp: ToldToolServer[];
  boxes: ToldBox[];
}

export interface GoLiveReviewAnswer {
  agents: { seat: string; name: string; access: ToldAccess }[];
}
