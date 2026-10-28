---
title: Go SDK
description: The Go client in pkg/client, used by the CLI itself.
---

The Go SDK lives in [`pkg/client`](https://github.com/different-ai/differently/tree/pre-release/pkg/client) with request and response types in [`pkg/api`](https://github.com/different-ai/differently/tree/pre-release/pkg/api). The `differently` CLI is built on it, so it covers the whole API. Like the other SDKs it never names a backend: placement comes from server policy unless a request narrows it.

```sh
go get github.com/different-ai/differently/pkg/client
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

func main() {
	ctx := context.Background()
	c := client.New("http://localhost:8080", os.Getenv("DIFFERENTLY_API_KEY"))

	// true waits until the sandbox is running.
	sb, err := c.CreateSandbox(ctx, api.CreateSandboxRequest{Profile: "small"}, true)
	if err != nil {
		panic(err)
	}
	defer c.DeleteSandbox(ctx, sb.ID)

	res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: "uname -a"})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Stdout)
}
```

## Methods

| Method | Does |
|---|---|
| `CreateSandbox(ctx, req, wait)` | Create a sandbox, optionally waiting until it runs |
| `WaitSandbox(ctx, id, status, timeout)` | Wait for a status |
| `GetSandbox`, `ListSandboxes`, `DeleteSandbox` | Read and delete |
| `Exec(ctx, id, req)` | Run a command and collect its output |
| `ExecStream(ctx, id, req, fn)` | Stream stdout, stderr and exit events to `fn`; returns the exit code |
| `ReadFile`, `WriteFile`, `ListFiles` | File transfer |
| `Action(ctx, id, action)` | `stop`, `start`, `pause` or `resume` |
| `Snapshot`, `Fork`, `ExposePort` | Lifecycle extras, where the placed backend supports them |
| `Placement(ctx, id)` | Why the sandbox runs where it does |
| `CreateAPIKey(ctx, req)` | Mint a scoped key |
| `Do(ctx, method, path, in, out)` | Any other endpoint in the [HTTP API](/differently/docs/reference/api/) |

`Client.Retries` (default 3) controls how often idempotent requests are retried on `429` and `503`. Errors from the server carry the API's `code`, so branch on the code rather than the message.

The package docs on [pkg.go.dev](https://pkg.go.dev/github.com/different-ai/differently/pkg/client) will list the full surface once the module is tagged.
