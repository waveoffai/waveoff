# Copyright 2026 The Waveoff Authors.
# SPDX-License-Identifier: Apache-2.0
"""A real LangGraph agent, for exercising the recorder end to end.

This is a test fixture, not a product surface. It exists because a recorder
proven only against traffic we generated ourselves is proven against our own
assumptions: the shapes that matter here — how the Anthropic SDK frames a
request, how a framework drives a tool-call loop, how streaming is consumed —
are exactly the ones a hand-written stub would get wrong in the same direction
as the code under test.

The model endpoint is fake and the tool is local, so the whole thing is
deterministic and needs no API key. Everything between them is genuine.
"""

import json
import os
import sys

from langchain_anthropic import ChatAnthropic
from langchain_core.messages import HumanMessage
from langchain_core.tools import tool
try:  # LangGraph v1 moved this; support both so the fixture is not version-locked.
    from langchain.agents import create_agent as create_react_agent
except ImportError:  # pragma: no cover
    from langgraph.prebuilt import create_react_agent


@tool
def lookup_refund_policy(topic: str) -> str:
    """Look up the refund policy for a topic."""
    return json.dumps({"topic": topic, "window_days": 30})


async def mcp_tools(endpoint: str) -> list:
    """Load tools from a real MCP server over streamable HTTP.

    This is what makes the tool plane real. A local Python tool never leaves
    the process, so it proves nothing about the recorder's MCP proxy or about
    replay serving a tool result. Pointed at WAVEOFF_MCP_ENDPOINT the agent
    speaks the actual protocol to whatever is on the other end — which during
    recording is the recorder, and during replay is the replayer.
    """
    from langchain_mcp_adapters.client import MultiServerMCPClient

    client = MultiServerMCPClient(
        {"everything": {"url": endpoint, "transport": "streamable_http"}}
    )
    return await client.get_tools()


def instrument() -> None:
    """Turn on OpenTelemetry so the recorder can see session boundaries.

    Without this every HTTP call the agent makes looks unrelated to the
    recorder, and one agent run is recorded as several disconnected sessions.
    The sidecar derives a session from W3C trace context, so the agent's own
    instrumentation is what makes a multi-step loop one recording. This is the
    difference between a corpus that supports divergence detection and one that
    only supports payload-level replay.
    """
    from opentelemetry import trace
    from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
    from opentelemetry.sdk.trace import TracerProvider

    trace.set_tracer_provider(TracerProvider())
    HTTPXClientInstrumentor().instrument()


def main() -> int:
    # ANTHROPIC_BASE_URL is what the injection webhook rewrites. The agent has
    # no idea it is being recorded, which is the entire point.
    base_url = os.environ.get("ANTHROPIC_BASE_URL")
    prompt = os.environ.get("AGENT_PROMPT", "What is the refund policy for laptops?")

    if os.environ.get("OTEL_ENABLED", "1") == "1":
        instrument()

    model = ChatAnthropic(
        model=os.environ.get("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
        base_url=base_url,
        api_key=os.environ.get("ANTHROPIC_API_KEY", "sk-ant-fake-key-for-tests"),
        temperature=0.2,
        max_tokens=1024,
        timeout=30,
        max_retries=0,
        # Streaming is the path that matters for perceived latency, so the
        # fixture can exercise it on demand.
        streaming=os.environ.get("AGENT_STREAM", "0") == "1",
    )

    # An MCP endpoint means the tool plane is exercised for real: the agent
    # speaks streamable HTTP to whatever is proxying for it.
    mcp_endpoint = os.environ.get("WAVEOFF_MCP_ENDPOINT")

    from opentelemetry import trace

    tracer = trace.get_tracer("waveoff.fixture")

    if mcp_endpoint:
        import asyncio

        async def run() -> dict:
            # The span opens before the MCP client is built, not after.
            #
            # OpenTelemetry context lives in contextvars, and the MCP SDK does
            # its handshake — initialize, tools/list — on tasks created when
            # the client is constructed. Building the client outside the span
            # leaves that handshake with no trace context, and the recorder
            # correctly files each of those calls as a session of its own.
            # Everything the agent does has to happen inside one span for the
            # recorder to see one session.
            with tracer.start_as_current_span("agent.invoke"):
                tools = await mcp_tools(mcp_endpoint)
                print(f"loaded {len(tools)} tools from {mcp_endpoint}", flush=True)
                agent = create_react_agent(model, tools)
                # MCP-backed tools are async only, so the invocation has to be.
                return await agent.ainvoke({"messages": [HumanMessage(content=prompt)]})

        result = asyncio.run(run())
    else:
        agent = create_react_agent(model, [lookup_refund_policy])
        with tracer.start_as_current_span("agent.invoke"):
            result = agent.invoke({"messages": [HumanMessage(content=prompt)]})

    for message in result["messages"]:
        kind = type(message).__name__
        text = getattr(message, "content", "")
        print(f"{kind}: {text}", flush=True)

    print("AGENT_OK", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
