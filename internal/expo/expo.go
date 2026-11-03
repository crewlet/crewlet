// Package expo gives each agent seat its own Expo robot user on the company's
// Expo account, with its own access token, at the seat's level.
//
// # A robot per seat
//
// Expo robot users act for an account, carry permissions there and sign in
// only with access tokens, so each agent acts as itself and its actions are
// attributable and revocable on their own. The per-seat access vendor
// ([seataccess.Vendor]) creates the robot through the company's provisioner
// token (an access token whose actor holds Admin on the account), mints its
// token, grants and revokes its permissions when the seat's level changes,
// rotates its token, and deletes the robot when the seat is turned off.
//
// # Only what the ledger records is the engine's
//
// Anyone holding Admin can create a robot under any name, so a name is never
// proof of provenance. The engine acts only on robot IDs its ledger recorded
// when it created them, and adopts nothing by name. A token is the engine's
// when it sits on such a robot and carries the engine's note ([TokenNote]).
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
package expo

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "expo"
