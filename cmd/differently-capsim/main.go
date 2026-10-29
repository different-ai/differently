// Command differently-capsim prints how a pool's capacity strategies compare
// for Workbot-like users: see internal/capsim.
//
//	go run ./cmd/differently-capsim -slots 4 -users 4,6,8,12,16,24
package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/different-ai/differently/internal/capsim"
)

func main() {
	slots := flag.Int("slots", 4, "pool slots (maxConcurrency)")
	users := flag.String("users", "4,6,8,12,16,24", "comma-separated user counts")
	days := flag.Int("days", 5, "days to simulate")
	seeds := flag.Int("seeds", 5, "runs per row, averaged")
	flag.Parse()
	fmt.Println("| users | strategy | users on the pool | external computer-hours | slot utilization | wait p50 | wait p95 | preemptions/day | interrupted sessions/day |")
	fmt.Println("|---:|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, u := range strings.Split(*users, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(u))
		if err != nil {
			panic(err)
		}
		for _, s := range []capsim.Strategy{capsim.Hold, capsim.Release, capsim.PreemptIdle, capsim.PreemptLRU} {
			cfg := capsim.Workbot(s, *slots, n)
			cfg.Days = *days
			r := capsim.Average(cfg, *seeds)
			fmt.Printf("| %d | %s | %.0f%% | %.0f | %.0f%% | %.0f s | %.0f s | %.1f | %.1f |\n", n, s,
				100*r.PrivateUsers, r.ExternalHours, 100*r.Utilization, r.WaitP50, r.WaitP95, r.PreemptionsPerDay, r.InterruptionsPerDay)
		}
	}
}
