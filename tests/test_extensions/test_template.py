"""Tests for the community extension template."""

from __future__ import annotations

import pytest

from crewlet.extensions.protocol import ExtensionContext
from crewlet.extensions.template import TemplateExtension


class TestTemplateExtension:
    def test_name(self):
        ext = TemplateExtension()
        assert ext.name == "template"

    def test_version(self):
        ext = TemplateExtension()
        assert ext.version == "0.1.0"

    @pytest.mark.asyncio
    async def test_lifecycle(self):
        ext = TemplateExtension()
        ctx = ExtensionContext()
        await ext.on_register(ctx)
        await ext.on_engine_start(ctx)
        await ext.on_engine_stop(ctx)

    def test_satisfies_extension_protocol(self):
        from crewlet.extensions.protocol import Extension

        ext = TemplateExtension()
        assert isinstance(ext, Extension)
