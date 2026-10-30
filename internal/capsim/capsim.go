// Package capsim simulates one pool's slots under the scheduler's capacity
// strategies, to put numbers on them. It is a model, not the scheduler: each
// user owns one long-lived computer (as OpenWork's Workbot does) that is
// created at the user's first session, runs while the user works, pauses
// after an idle timeout and resumes at the next session. Pauses take time to
// write their checkpoint; the slot is free only once they finish.
//
// Strategies:
//
//   - hold: a paused computer keeps its slot (what Differently did before),
//     so the pool holds at most Slots computers and later users' computers are
//     created on the external overflow pool.
//   - release: a paused computer on a checkpointing pool gives its slot
//     back; a resume on a full pool waits in a queue.
//   - preempt-idle: release, plus a running computer idle for PreemptIdle (and
//     not in a session) is paused early to make room for waiting work.
//   - preempt-lru: release, plus the least recently used running computer,
//     busy or not, is paused to make room (never one that ran less than
//     MinRun).
//
// A create that finds no free slot overflows to the external pool, which has
// no limit; a resume stays on its pool, where its memory is.
package capsim

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Strategy is a capacity strategy.
type Strategy string

// Strategies.
const (
	Hold        Strategy = "hold"
	Release     Strategy = "release"
	PreemptIdle Strategy = "preempt-idle"
	PreemptLRU  Strategy = "preempt-lru"
)

// Config describes one simulated pool and its users. Times are in seconds.
type Config struct {
	Strategy Strategy
	Slots    int
	Users    int
	Days     int
	// Sessions a user starts per working day (Poisson), in an 8-hour day.
	SessionsPerDay float64
	// Session length: log-normal with this median and sigma.
	SessionMedian float64
	SessionSigma  float64
	IdlePause     int // pause a computer idle this long
	PauseTime     int // how long a pause takes to free the slot
	ResumeTime    int // how long a resume takes once it holds a slot
	MinRun        int // preemption never picks a computer that ran less
	PreemptIdle   int // preempt-idle picks computers idle this long
	Seed          uint64
}

// Workbot returns OpenWork Workbot's settings on the Azure Fast Sandbox pool:
// a 5-minute idle pause, a checkpoint pause of 182 s (the median for an
// in-use desktop with sparse templates, docs/research/fast-sandbox-pause-time.md)
// and a resume of about 2 s.
func Workbot(s Strategy, slots, users int) Config {
	return Config{
		Strategy: s, Slots: slots, Users: users, Days: 5,
		SessionsPerDay: 6, SessionMedian: 12 * 60, SessionSigma: 0.9,
		IdlePause: 300, PauseTime: 182, ResumeTime: 2, MinRun: 600, PreemptIdle: 60, Seed: 1,
	}
}

// Result is what one run measured.
type Result struct {
	Config Config
	// PrivateUsers is the share of users whose computer is on the pool.
	PrivateUsers float64
	// ExternalHours is computer-hours run on the overflow pool.
	ExternalHours float64
	// Utilization is busy slot time over all slot time, on the pool.
	Utilization float64
	// Wait percentiles: seconds from a session's start until its computer
	// runs, over every session (create, resume, or already running).
	WaitP50, WaitP95, WaitMax float64
	// Preemptions per day, and how many paused a computer during a session.
	PreemptionsPerDay, InterruptionsPerDay float64
}

type state int

const (
	none state = iota
	running
	pausing
	paused
	resuming
)

type computer struct {
	external     bool
	state        state
	busy         bool
	lastActive   int
	runningSince int
	until        int // end of pausing or resuming
	queuedAt     int // resume waiting for a slot; -1 when not
	waitFrom     int // session waiting for this computer; -1 when not
}

type session struct{ start, end, user int }

// Run simulates cfg.
func Run(cfg Config) Result {
	rng := rand.New(rand.NewPCG(cfg.Seed, 7))
	horizon := cfg.Days * 86400
	var sessions []session
	for u := 0; u < cfg.Users; u++ {
		for d := 0; d < cfg.Days; d++ {
			day := d*86400 + 9*3600
			n := poisson(rng, cfg.SessionsPerDay)
			for i := 0; i < n; i++ {
				start := day + rng.IntN(8*3600)
				length := int(cfg.SessionMedian * math.Exp(cfg.SessionSigma*rng.NormFloat64()))
				sessions = append(sessions, session{start, start + max(length, 30), u})
			}
		}
	}
	starts := map[int][]session{}
	ends := map[int][]int{}
	for _, s := range sessions {
		starts[s.start] = append(starts[s.start], s)
		ends[s.end] = append(ends[s.end], s.user)
	}
	cs := make([]computer, cfg.Users)
	for i := range cs {
		cs[i].queuedAt, cs[i].waitFrom = -1, -1
	}
	holds := func(c *computer) bool {
		if c.external || c.state == none {
			return false
		}
		if c.state == paused && cfg.Strategy != Hold {
			return false
		}
		return true
	}
	held := func() int {
		n := 0
		for i := range cs {
			if holds(&cs[i]) {
				n++
			}
		}
		return n
	}
	var waits []float64
	var busySlotSeconds, externalSeconds float64
	preemptions, interruptions := 0, 0
	var queue []int
	privateUsers := 0

	resume := func(u, t int) {
		c := &cs[u]
		c.state, c.until, c.queuedAt = resuming, t+cfg.ResumeTime, -1
	}
	for t := 0; t < horizon; t++ {
		// Finish pauses and resumes.
		for u := range cs {
			c := &cs[u]
			switch {
			case c.state == pausing && t >= c.until:
				c.state = paused
				if c.waitFrom >= 0 {
					c.queuedAt = t // the session came back during the pause
				}
			case c.state == resuming && t >= c.until:
				c.state, c.runningSince = running, t
				if c.waitFrom >= 0 {
					waits = append(waits, float64(t-c.waitFrom))
					c.waitFrom = -1
				}
			}
		}
		// Sessions end and start.
		for _, u := range ends[t] {
			cs[u].busy, cs[u].lastActive = false, t
		}
		for _, s := range starts[t] {
			c := &cs[s.user]
			if c.busy {
				continue // overlapping sessions of one user share the computer
			}
			c.busy, c.lastActive = true, t
			switch c.state {
			case none:
				c.external = held() >= cfg.Slots
				if !c.external {
					privateUsers++
				}
				c.state, c.until, c.waitFrom = resuming, t+cfg.ResumeTime, t
			case running, resuming:
				if c.state == running {
					waits = append(waits, 0)
				} else if c.waitFrom < 0 {
					c.waitFrom = t
				}
			case pausing:
				c.waitFrom = t
			case paused:
				c.waitFrom = t
				c.queuedAt = t
			}
		}
		// Queue resumes; serve in order while slots are free.
		for u := range cs {
			c := &cs[u]
			if c.state == paused && c.queuedAt >= 0 && !contains(queue, u) {
				if c.external || cfg.Strategy == Hold {
					resume(u, t)
					continue
				}
				queue = append(queue, u)
			}
		}
		for len(queue) > 0 && held() < cfg.Slots {
			resume(queue[0], t)
			queue = queue[1:]
		}
		// Idle pauses.
		for u := range cs {
			c := &cs[u]
			if c.state == running && !c.busy && t-c.lastActive >= cfg.IdlePause {
				c.state, c.until = pausing, t+cfg.PauseTime
			}
		}
		// Preemption: one victim per waiting resume, counting pauses under way.
		if (cfg.Strategy == PreemptIdle || cfg.Strategy == PreemptLRU) && len(queue) > 0 {
			inflight := 0
			for u := range cs {
				if !cs[u].external && cs[u].state == pausing {
					inflight++
				}
			}
			for need := len(queue) - inflight; need > 0; need-- {
				v := -1
				for u := range cs {
					c := &cs[u]
					if c.external || c.state != running {
						continue
					}
					if cfg.Strategy == PreemptIdle && (c.busy || t-c.lastActive < cfg.PreemptIdle) {
						continue
					}
					if cfg.Strategy == PreemptLRU && t-c.runningSince < cfg.MinRun {
						continue
					}
					if v < 0 || c.lastActive < cs[v].lastActive {
						v = u
					}
				}
				if v < 0 {
					break
				}
				preemptions++
				if cs[v].busy {
					interruptions++
					cs[v].waitFrom = t
				}
				cs[v].state, cs[v].until = pausing, t+cfg.PauseTime
			}
		}
		// Accounting.
		for u := range cs {
			c := &cs[u]
			if c.state == none || c.state == paused {
				continue
			}
			if c.external {
				externalSeconds++
			} else if c.busy {
				busySlotSeconds++
			}
		}
	}
	sort.Float64s(waits)
	days := float64(cfg.Days)
	return Result{
		Config:              cfg,
		PrivateUsers:        float64(privateUsers) / float64(cfg.Users),
		ExternalHours:       externalSeconds / 3600,
		Utilization:         busySlotSeconds / float64(cfg.Slots*horizon),
		WaitP50:             pct(waits, 0.5),
		WaitP95:             pct(waits, 0.95),
		WaitMax:             pct(waits, 1),
		PreemptionsPerDay:   float64(preemptions) / days,
		InterruptionsPerDay: float64(interruptions) / days,
	}
}

func poisson(rng *rand.Rand, lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Average runs cfg with seeds 1..n and averages the results.
func Average(cfg Config, n int) Result {
	var sum Result
	for seed := 1; seed <= n; seed++ {
		cfg.Seed = uint64(seed)
		r := Run(cfg)
		sum.PrivateUsers += r.PrivateUsers
		sum.ExternalHours += r.ExternalHours
		sum.Utilization += r.Utilization
		sum.WaitP50 += r.WaitP50
		sum.WaitP95 += r.WaitP95
		sum.WaitMax = max(sum.WaitMax, r.WaitMax)
		sum.PreemptionsPerDay += r.PreemptionsPerDay
		sum.InterruptionsPerDay += r.InterruptionsPerDay
	}
	k := float64(n)
	cfg.Seed = 0
	return Result{
		Config: cfg, PrivateUsers: sum.PrivateUsers / k, ExternalHours: sum.ExternalHours / k, Utilization: sum.Utilization / k,
		WaitP50: sum.WaitP50 / k, WaitP95: sum.WaitP95 / k, WaitMax: sum.WaitMax,
		PreemptionsPerDay: sum.PreemptionsPerDay / k, InterruptionsPerDay: sum.InterruptionsPerDay / k,
	}
}
