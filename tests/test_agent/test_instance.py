"""Tests for AgentInstance."""

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.org.models import Organization, OrgUnit, Role


def make_agent() -> AgentInstance:
    org = Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Engineer",
                roles=[Role(name="Engineer")],
            )
        ],
    )
    role = org.get_role("Engineer")
    assert role is not None
    defn = AgentDefinition(role=role, org=org)
    return AgentInstance(defn)


def test_initial_state():
    agent = make_agent()
    assert agent.state == AgentState.CREATED
    assert agent.role_name == "Engineer"
    assert agent.current_task_id is None
    assert not agent.is_available


def test_activate():
    agent = make_agent()
    agent.activate()
    assert agent.state == AgentState.IDLE
    assert agent.is_available


def test_start_working():
    agent = make_agent()
    agent.activate()
    agent.start_working("t1")
    assert agent.state == AgentState.WORKING
    assert agent.current_task_id == "t1"
    assert not agent.is_available


def test_finish_working():
    agent = make_agent()
    agent.activate()
    agent.start_working("t1")
    agent.finish_working()
    assert agent.state == AgentState.IDLE
    assert agent.current_task_id is None
    assert agent.is_available


def test_await_sandbox_keeps_task_and_is_busy():
    agent = make_agent()
    agent.activate()
    agent.start_working("t1")
    assert agent.await_sandbox() is True
    assert agent.state == AgentState.AWAITING_SANDBOX
    # The job tracks the task id; the agent is busy (not available).
    assert agent.current_task_id == "t1"
    assert not agent.is_available


def test_await_sandbox_from_idle_for_recovery_race():
    # The kick-off event and the turn's finish_working race with no fixed
    # order, so AWAITING_SANDBOX is reachable from IDLE too (and on
    # post-restart recovery, where the agent re-activates to IDLE first).
    agent = make_agent()
    agent.activate()  # IDLE
    assert agent.await_sandbox(task_id="t-recover") is True
    assert agent.state == AgentState.AWAITING_SANDBOX
    assert agent.current_task_id == "t-recover"


def test_await_sandbox_rejected_when_terminated():
    agent = make_agent()
    agent.activate()
    agent.terminate()
    assert agent.await_sandbox() is False
    assert agent.state == AgentState.TERMINATED


def test_resume_from_sandbox_frees_agent():
    agent = make_agent()
    agent.activate()
    agent.start_working("t1")
    agent.await_sandbox()
    assert agent.resume_from_sandbox() is True
    assert agent.state == AgentState.IDLE
    assert agent.current_task_id is None
    assert agent.is_available


def test_resume_from_sandbox_only_from_awaiting():
    agent = make_agent()
    agent.activate()
    agent.start_working("t1")  # WORKING, not AWAITING_SANDBOX
    assert agent.resume_from_sandbox() is False
    assert agent.state == AgentState.WORKING


def test_terminate():
    agent = make_agent()
    agent.activate()
    agent.terminate()
    assert agent.state == AgentState.TERMINATED
    assert not agent.is_available


def test_unique_ids():
    a1 = make_agent()
    a2 = make_agent()
    assert a1.id != a2.id


def test_id_str():
    agent = make_agent()
    assert isinstance(agent.id_str, str)
    assert len(agent.id_str) > 0
