# Upgrading: MCP protocol negotiation and the `/mcp` Host check

**Four setups that worked before fail after upgrade: a proxy or sidecar that forwards to the
workflow-tools `/mcp` endpoint over loopback and passes the outside Host through, an MCPServer
whose server answers the protocol probe but is restricted to older protocol versions, an
MCPServer whose server answers the probe successfully but lists only older versions, and an
MCPServer with a short `spec.timeout` whose server ignores the probe.**

## `/mcp` behind a proxy that forwards over loopback

- **Symptom:** callers of the workflow-tools endpoint (`controller.mcp.enabled`) get
  `403 Forbidden: invalid Host header "<host>"`.
- **Cause:** a request that reaches the server over `127.0.0.1` or `::1` must carry a Host of
  `localhost` or a loopback IP (optionally with a port). Any other Host is refused, as a guard
  against DNS rebinding. Requests sent to the pod IP or the Service are unaffected.
- **Fix:** make the proxy or sidecar rewrite Host to `localhost`.
  Callers using `kubectl port-forward` address the server as `localhost:<port>`.
  No setting turns the check off.

## MCPServer whose server answers the probe but is restricted to older protocol versions

- **Symptom:** the step fails with an error like
  `initializing MCP client: unsupported protocol version: "2026-07-28" (supported: [2025-11-25 2025-06-18 2025-03-26])`.
  The bracketed list is the versions that server allows.
- **Cause:** OttoFlow opens each connection with a `server/discover` probe on protocol
  `2026-07-28`. Such a server answers it with an "unsupported protocol version" error listing
  the versions it accepts (error code -32022), and OttoFlow then fails instead of retrying with
  the earlier handshake. Not every probe error ends this way: an error such as "method not found"
  or an HTTP 400, 404 or 500 without that error marks the server as older, and OttoFlow falls
  back to the earlier handshake as before.
- **Fix:** allow `2026-07-28` on the server, or remove its version restriction.
- **Workaround:** to keep the restriction on an `http` or `sse` MCPServer, set the header
  `Mcp-Protocol-Version` under `spec.transport.headers` to a protocol version older than
  `2026-07-28` that the server accepts (for example `2025-11-25`). The header is sent on every
  request, so OttoFlow then talks to that server on the earlier handshake. It does not apply to
  stdio MCPServers or to `spec.auth.type: oauth2`, where headers are not sent. It was checked
  against mcp-go servers only.

## MCPServer whose server answers the probe successfully but lists only older versions

- **Symptom:** the step fails with
  `initializing MCP client: server/discover result lists no modern version this client supports: [2025-11-25 2025-06-18]`.
  The bracketed list is the versions that server reports.
- **Cause:** the server implements `server/discover` and replies with a success result, but
  its `supportedVersions` holds only versions older than `2026-07-28`. OttoFlow treats any
  successful `server/discover` reply as coming from a modern server, so it does not fall back
  to the earlier handshake. The `Mcp-Protocol-Version` header workaround above does not help:
  the step fails the same way with the header set to `2025-11-25`. An mcp-go server does not
  produce this reply; it comes from a server that implements `server/discover` but advertises
  only older versions. This was seen with a synthetic test server.
- **Fix:** make the server list `2026-07-28` in its `server/discover` reply, or stop it from
  answering `server/discover`, so OttoFlow falls back to the earlier handshake. There is no
  OttoFlow-side workaround.

## MCPServer with `spec.timeout` of 5s or less whose server never answers the probe

- **Symptom:** the step fails with `initializing MCP client: transport error: context deadline exceeded`.
- **Cause:** OttoFlow waits up to 5s for the `server/discover` probe on each new connection
  before it falls back to the earlier handshake. A `spec.timeout` at or below that leaves no
  time to fall back.
- **Fix:** set `spec.timeout` to more than 5s plus the server's normal connect time, or
  leave it unset (the default is 90s).
