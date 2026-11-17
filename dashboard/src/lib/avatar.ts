/**
 * How an agent seat is drawn: one of the Crewlet characters, in one of the
 * six seat colours, as the engine serves it on every seat it publishes: each
 * part as the seat's author chose it, or picked from the seat's handle.
 *
 * ONE IMPLEMENTATION, THE ENGINE'S. Nothing here picks a look: a screen draws
 * the look it is served, and a part this build's kit cannot draw (a character
 * a newer engine admitted) is no look, drawn as the seat's initials. The org
 * builder lays a seat's AUTHORED parts over the look the running chart serves.
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

const isCharacter = (value: unknown): value is AvatarCharacter =>
  (AVATAR_CHARACTERS as readonly unknown[]).includes(value);

const isColor = (value: unknown): value is AvatarColor =>
  (AVATAR_COLORS as readonly unknown[]).includes(value);

/**
 * An agent's look as the kit's `character` and `hue`, the one translation every
 * face goes through; nothing for a person or a key the chart does not hold.
 */
export function faceOf(
  kind: "human" | "agent",
  avatar: AgentAvatar | null | undefined,
): { character?: AvatarCharacter; hue?: AvatarColor } {
  return kind === "agent" && avatar ? { character: avatar.character, hue: avatar.color } : {};
}

/** The look a block names, where both of its parts are ones this build draws; null otherwise. */
export function avatarOf(
  raw: { character?: unknown; color?: unknown } | null | undefined,
): AgentAvatar | null {
  return isCharacter(raw?.character) && isColor(raw?.color)
    ? { character: raw.character, color: raw.color }
    : null;
}

/**
 * A seat's look in the org builder: each part its author wrote, the part it
 * left out as the running chart serves it, and null where neither says.
 */
export function lookOf(
  authored: { character?: unknown; color?: unknown } | null | undefined,
  served: AgentAvatar | null | undefined,
): AgentAvatar | null {
  const character = isCharacter(authored?.character) ? authored.character : served?.character;
  const color = isColor(authored?.color) ? authored.color : served?.color;
  return character && color ? { character, color } : null;
}
