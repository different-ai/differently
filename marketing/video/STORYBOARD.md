# Intro video: storyboard

About 41 s at 60 fps. The pitch: Differently is a sandbox router. The 16:9 cut is 1920×1080 and the 1:1 cut is 1080×1080, re-laid out rather than letterboxed. There is no voiceover yet: the on-screen type tells the story, because X autoplays muted.

Every cut lands on a 110 BPM grid, where one beat is about 33 frames. Scene start beats live in `src/scenes.json`, and both the picture and the score read them.

| Beats | Card (on screen) | Picture |
|---|---|---|
| 0–6 | "Everyone's building a sandbox company." | Frame 0 is the poster: the headline alone, dead-center. A few made-up vendor cards drop in one at a time, then about 40 pour in from every edge. Later, bigger cards pile onto the headline until it's buried. |
| 6–12 | "Their cloud." / "Their SDK." / "Their rules." | The pile snaps into a walled grid and the spare cards get flung off-screen. Every card is "hosted only". There's a punch-in on one SDK call, then padlocks ripple across the grid. |
| 12–16 | "Self-hosting?" / "Contact sales." | Some cards flip to a black "contact sales" tag. Then a whip pan with horizontal motion blur. |
| 16–27 | "I just want sandboxes on my own servers." … "So. Much. Glue." | The whip lands on you, your servers and your cluster. Firecracker, Kubernetes and Postgres drop in one per beat as glue wires tangle into a knot. The whole scene gets sucked into the knot and a black iris opens from it. |
| 27–37 | "So we did it…" / (logo) / "Not another sandbox." / "A sandbox router." | The two squares fly in and lock on the downbeat, and the wordmark rises. |
| 37–42 | "One API in." / "Never a backend." | The real SDK call types out with `profile` and `placement` highlighted. A `backend: "sandbox.co"` line sneaks in and is struck out. |
| 42–57 | "Sensitive work stays home." / "Quotas per team." / "Full? It fails closed." / "Burst out only if policy allows." / "Every route, audited." | Requests flow from your app through differently-server into your private pools (your microVMs on Firecracker, and OpenSandbox self-hosted) until both hit quota. Daytona and Freestyle sit behind a gate with `external: "never"`, so a request bounces with `422 no_eligible_pool`. The config flips to `"overflow"`, the lock opens and work lands outside. An audit log tails three placements, and the quotas, audit and trust report badges pop. |
| 55–57 | — | Hand-off: the server box's light fill grows to cover the screen while its mark glides to the center. |
| 57–64 | "OpenSandbox — for self-hosting — 64 ms P50 sandbox creation*" / "Freestyle · Daytona · … — for more capacity or non-sensitive work" | The mark is the router: two wires branch from it to each destination, with packets riding them. The 64 ms counts up. The light-gray footnote reads "* As reported by OpenSandbox for its Fast Sandbox runtime." |
| 64–75 | "Come build it with us." → "Your infrastructure. Your rules. One API." | Pills: pre-release, Apache-2.0, self-hosted, TS · Python · Go, plus "not production-ready yet". The mark settles into the end-card lockup, then come the tagline and the URLs. The last ~3 s are still. |

## Claims we checked

Sources: the pre-release README, ADR 0006, PRs 3–5, the OpenSandbox README, and a market scan dated 2026-10-28.

- **Pre-release.** Differently hasn't run on bare-metal or cloud KVM or on live Daytona or Freestyle accounts. The e2e suite runs on arm64 under nested virtualization. The video says "pre-release" and "not production-ready yet" and quotes no Differently performance numbers.
- **The 64 ms is OpenSandbox's claim, footnoted as theirs.** Their README lists "Firecracker creation: 64 ms P50 (serial)" for the Fast Sandbox runtime on Kubernetes, with no hardware or method stated. Our OpenSandbox adapter (PR 5) is verified against a live server on the Docker runtime only, not Fast Sandbox. Any Differently p50 has to come from our own benchmark first.
- **The burst column ends in "…" rather than naming Boat.** Boat has no adapter (see ADR 0013), so it isn't shown as a destination.
- **Real names are used only for adapters that exist.** Firecracker, OpenSandbox, Daytona and Freestyle are all backends in the code. The opening vendors are made up. "Hosted only" and "contact sales" reflect the market scan: of 12 managed sandbox services checked, 7 publish no self-host option and 5 offer it only through enterprise or custom deals.
- **The policy vocabulary is the product's own.** It uses `external: never | overflow | allowed`, a denied create returns HTTP 422 `no_eligible_pool`, and an unset policy means `never`. Registering Daytona or Freestyle authorizes nothing.
- **The SDK snippet matches the README** and the site.
