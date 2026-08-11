"""Agent-learning subsystem.

The package re-exports lightweight types eagerly and provides
:func:`__getattr__` lazy access for the heavy modules
(``CounterpartyProfiler``, ``PersistDecider``, ``ReflectEngine``,
etc.) so importing ``crewlet.events.types`` does not pull the entire
dependency graph into the import cycle.

See ``docs/concepts/agent-learning.md`` for the full design.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from crewlet.learning.interaction import (
    CanonicalIdentity,
    ChannelKind,
    InboundInteraction,
    interactions_require_recon,
    merge_interactions_by_sender,
    salient_task_text,
)
from crewlet.learning.models import (
    CounterpartyProfile,
    Episode,
    SynthesizedSkill,
    SynthesizedSkillVersion,
)

if TYPE_CHECKING:  # pragma: no cover - import only for type checkers
    from crewlet.learning.counterparty_profiler import (
        CounterpartyProfiler as CounterpartyProfiler,
    )
    from crewlet.learning.counterparty_store import (
        CounterpartyStore as CounterpartyStore,
    )
    from crewlet.learning.episode_store import EpisodeStore as EpisodeStore
    from crewlet.learning.onboarding import (
        ONBOARDING_PAGE_TITLE as ONBOARDING_PAGE_TITLE,
    )
    from crewlet.learning.onboarding import (
        build_onboarding_hint as build_onboarding_hint,
    )
    from crewlet.learning.onboarding import compute_chain_hash as compute_chain_hash
    from crewlet.learning.onboarding import is_onboarded as is_onboarded
    from crewlet.learning.onboarding import (
        register_mark_onboarded_tool as register_mark_onboarded_tool,
    )
    from crewlet.learning.persist_decider import PersistDecider as PersistDecider
    from crewlet.learning.reflect_engine import ReflectEngine as ReflectEngine
    from crewlet.learning.skill_refiner import SkillRefiner as SkillRefiner
    from crewlet.learning.skill_scheduler import (
        SkillClusteringScheduler as SkillClusteringScheduler,
    )
    from crewlet.learning.skill_synthesizer import (
        PromotionSynthesizer as PromotionSynthesizer,
    )
    from crewlet.learning.skill_synthesizer import (
        SkillSynthesizer as SkillSynthesizer,
    )
    from crewlet.learning.summarize import summarize_episodes as summarize_episodes
    from crewlet.learning.synthesized_skill_store import (
        SynthesizedSkillStore as SynthesizedSkillStore,
    )
    from crewlet.learning.tools import register_episode_tools as register_episode_tools
    from crewlet.learning.tools import (
        register_refine_skill_tool as register_refine_skill_tool,
    )
    from crewlet.learning.tools import (
        register_reflect_and_persist_tool as register_reflect_and_persist_tool,
    )
    from crewlet.learning.tools import (
        was_reflect_and_persist_called as was_reflect_and_persist_called,
    )

# Map exported name -> (submodule, attribute) for the lazy heavy paths.
_LAZY_EXPORTS: dict[str, tuple[str, str]] = {
    "CounterpartyProfiler": ("counterparty_profiler", "CounterpartyProfiler"),
    "CounterpartyStore": ("counterparty_store", "CounterpartyStore"),
    "EpisodeStore": ("episode_store", "EpisodeStore"),
    "ONBOARDING_PAGE_TITLE": ("onboarding", "ONBOARDING_PAGE_TITLE"),
    "build_onboarding_hint": ("onboarding", "build_onboarding_hint"),
    "compute_chain_hash": ("onboarding", "compute_chain_hash"),
    "is_onboarded": ("onboarding", "is_onboarded"),
    "register_mark_onboarded_tool": ("onboarding", "register_mark_onboarded_tool"),
    "PersistDecider": ("persist_decider", "PersistDecider"),
    "ReflectEngine": ("reflect_engine", "ReflectEngine"),
    "SkillRefiner": ("skill_refiner", "SkillRefiner"),
    "SkillClusteringScheduler": ("skill_scheduler", "SkillClusteringScheduler"),
    "PromotionSynthesizer": ("skill_synthesizer", "PromotionSynthesizer"),
    "SkillSynthesizer": ("skill_synthesizer", "SkillSynthesizer"),
    "summarize_episodes": ("summarize", "summarize_episodes"),
    "SynthesizedSkillStore": ("synthesized_skill_store", "SynthesizedSkillStore"),
    "jaccard": ("skill_synthesizer", "jaccard"),
    "register_episode_tools": ("tools", "register_episode_tools"),
    "register_refine_skill_tool": ("tools", "register_refine_skill_tool"),
    "register_reflect_and_persist_tool": (
        "tools",
        "register_reflect_and_persist_tool",
    ),
    "was_reflect_and_persist_called": ("tools", "was_reflect_and_persist_called"),
}


def __getattr__(name: str) -> Any:  # pragma: no cover - trivial dispatcher
    target = _LAZY_EXPORTS.get(name)
    if target is None:
        raise AttributeError(f"module 'crewlet.learning' has no attribute {name!r}")
    module_name, attr = target
    import importlib

    module = importlib.import_module(f"crewlet.learning.{module_name}")
    value = getattr(module, attr)
    globals()[name] = value
    return value


__all__ = [
    "CanonicalIdentity",
    "ChannelKind",
    "CounterpartyProfile",
    "CounterpartyProfiler",
    "CounterpartyStore",
    "Episode",
    "EpisodeStore",
    "InboundInteraction",
    "ONBOARDING_PAGE_TITLE",
    "PersistDecider",
    "PromotionSynthesizer",
    "ReflectEngine",
    "SkillClusteringScheduler",
    "SkillRefiner",
    "SkillSynthesizer",
    "SynthesizedSkill",
    "SynthesizedSkillStore",
    "SynthesizedSkillVersion",
    "build_onboarding_hint",
    "compute_chain_hash",
    "interactions_require_recon",
    "is_onboarded",
    "jaccard",
    "merge_interactions_by_sender",
    "register_mark_onboarded_tool",
    "register_episode_tools",
    "register_refine_skill_tool",
    "register_reflect_and_persist_tool",
    "salient_task_text",
    "summarize_episodes",
    "was_reflect_and_persist_called",
]
