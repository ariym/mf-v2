# Custom Go flows

For downloaded, standalone Go flows from mf-hub, see [hub integration](hub.md). They execute through a generic subprocess activity and need no rebuild or runtime Go plugin loading. The instructions below remain the extension point for workflows compiled into `mf` itself.

`flows.Registry` registers compiled workflows and activities with the embedded `go-workflows` worker. See the compilable two-activity example in `examples/flows/flows.go`.

To enable it, import the example and server packages in `cmd/mf/main.go`:

```go
import (
    example "github.com/ariym/mf-v2/examples/flows"
    "github.com/ariym/mf-v2/internal/server"
)
```

At the beginning of `main`, before calling the CLI, assign:

```go
server.RegisterCustomFlows = example.Register
```

Rebuild the executable, restart with `mf daemon start` (or `mf server start`), then submit this body to `POST /api/v1/jobs`:

```json
{"flow":"greet","input":{"name":"Ada"}}
```

A flow has signature `func(workflow.Context, json.RawMessage) (any, error)`. Register it with a stable name and an optional `Prepare` function that validates/normalizes API input before durable submission. Activities are named functions accepting `context.Context` first, with JSON-serializable arguments and results.

```go
func Greeting(ctx workflow.Context, input json.RawMessage) (any, error) {
    var in Input
    if err := json.Unmarshal(input, &in); err != nil {
        return nil, err
    }
    name, err := workflow.ExecuteActivity[string](
        ctx, flows.ActivityOptions(), "example_normalize", in.Name,
    ).Get(ctx)
    if err != nil {
        return nil, err
    }
    return workflow.ExecuteActivity[string](
        ctx, flows.ActivityOptions(), "example_greet", name,
    ).Get(ctx)
}
```

Keep filesystem access, subprocesses, database operations, network calls, random values, and ordinary wall-clock reads in activities. Workflow code is replayed and must remain deterministic; use engine workflow primitives for timers and concurrency, and do not iterate Go maps to decide execution order.

`flows.ActivityOptions()` configures three total attempts with bounded exponential retry delays. Return `workflow.NewPermanentError(err)` for failures that retrying cannot fix. Activities must be idempotent: a process can stop after an external side effect but before its checkpoint is saved. The built-in import activity uses canonical-path upserts for this reason.

Registered activity contexts are canceled during server shutdown. Use context-aware I/O and `exec.CommandContext`; do not swallow cancellation or start unmanaged background work. Forced shutdown deliberately leaves interrupted activities uncheckpointed so they can execute again after restart. The worker and HTTP server have a bounded shutdown window.

Workflow and activity names and argument shapes form a persisted contract. Do not rename or incompatibly change them while unfinished instances exist. Finish old runs or retain their registered implementations when deploying a new binary. There is no runtime Go plugin loader or automated workflow-code migration. Downloaded flows retain exact executable/dependency snapshots for unfinished jobs.
