/** The kinds a token is coloured as: the kit's seven syntax inks. */
export const KINDS = [
  "keyword",
  "string",
  "number",
  "function",
  "type",
  "comment",
  "punctuation",
] as const;
export type Kind = (typeof KINDS)[number];

/** A run of a line, up to `end` (exclusive), in code units; `kind` null is plain. */
export interface Token {
  end: number;
  kind: Kind | null;
}

/** What a line leaves open for the next (a comment, a raw string, a heredoc); "" at a file's start. */
export type State = string;

/** One line's tokens and the state it leaves. */
export interface Line {
  tokens: Token[];
  state: State;
}

/** One language: a line's tokens from the state the line before it left. */
export interface Grammar {
  line(text: string, state: State): Line;
}
