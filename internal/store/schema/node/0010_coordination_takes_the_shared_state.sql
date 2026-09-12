-- The state that moved to the fleet's coordination store.
--
-- Four tables here answered questions a FLEET has to agree on, while this
-- database is the node's own — "one file, one process", as store.go says. So
-- each was per-node while its own comments described a company:
--
--   webhook_deliveries    a vendor retrying a delivery reaches whichever
--                         ingress node the load balancer picks, and a claim
--                         only one node could see suppressed nothing. The
--                         same push woke the same seat twice.
--   rate_limits           the notification valve called itself "the shared
--                         fixed-window counter". Four nodes ran four of them,
--                         so a seat capped at five a second could emit twenty.
--   config_activations    the activation pointer, whose epoch is a fencing
--   config_apply_status   token, and the per-node status the fleet view is
--                         drawn from. Each node was reading its own row and
--                         drawing a fleet of one.
--   turn_completions      the record that stops a redelivered trigger being
--                         worked again. A redelivery that landed on a peer
--                         found nothing and ran the turn a second time.
--
-- They now live in internal/coord, where the KV bucket's own age is the
-- retention — which is also why the maintenance sweep no longer has jobs for
-- them. See internal/coord/fleet.go.
--
-- DROPPED rather than left in place: a table nothing reads is a table the
-- next reader assumes is authoritative, and these four are exactly the shape
-- somebody would wire a dashboard query to.
--
-- conversation_sessions stays. A seat's own thread history is read only by
-- the node running that seat, and replicating a long conversation to the
-- whole fleet would buy nothing.

DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS rate_limits;
DROP TABLE IF EXISTS config_apply_status;
DROP TABLE IF EXISTS config_activations;
DROP TABLE IF EXISTS turn_completions;
