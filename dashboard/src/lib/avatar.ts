/**
 * How an agent seat is drawn: one of the Crewlet characters, in one of the
 * six seat colours, both chosen by whoever authors the seat.
 *
 * ONE READING OF THE WIRE, here, so every screen draws a seat the same way.
 * The engine fills an agent's defaults in before it publishes the org, so a
 * seat read off the projection always names both parts; a part this build's
 * kit cannot draw (a character a newer engine admitted) is drawn as the
 * default part rather than as nothing. The org builder reads a seat's
 * AUTHORED block, where either part may be absent, through the same rule.
 *
 * The lists themselves are the contract's (`contract/avatar.ts`), which the
 * engine and this build's design system are both held to.
 */

import { AVATAR_CHARACTERS, AVATAR_COLORS } from "../contract/avatar.ts";

export type AvatarCharacter = (typeof AVATAR_CHARACTERS)[number];
export type AvatarColor = (typeof AVATAR_COLORS)[number];

/** An agent seat's look: the character it is drawn as, and its colour. */
export interface AgentAvatar {
  readonly character: AvatarCharacter;
  readonly color: AvatarColor;
}

/** What a seat that chose nothing is drawn as: the first of each list. */
export const DEFAULT_AVATAR: AgentAvatar = {
  character: AVATAR_CHARACTERS[0],
  color: AVATAR_COLORS[0],
};

const isCharacter = (value: unknown): value is AvatarCharacter =>
  (AVATAR_CHARACTERS as readonly unknown[]).includes(value);

const isColor = (value: unknown): value is AvatarColor =>
  (AVATAR_COLORS as readonly unknown[]).includes(value);

/**
 * The avatar a block describes, part by part: each part as written where it
 * is one this build draws, and the default part otherwise.
 */
export function avatarOf(
  raw: { character?: unknown; color?: unknown } | null | undefined,
): AgentAvatar {
  return {
    character: isCharacter(raw?.character) ? raw.character : DEFAULT_AVATAR.character,
    color: isColor(raw?.color) ? raw.color : DEFAULT_AVATAR.color,
  };
}
