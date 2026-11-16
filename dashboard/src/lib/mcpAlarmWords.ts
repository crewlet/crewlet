/**
 * The one wording of a tool server alarm's row, for the Inbox and the
 * server's own page alike: its tone, and its title naming the server.
 */

import type { McpAlarmKind } from "~/contract/mcp.ts";

/** What an alarm row is about: the server by its title, and the agent or node it names. */
export interface AlarmSubject {
  server: string;
  /** The agent whose instance it names, by name; "" for none. */
  who: string;
  /** The node a shared server's instance is down on; "" where none is named. */
  node: string;
  /** How many tools, for a refused name. */
  count: number;
}

/**
 * Each MCP alarm on this page: its tone and what it says about one server in
 * a line. NOT RUNNING IS A PROBLEM for whoever it stops, however many others
 * it serves, so a partial outage is drawn as a failure for that agent or node.
 */
export const ALARM: Record<
  McpAlarmKind,
  { tone: "danger" | "warning" | "info"; title: (about: AlarmSubject) => string }
> = {
  mcp_server_failing: {
    tone: "danger",
    title: ({ server }) => `${server} is not running`,
  },
  mcp_server_partial: {
    tone: "danger",
    title: ({ server, who, node }) =>
      who
        ? `${server} is not running for ${who}`
        : node
          ? `${server} is not running on ${node}`
          : `${server} is not running everywhere`,
  },
  mcp_server_waiting_long: {
    tone: "warning",
    title: ({ server, who }) =>
      who
        ? `${server} is still waiting for ${who}'s credential`
        : `${server} is still waiting for a credential`,
  },
  mcp_tools_refused: {
    tone: "warning",
    title: ({ server, count }) =>
      count === 0
        ? `Some of ${server}'s tools are not offered`
        : count === 1
          ? `1 ${server} tool is not offered`
          : `${count} ${server} tools are not offered`,
  },
  mcp_oauth_needs_sign_in: {
    tone: "danger",
    title: ({ server }) => `${server} needs somebody to sign in`,
  },
  mcp_oauth_expiring: {
    tone: "info",
    title: ({ server }) => `${server}'s sign-in runs out soon`,
  },
  mcp_approval_waiting: {
    tone: "warning",
    title: ({ server, who }) =>
      who ? `${who} is waiting for your approval` : `A call to ${server} is waiting for approval`,
  },
  mcp_tools_changed: {
    tone: "warning",
    title: ({ server }) => `${server} changed tools since you allowed them`,
  },
};
