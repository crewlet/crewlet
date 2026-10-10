/**
 * The Crewlet characters an agent seat can be drawn as, by id, in the order
 * the seat editor offers them. The FIRST is what a seat that names no
 * character is drawn as: the original Crewlet.
 *
 * THE ENGINE'S SET AND THE DESIGN SYSTEM'S, held from both sides: against
 * `config.AvatarCharacters` by `internal/config/avatar_test.go`'s
 * `TestTheAvatarSetsAreTheDashboards`, and against `@crewlethq/icons`'
 * `CREWLET_CHARACTERS` by `lib/avatar.test.ts`. An id the engine admits that
 * the kit cannot draw is a seat drawn as nothing; one the editor offers that
 * the engine refuses is a pick whose every save fails.
 */
export const AVATAR_CHARACTERS = [
  "crewlet",
  "hexlet",
  "peaklet",
  "prismlet",
  "towerlet",
  "bricklet",
  "gemlet",
  "coglet",
  "pluslet",
  "shieldlet",
  "stacklet",
  "sparklet",
  "cloudlet",
  "foxlet",
  "rocketlet",
  "crownlet",
  "cactlet",
  "wisplet",
  "heartlet",
  "flasklet",
  "chiplet",
  "duolet",
  "chatlet",
  "pagelet",
  "archlet",
  "dashlet",
  "octlet",
  "conelet",
  "hivelet",
  "folderlet",
] as const;

/**
 * The six colours an agent seat can be drawn in, in the engine's order. The
 * FIRST is what a seat that names no colour is drawn in: purple.
 *
 * Held as {@link AVATAR_CHARACTERS} is: against `config.AvatarColors`, and
 * against `@crewlethq/ui`'s `NODE_HUES`.
 */
export const AVATAR_COLORS = ["purple", "cyan", "green", "amber", "rose", "blue"] as const;
