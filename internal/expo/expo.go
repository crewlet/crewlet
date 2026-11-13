// Package expo gives each agent seat its own Expo robot user on the company's
// Expo account, with its own access token, at the seat's level.
//
// # A person creates each robot
//
// Expo lets robot users be created only in its website, so a person creates
// each seat's robot there and pastes its access token ([seataccess.HandsIn]).
// The engine checks the token with reads only (it is a robot's, the robot holds
// the seat's role on the account, and no other seat has it), seals it, and
// from then on reads everything about the robot with that token. The engine
// holds no Expo token that manages robots: a role change, a turned-off
// robot's deletion and a rotate's old tokens are a person's steps at Expo,
// each confirmed by reading the robot back.
//
// # A robot is its ID, never its name
//
// A robot's name is a label a person types, so it proves nothing about
// provenance. The engine identifies a robot by the ID its token proves, and
// records that ID in the ledger when the seat's first token is pasted.
//
// # The token reaches the agent's box by reference
//
// The seat's token is sealed as EXPO_TOKEN_<HANDLE>. A seat whose box runs the
// EAS CLI names it in `role.sandbox.env` as `EXPO_TOKEN`, the variable the CLI
// reads, and the engine holds a launch for it while the access sync mints it.
//
// # Expo's GraphQL API
//
// Every call is one hand-written operation against [GraphQLURL]. A refusal
// arrives as HTTP 200 with `errors`, which the client reads as an [APIError]
// carrying the status its code stands for.
//
// # The atlas reads as a robot of its own
//
// The atlas reads the account's projects and their latest builds only where a
// person made one more robot for it alone (`integrations.expo.atlas.token`,
// ADR-0146): a Viewer, proven before every read to hold that role and to be
// no agent's ([CheckAtlas]). The one query names every field it is sent.
package expo

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "expo"
