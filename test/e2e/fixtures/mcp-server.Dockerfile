# A real MCP server for the `waveoff pin` end-to-end case.
#
# Built here rather than pulled from mcp/everything on Docker Hub, because that
# image is pinned to an old release that predates the streamable HTTP transport
# and ships only stdio and the deprecated HTTP+SSE one. The point of this test
# is to check pin against the protocol as it is actually spoken, so the server
# has to be current.
FROM node:22-alpine

ARG SERVER_VERSION=2026.8.18
RUN npm install -g "@modelcontextprotocol/server-everything@${SERVER_VERSION}"

EXPOSE 3001
CMD ["mcp-server-everything", "streamableHttp"]
