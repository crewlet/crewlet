-- An answered ask is found by whom it was put to: the index the answered read
-- (worklet.Reader.AnsweredAsks, the `answered_decisions` question) searches.
--
-- # The reader
--
-- "What did I decide lately": the asks put to one person that were answered,
-- by them or by somebody else, or resolved without an answer, newest closing
-- first, within a working week. It selects on the person (`ask`) and on the
-- ask being closed, and on nothing that narrows it first, so without this
-- index it is a scan of `tracker_comments`, a table that is never swept and
-- grows for the life of the company.
--
-- # Why partial, and why on `ask` alone
--
-- 0002's two partial indexes on this table hold the OPEN asks, which is what
-- keeps them the size of the answer. This one is their complement over the
-- same rows: an ask (`ask <> ''`) that is not removed and is closed, so a
-- remark, a reply and an answer are never in it. The predicate is the one the
-- reader states verbatim, so the planner proves the implication term for term
-- and SEARCHes on the index (asserted by worklet's
-- TestTheAnsweredReadSearchesItsIndex).
--
-- The window is not a column of it: when an ask closed is its answer's
-- instant, a different row's, and the index range is already one person's
-- closed asks, which the window then cuts by reading each one's answer by its
-- primary key (the ask's `answered_by` is the answer's id).

CREATE INDEX tracker_comments_answered_asks_idx ON tracker_comments (ask)
    WHERE ask <> '' AND removed = 0 AND (resolved = 1 OR answered_by IS NOT NULL);   -- answered_decisions (what a person decided lately)
