# Profiling Memory and CPU in the Agent Executor

This guide explains how to capture and analyse Go `pprof` profiles from the
`agent-executor` binary — locally against a port-forwarded pod or directly in a
dev cluster.

## Background

The agent executor runs as a long-lived HTTPS server.  Each incoming A2A request
spawns an LLM conversation that may allocate significant memory (conversation
history, streaming buffers, HTTP response bodies).  `pprof` lets you:

- **Heap profiles** — see what is allocated and what is retained (not yet GC'd)
- **CPU profiles** — find hot paths during high-throughput request bursts
- **Goroutine dumps** — detect goroutine leaks from stalled requests

---

## Step 1 — Enable the profiler

The profiler endpoint is **disabled by default** and must be opted-in with a
flag.  It binds only to `localhost` and is never exposed on the TLS port.

### In a Kubernetes deployment

Add the flags to the container args and expose the profiler port:

```yaml
# In the agent-executor Deployment, under spec.template.spec.containers[0]:
args:
  - --profile
  - --profiler-port=6060
ports:
  - name: pprof
    containerPort: 6060
    protocol: TCP
```

> **Never add `--profile` to a production deployment.** The pprof endpoint has
> no authentication and exposes internal memory layout.

### Locally (without Kubernetes)

```bash
./bin/agent-executor --profile --profiler-port=6060 \
  --tls-port=8443 \
  [other flags...]
```

The binary will log:

```
pprof profiler listening on http://localhost:6060/debug/pprof/ (--profile flag is set; disable in production)
```

---

## Step 2 — Access the pprof endpoint

### Local binary

The endpoint is already reachable at `http://localhost:6060/debug/pprof/`.

### In-cluster pod

Port-forward the pprof port from the running pod:

```bash
# Find the agent-executor pod
kubectl -n ottoflow get pods -l app=agent-executor

# Forward the pprof port (replace <pod-name>)
kubectl -n ottoflow port-forward pod/<pod-name> 6060:6060
```

The endpoint is now available at `http://localhost:6060/debug/pprof/`.

---

## Step 3 — Capture profiles

### Heap profile (memory)

A heap profile captures live allocations at a point in time.  Capture it
**while the process is under load** to see what is in use.

```bash
# Capture a heap profile (30-second window)
go tool pprof http://localhost:6060/debug/pprof/heap

# Save to file for later analysis
curl -s http://localhost:6060/debug/pprof/heap -o heap.pprof
go tool pprof heap.pprof
```

To compare two heap snapshots and find what grew between them:

```bash
curl -s http://localhost:6060/debug/pprof/heap -o heap-before.pprof
# ... run workload ...
curl -s http://localhost:6060/debug/pprof/heap -o heap-after.pprof

go tool pprof --base heap-before.pprof heap-after.pprof
```

Useful `pprof` commands inside the interactive shell:

| Command | Description |
|---------|-------------|
| `top20` | Top 20 allocating functions by in-use bytes |
| `top20 -cum` | Same, but including callees (cumulative) |
| `web` | Open a flame graph in the browser (requires Graphviz) |
| `list FuncName` | Annotate source lines for a specific function |
| `svg > heap.svg` | Save a call graph as SVG |

### Goroutine dump (leak detection)

```bash
# Print all goroutines with full stack traces
curl -s "http://localhost:6060/debug/pprof/goroutine?debug=2" | less

# Capture as pprof for interactive analysis
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

Look for large numbers of goroutines blocked in the same function — this
indicates a goroutine leak (e.g., stalled HTTP responses, un-cancelled
contexts, or leaked ticker goroutines).

### CPU profile

CPU profiling samples the call stack at ~100 Hz for the duration you specify.
Run it while sending requests to the agent executor.

```bash
# 30-second CPU profile
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Save to file
curl -s "http://localhost:6060/debug/pprof/profile?seconds=30" -o cpu.pprof
go tool pprof cpu.pprof
```

### All-in-one: continuous memory growth

To reproduce and capture the memory growth pattern observed with large
prompts (e.g., the FinOps cost-analyzer scenario):

```bash
# Terminal 1 — port-forward the pprof port
kubectl -n ottoflow port-forward pod/<agent-executor-pod> 6060:6060

# Terminal 2 — baseline snapshot
curl -s http://localhost:6060/debug/pprof/heap -o heap-baseline.pprof

# Terminal 3 — drive load by running workflows against the cluster
# e.g. ./bin/ottoflow run cost-optimization --workflow-dir samples

# Terminal 2 — post-load snapshot, then diff
curl -s http://localhost:6060/debug/pprof/heap -o heap-loaded.pprof
go tool pprof --base heap-baseline.pprof heap-loaded.pprof
# Inside pprof: top20 -cum
```

---

## Step 4 — Web UI (optional)

`pprof` ships a browser UI that renders flame graphs without requiring
Graphviz:

```bash
go tool pprof -http=:8080 heap.pprof
# Opens http://localhost:8080 automatically
```

The **Flame Graph** view (`View → Flame Graph`) is the fastest way to spot
which call chains hold the most memory or CPU time.

---

## Tips

- Capture **alloc** (total allocations) vs **inuse** (currently live) heap profiles separately.
  Alloc pinpoints where memory is created; inuse pinpoints what is retained.

  ```bash
  # alloc_space — cumulative allocations since process start
  curl -s "http://localhost:6060/debug/pprof/heap?gc=1" -o alloc.pprof
  go tool pprof -alloc_space alloc.pprof

  # inuse_space — memory currently held (default)
  go tool pprof -inuse_space heap.pprof
  ```

- Always trigger a GC before capturing a heap profile to avoid measuring
  unreachable objects that haven't been collected yet:

  ```bash
  curl -s "http://localhost:6060/debug/pprof/heap?gc=1" -o heap.pprof
  ```

- For OOM investigations, capture profiles at **regular intervals** (e.g.,
  every 5 minutes with a cron job or a `watch` loop) so you can see the
  growth rate over time rather than a single snapshot.
