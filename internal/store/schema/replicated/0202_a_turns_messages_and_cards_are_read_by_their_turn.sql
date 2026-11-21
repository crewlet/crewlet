-- A turn's messages and cards are read by the turn.
--
-- A stream that lost a turn's frames is answered with a snapshot of that one
-- turn (assistantdomain.Reader.TurnTranscript). Over the conversation's
-- indexes alone the read visited every message and card of every turn before
-- it; these hold one turn's rows together, in log order, as
-- assistant_steps_conversation_idx already does for steps.
--
-- WHO HAS TO AGREE ON IT: nobody. An index moves no row.
CREATE INDEX assistant_messages_turn_idx ON assistant_messages (conversation_id, turn_id, version); -- read by assistantdomain's Reader.TurnTranscript (read.go)
CREATE INDEX assistant_cards_turn_idx ON assistant_cards (conversation_id, turn_id, version);       -- read by assistantdomain's Reader.TurnTranscript (read.go)
