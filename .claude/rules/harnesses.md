---
paths:
  - "internal/{providers,agent,toolsession,sandbox,mcprelay,mcp,assistant,compact,learning,tools,engine}/**"
  - "internal/api/mcpbridge/**"
  - "docs/concepts/llm-backends.md"
  - ".claude/worktrees/*/internal/{providers,agent,toolsession,sandbox,mcprelay,mcp,assistant,compact,learning,tools,engine}/**"
  - ".claude/worktrees/*/internal/api/mcpbridge/**"
  - ".claude/worktrees/*/docs/concepts/llm-backends.md"
---

# Harnesses
- **Harnesses are the only LLM backend** — every model call runs a vendor's own harness as a subprocess (Claude Code for Anthropic's models, on a plan or an API key; Codex for OpenAI's; OpenCode for open models), driven through `internal/providers/llm/harness`. There is no HTTP client to a model vendor and none is to be added: the vendors move their request shapes, caching and sign-in with every model, and their harnesses carry that for us (`docs/concepts/llm-backends.md`). The harness runs its OWN loop: a phase's tools reach it as ONE MCP server, `crewlet`, which it calls natively. A seat's executor is always its harness's own fully permissioned run in a box (Claude Code or OpenCode, the two harnesses with an in-box runner; ADR-0066), reaching the seat's tools over the bridge (`internal/api/mcpbridge`), which holds every call to the seat's surface; every other phase (the reviewer, delegated workers, the onboarding pass, the side passes) runs on the engine host with the harness's local tools denied, through `crewlet mcp-relay` into a per-run unix socket (`internal/toolsession`), every call held to the phase's gates by the tool loop. No tool rides a prompt and no call is read out of a model's prose (ADR-0049)
