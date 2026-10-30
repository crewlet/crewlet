/**
 * The presence vocabulary — who is in a room right now and what they are
 * doing there (`internal/presence`, ADR-0038).
 *
 * A ROOM IS NAMED BY ITS KIND AND ITS ID: an artifact's room by the artifact,
 * a chat room's by the room. Every room rides the one hub, so the `presence`
 * frame a tab sends names the room's kind, and what a member is doing is one
 * set of activities whatever the room.
 *
 * Held to the engine by `internal/presence`'s
 * `TestTheDashboardSpeaksThePresenceVocabulary`, in the engine's order.
 */

/** The kinds of room a `presence` frame names (`presence.RoomKinds`). */
export const PRESENCE_ROOMS = ["artifact", "chat"] as const;

/** What somebody present in a room is doing (`presence.Activities`). */
export const PRESENCE_ACTIVITIES = ["viewing", "editing", "idle"] as const;

/**
 * The most presence frames one socket sends a second (`presence.ClientRate`),
 * whatever rooms they name. The engine drops the rest, so a screen coalesces
 * what it says to this.
 */
export const PRESENCE_FRAMES_PER_SECOND = 10;
