"""Subject / topic pattern matching (the engine's internal subject syntax).

These dot-separated subjects with ``*`` / ``>`` wildcards are the queue's
backend-neutral subject language; the Pulsar backend translates them to
its own topic regex (see :meth:`crewlet.queue.pulsar.PulsarEventQueue.
_pattern_to_regex`), while the in-memory backend matches with the
function below.
"""

from __future__ import annotations


def topic_matches(pattern: str, topic: str) -> bool:
    """Return True when *topic* matches subject-wildcard *pattern*.

    Patterns use these conventions:

    - ``*`` matches exactly one subject segment.
    - ``>`` matches one-or-more trailing segments and is valid only at
      the end of the pattern.
    - Any other token must match the corresponding segment exactly.

    Examples::

        topic_matches("crewlet.events.>", "crewlet.events.task_created") -> True
        topic_matches("crewlet.events.*", "crewlet.events.task.created") -> False
        topic_matches("crewlet.events.task_*", "crewlet.events.task_done")  -> False
        topic_matches("foo.bar", "foo.bar") -> True
    """
    pattern_parts = pattern.split(".")
    topic_parts = topic.split(".")
    for index, part in enumerate(pattern_parts):
        if part == ">":
            return index < len(topic_parts)
        if index >= len(topic_parts):
            return False
        if part == "*":
            continue
        if part != topic_parts[index]:
            return False
    return len(pattern_parts) == len(topic_parts)
