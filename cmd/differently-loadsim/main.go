//go:build js && wasm

// Command differently-loadsim is internal/loadsim for the browser: the
// website's /simulator/ page loads it as WebAssembly in a worker
// (site/public/sim/worker.js) and draws the snapshots it posts.
//
//	GOOS=js GOARCH=wasm go build -o differently.wasm ./cmd/differently-loadsim
//
// The worker sets differentlySimConfig (JSON, a loadsim.Config with "load"
// and "routing" objects) before the program starts and defines
// differentlySimEmit(kind, json). The program answers with
// differentlySimSend(json), which takes
// {"op": "load" | "routing" | "add" | "quota" | "audit" | "verify" | "spike" | "outage" | "snapshot", ...}. Commands
// only queue: a JavaScript callback must never block on Go's timers.
package main

import (
	"context"
	"encoding/json"
	"syscall/js"

	"github.com/different-ai/differently/internal/loadsim"
)

type command struct {
	Op       string           `json:"op"`
	Load     loadsim.Load     `json:"load"`
	Routing  loadsim.Routing  `json:"routing"`
	Share    float64          `json:"share"`
	Pool     string           `json:"pool"`
	Provider loadsim.Provider `json:"provider"`
	Outage   bool             `json:"outage"`
	Quota    loadsim.Quota    `json:"quota"`
	All      bool             `json:"all"`
}

func main() {
	global := js.Global()
	emit := func(kind string, v any) {
		b, _ := json.Marshal(v)
		global.Call("differentlySimEmit", kind, string(b))
	}
	var start struct {
		loadsim.Config
		Load    loadsim.Load    `json:"load"`
		Routing loadsim.Routing `json:"routing"`
		Quota   loadsim.Quota   `json:"quota"`
	}
	if err := json.Unmarshal([]byte(global.Get("differentlySimConfig").String()), &start); err != nil {
		emit("error", err.Error())
		return
	}
	world, err := loadsim.New(start.Config)
	if err != nil {
		emit("error", err.Error())
		return
	}
	if err := world.SetRouting(start.Routing); err != nil {
		emit("error", err.Error())
		return
	}
	if err := world.SetQuota(start.Quota); err != nil {
		emit("error", err.Error())
		return
	}
	world.Start(context.Background())
	world.SetLoad(start.Load)

	commands := make(chan command, 64)
	global.Set("differentlySimSend", js.FuncOf(func(_ js.Value, args []js.Value) any {
		var c command
		if len(args) == 1 && json.Unmarshal([]byte(args[0].String()), &c) == nil {
			select {
			case commands <- c:
			default: // the page asks again
			}
		}
		return nil
	}))
	emit("ready", true)
	for c := range commands {
		switch c.Op {
		case "load":
			world.SetLoad(c.Load)
		case "spike":
			world.Spike(c.Share)
		case "routing":
			if err := world.SetRouting(c.Routing); err != nil {
				emit("error", err.Error())
			}
		case "add":
			if err := world.AddProvider(c.Provider, c.Routing); err != nil {
				emit("error", err.Error())
			}
		case "quota":
			if err := world.SetQuota(c.Quota); err != nil {
				emit("error", err.Error())
			}
		case "audit":
			world.SetAuditAll(c.All)
		case "verify":
			emit("verify", world.VerifyAudit())
		case "outage":
			world.SetOutage(c.Pool, c.Outage)
		case "snapshot":
			emit("snapshot", world.Snapshot())
		}
	}
}
