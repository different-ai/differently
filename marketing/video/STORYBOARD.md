# Intro video: storyboard

About 35 s at 60 fps. The pitch: Differently is the layer above sandboxes, routing each one by policy. The video shows this rather than naming it. The 16:9 cut is 1920×1080 and the 1:1 cut is 1080×1080, re-laid out rather than letterboxed. There is no voiceover yet: the on-screen type tells the story, because X autoplays muted.

Every cut lands on a 110 BPM grid, where one beat is about 33 frames. Scene start beats live in `src/scenes.json`, and both the picture and the score read them.

| Beats | Card (on screen) | Picture |
|---|---|---|
| 0–6 | "Everyone's building a sandbox company." | Frame 0 is the poster: the headline, plus a small "differently · open source" tag in the corner. A few "Sandbox Co. N" cards drop in, then about 40 pour in and bury the headline. The cards are obviously fictional: the joke is that they're all the same company. |
| 6–13 | "Their cloud." / "Their SDK." / "Their rules." / "Self-host? 'Contact sales.'" | The pile snaps into a walled grid, every card "hosted only". There's a punch-in on one SDK call, then padlocks ripple and some cards flip to "contact sales". A whip pan with motion blur takes us out. |
| 13–21.5 | "I just want sandboxes on my own servers." … "So. Much. Glue." | Firecracker, Kubernetes and Postgres drop in, and glue wires knot. The scene gets sucked into the knot and a black iris opens from it. |
| 21.5–30 | "So we did it…" / (logo) / "Not another sandbox." / "The layer above them." | The squares lock on a medium hit, with pad and bass underneath (no dip). The full groove and the biggest hit land on "The layer above them." |
| 30–35 | "One API in." / "Write a policy, not a provider." | A 4-line `sandboxes.create` call at 46 px with `placement: { external: "never" }` highlighted. A `backend: "sandbox.co"` line sneaks in and is struck out. |
| 35–50 | "Sensitive work stays home." / "Quotas per team." / "Full? It fails closed." / "Burst out only if policy allows." / "Every placement, audited." | Private pools (your microVMs, OpenSandbox) fill to quota. The camera punches in on the `422 no_eligible_pool` bounce, then on the `"never"` → `"overflow"` flip as work lands in Daytona and Freestyle, then on the audit log. A dark band keeps the headline readable while zoomed. Badges and labels fade before the hand-off. |
| 48–50 | — | Hand-off: the server box fills the screen while its mark glides to the center. |
| 50–56 | three destinations | The mark is the router, branching to your microVMs (Firecracker on your own metal), OpenSandbox for self-hosting (64 ms P50 creation*), and Freestyle · Daytona · … for more capacity or non-sensitive work (`external: "overflow"`). The footnote reads "* As reported by OpenSandbox for its Fast Sandbox runtime." |
| 56–63.5 | "Your infrastructure. Your rules. One API." | The end card: the mark settles into the lockup, then the tagline, the pills (pre-release, open source, Apache-2.0), "★ github.com/different-ai/differently" and the site URL. It holds still for about 2.3 s. |

## Claims we checked

Sources: the pre-release README, ADR 0006, ADR 0013, PRs 3–5, the OpenSandbox README, the SDK types, and a market scan dated 2026-10-28.

- **Pre-release.** Differently hasn't run on bare-metal or cloud KVM or on live Daytona or Freestyle accounts. The e2e suite runs on arm64 under nested virtualization. The end card carries a "pre-release" pill, and the video quotes no Differently performance numbers.
- **The 64 ms is OpenSandbox's claim, footnoted as theirs.** Their README lists "Firecracker creation: 64 ms P50 (serial)" for the Fast Sandbox runtime on Kubernetes, with no hardware or method stated. Our adapter (PR 5) is verified only on their Docker runtime. On screen it stays smaller than the titles, and any Differently p50 has to come from our own benchmark first.
- **Real names are used only for backends that exist in code:** Firecracker, OpenSandbox, Daytona and Freestyle. Boat has no adapter (ADR 0013), so the burst column ends in "…". The opening vendors are "Sandbox Co. N", which can't be mistaken for real companies.
- **The policy vocabulary is the product's own.** It uses `external: never | overflow | allowed`, and a denied create returns HTTP 422 `no_eligible_pool`. A request's `placement.external` can only narrow what operator policy allows; `strategy` is optional.
