def test_a_merged_body_is_capped_like_its_documented_sibling():
    """`interaction_text` caps the joined text; this did not.

    Each constituent body is clipped at construction, but a coalesced
    trigger can carry `max_batch` of them from one chatty sender — and
    the merged body goes straight into the counterparty profiler's
    prompt.
    """
    from crewlet.learning.interaction import (
        INTERACTION_BODY_LIMIT,
        CanonicalIdentity,
        InboundInteraction,
        merge_interactions_by_sender,
    )

    sender = CanonicalIdentity(handle="", external_id="U1", platform="slack")
    chatty = [
        InboundInteraction(sender=sender, body="x" * INTERACTION_BODY_LIMIT)
        for _ in range(20)
    ]

    merged = merge_interactions_by_sender(chatty)

    assert len(merged) == 1
    assert len(merged[0].body) <= INTERACTION_BODY_LIMIT + len(" … [truncated]")


def test_a_short_merge_is_untouched():
    from crewlet.learning.interaction import (
        CanonicalIdentity,
        InboundInteraction,
        merge_interactions_by_sender,
    )

    sender = CanonicalIdentity(handle="", external_id="U1", platform="slack")
    merged = merge_interactions_by_sender(
        [
            InboundInteraction(sender=sender, body="first"),
            InboundInteraction(sender=sender, body="second"),
        ]
    )
    assert merged[0].body == "first\n\nsecond"
