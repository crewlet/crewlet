"""Tests for AgentMemory."""

from crewlet.agent.memory import AgentMemory


def test_add_and_get():
    mem = AgentMemory()
    mem.add("user", "Hello")
    mem.add("assistant", "Hi there")
    assert len(mem.entries) == 2


def test_get_recent():
    mem = AgentMemory()
    for i in range(10):
        mem.add("user", f"msg-{i}")
    recent = mem.get_recent(3)
    assert len(recent) == 3
    assert recent[0].content == "msg-7"


def test_eviction():
    mem = AgentMemory(max_size=5)
    for i in range(10):
        mem.add("user", f"msg-{i}")
    assert len(mem.entries) == 5
    # Oldest should be evicted
    assert mem.entries[0].content == "msg-5"
    assert mem.entries[-1].content == "msg-9"


def test_clear():
    mem = AgentMemory()
    mem.add("user", "Hello")
    mem.clear()
    assert len(mem.entries) == 0


def test_to_messages():
    mem = AgentMemory()
    mem.add("user", "Hello")
    mem.add("assistant", "Hi")
    msgs = mem.to_messages()
    assert len(msgs) == 2
    assert msgs[0] == {"role": "user", "content": "Hello"}
    assert msgs[1] == {"role": "assistant", "content": "Hi"}
