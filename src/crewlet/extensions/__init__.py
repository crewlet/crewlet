"""Extension system — hooks and middleware for extending the engine."""

from crewlet.extensions.loader import ExtensionManager
from crewlet.extensions.protocol import Extension, ExtensionContext
from crewlet.extensions.template import TemplateExtension

__all__ = [
    "Extension",
    "ExtensionContext",
    "ExtensionManager",
    "TemplateExtension",
]
