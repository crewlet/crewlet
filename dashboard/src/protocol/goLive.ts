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
}

/** One MCP server the agent may call, with its tools at each level. */
export interface ToldToolServer {
  server: string;
  allow: string[];
  approve: string[];
  blocked: string[];
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
