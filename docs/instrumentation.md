# Instrumenting an agent

Waveoff needs one thing from your agent that a proxy cannot provide for it: a
session boundary.

## What is genuinely zero-code-change, and what is not

**Zero:** the recorder is a sidecar proxy. The injection webhook rewrites
`ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` and the MCP endpoints to point at
`localhost`, and your agent keeps talking to what it thinks is its provider.
Nothing in your code changes.

**Not zero:** correlating a multi-step agent loop into *one* recorded session.
The recorder derives a session from W3C trace context on the outgoing request.
Without it, every HTTP call the agent makes looks unrelated to every other, and
the recorder honestly files each as its own session.

This is two lines of OpenTelemetry bootstrap, not a rewrite. But it is not
nothing, and the difference matters enough to be worth stating plainly rather
than discovering later.

## What you lose without it

| Capability | Uninstrumented | Instrumented |
|---|---|---|
| Recording model and tool traffic | works | works |
| Credential redaction | works | works |
| Contract drift detection | works | works |
| Payload-level replay | works | works |
| **Divergence detection** | **not possible** | works |
| **Paired comparison over tasks** | **no valid unit** | works |

The bottom two rows are the reason this page exists.

Divergence detection needs an ordered multi-step path to compare against. One
call per session has no path to depart from.

The gating design measures **task-level** outcomes — task completion, cost per
*completed task*, consistency across repeated runs of the same task. If a
session is a single model call, the unit of a paired test is one call rather
than one task. The statistics remain valid and answer a question nobody asked.

## Doing it

Any OpenTelemetry setup that propagates trace context on outgoing HTTP works.
Waveoff reads the standard `traceparent` header and needs no vendor of ours.

### Python

```python
from opentelemetry import trace
from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
from opentelemetry.sdk.trace import TracerProvider

trace.set_tracer_provider(TracerProvider())
HTTPXClientInstrumentor().instrument()

tracer = trace.get_tracer("my-agent")
with tracer.start_as_current_span("agent.invoke"):
    result = agent.invoke(...)
```

The span is the session. Everything the agent does inside it — model calls, tool
calls — shares a trace id, and the recorder sees one recording.

Use `opentelemetry-instrumentation-requests` or `-aiohttp` instead if your SDK
does not use httpx.

### A trap worth knowing

**Open the span before you construct any client**, not after.

OpenTelemetry context lives in contextvars, and asynchronous clients do work on
tasks created when the client is built. An MCP client constructed outside the
span performs its whole handshake — `initialize`, `tools/list` — with no trace
context, and the recorder correctly files each of those calls as a session of
its own.

We found this the obvious way: one agent run produced six cassettes. Moving the
client construction inside the span produced one, with all thirteen calls
correlated.

```python
with tracer.start_as_current_span("agent.invoke"):
    tools = await mcp_client.get_tools()      # inside
    agent = create_agent(model, tools)
    return await agent.ainvoke(...)
```

`test/fixtures/langgraph/agent.py` is a working example, and
`TestInstrumentedAgentIsOneSession` / `TestUninstrumentedAgentDegrades` pin both
behaviours.

### If you already run LLM observability

You are probably already done. LangSmith, OpenLLMetry and OpenInference all
propagate W3C trace context on outgoing requests. Check for a `traceparent`
header on a model call and, if it is there, so is your session boundary.

## The escape hatch

If trace context is genuinely unavailable, set the session yourself:

```
X-Waveoff-Session: <a stable id for this agent run>
```

It takes precedence over trace context. This is a code change too, but a smaller
one, and it is the right answer for a runtime that has no OpenTelemetry support
at all.
