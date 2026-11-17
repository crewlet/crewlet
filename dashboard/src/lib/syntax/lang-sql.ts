/**
 * SQL, its words in any case; a state carries an open `'` string, a dollar
 * quote's tag (`$$`, `$body$`) or a block comment.
 */
import { Scan, clike, until, word } from "./scan.ts";
import type { Grammar } from "./types.ts";

const DOLLAR = /\$[A-Za-z_]*\$/y;
/** The longest dollar quote tag carried. */
const MAX_TAG = 32;

/** A `'` string's rest from `i`, `''` an escaped quote; leaves it open at the line's end. */
function quoted(s: Scan, i: number): number {
  const t = s.text;
  for (let j = i; j < t.length; j++) {
    if (t[j] !== "'") continue;
    if (t[j + 1] === "'") j++;
    else return s.emit(j + 1, "string", true);
  }
  s.out.push(t.length, "string");
  s.open = "'";
  return t.length;
}

/** A dollar-quoted body's rest from `i` to `tag`; leaves it open at the line's end. */
function dollar(s: Scan, i: number, tag: string): number {
  const end = until(s.text, i, tag);
  if (end >= 0) return s.emit(end, "string", true);
  s.out.push(s.text.length, "string");
  if (tag.length <= MAX_TAG) s.open = tag;
  return s.text.length;
}

export const sql: Grammar = clike({
  keywords:
    "select from where and or not in is like ilike between exists insert into values update set delete create alter drop table view index unique primary foreign key references constraint default check on join inner left right full outer cross natural using group by order having limit offset union all distinct as case when then else end begin commit rollback transaction with recursive returning grant revoke if replace function returns language trigger for each row execute procedure declare do loop return asc desc nulls first last cascade restrict schema database sequence type cast collate over partition window filter conflict nothing lateral",
  constants: "true false null unknown",
  types:
    "int integer smallint bigint serial bigserial real double precision float numeric decimal text varchar char character varying boolean bool date time timestamp timestamptz interval uuid json jsonb bytea blob clob",
  typeAfter: "table",
  caseless: true,
  line: ["--"],
  block: ["/*", "*/"],
  quotes: '"`',
  hooks: [
    (s, i) => (s.text[i] === "'" ? quoted(s, i + 1) : -1),
    (s, i) => {
      if (s.text[i] !== "$") return -1;
      const end = word(s.text, i, DOLLAR);
      return end > i ? dollar(s, end, s.text.slice(i, end)) : -1;
    },
  ],
  resume: (s, open) => {
    if (open === "'") return quoted(s, 0);
    if (open.length >= 2 && open.length <= MAX_TAG && word(open, 0, DOLLAR) === open.length) {
      return dollar(s, 0, open);
    }
    return -1;
  },
});
