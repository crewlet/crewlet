/**
 * The NAME a `${NAME}` reference carries between its braces, as the source of
 * a regular expression — the engine's `envref.NameGrammar`.
 *
 * A screen decides from it whether a value is a reference: to mask a
 * credential field, to tell a provisionable value from a hand-written one, to
 * label a configuration value, to refuse a "may be unset" row whose value
 * names nothing before it is sent. Held against the engine's grammar by
 * `internal/envref/client_gate_test.go`: a copy that drifted would mask a
 * reference as a secret, show a secret as a reference, or refuse a row the
 * engine accepts. Read through `lib/references.ts`, never spelled again
 * beside a screen.
 */
export const REFERENCE_NAME = "[A-Za-z_][A-Za-z0-9_]*";
