# Upgrading: MCP redirects and message size limits

**Three setups that worked before fail after upgrade: an MCPServer whose server redirects to
another origin, an `http` or `sse` MCPServer that sends a reply larger than 32 MiB, and a
caller that POSTs more than 4 MiB to the workflow-tools `/mcp` endpoint.**

## MCPServer that redirects to another origin

- **Symptom:** the step fails with an error that contains
  `refusing cross-origin redirect: <scheme>://<host> to <scheme>://<host>`.
- **Cause:** MCP clients for `http` and `sse` MCPServers now follow a redirect only to the
  origin (scheme, host and port) of the first request. A redirect from `http://` to `https://`
  on the same host counts as another origin.
- **Fix:** set the MCPServer's address to the URL the server redirects to.

## MCPServer reply larger than 32 MiB

- **Symptom:** the step fails with an error that contains
  `message from the server exceeds the size bound`.
- **Cause:** a single JSON response body or SSE event from an `http` or `sse` MCPServer is now
  limited to 32 MiB. `stdio` MCPServers are not limited.
- **Fix:** make the tool return less, for example by paging its results.

## POST to `/mcp` larger than 4 MiB

- **Symptom:** callers of the workflow-tools endpoint (`controller.mcp.enabled`) get
  `413 Request Entity Too Large` with a JSON-RPC error `request body exceeds 4194304 bytes`.
- **Cause:** the endpoint now limits a request body to 4 MiB.
- **Fix:** send smaller tool arguments.
