# Intro video: storyboard

About 61 s at 60 fps. The 16:9 cut is 1920×1080 and the 1:1 cut is 1080×1080, re-laid out rather than letterboxed. There is no voiceover: the on-screen type tells the story, because X autoplays muted.

Every cut lands on an 80 BPM grid, where one beat is 45 frames: holds are long enough to read, while spring motions stay quick. Scene start beats live in `src/scenes.json`, and both the picture and the score read them.

| Beats | Card (on screen) | Picture |
|---|---|---|
| 0–6 | "Everyone's building a sandbox company." | Frame 0 is the poster: the headline alone, dead-center. A few made-up vendor cards drop in one at a time, then more, then about 40 pour in from every edge. Later, bigger cards pile right on top of the headline until it's buried. Each landing nudges the camera, and the pops climb in pitch. |
| 6–10 | "Their cloud." / "Their SDK." | On the downbeat the chaos snaps into a walled grid and the spare cards get flung off-screen. Every card is "hosted only". "Their SDK." punches into one card while every SDK call lights up, each one different. |
| 10–12 | "Their rules." | Padlocks ripple across the grid as the borders thicken. |
| 12–16 | "Self-hosting?" / "Contact sales." | Some cards flip to a black "contact sales" tag, then the grid falls away. |
| 16–20 | "I just want sandboxes on my own servers." | You, your servers and your cluster. |
| 20–27 | "Firecracker." "Kubernetes." "Postgres." "Glue." "More glue." "So. Much. Glue." | One part drops per beat, glue wires tangle into a knot, and the screen shakes. |
| 28–32 | "So we did it…" | Black. The two squares fly in from opposite corners. |
| 32–36 | (logo) | The squares lock together on the downbeat (boom) and the wordmark rises. |
| 36–42 | "One open-source API" / "for sandboxes and microVMs." | The lockup lifts and a small mark sits in the corner. |
| 42–48 | "Ask for a shape." / "And a policy." / "Never a backend." | The real SDK call types out, with `profile` and `placement` highlighted. A `backend: "sandbox.co"` line sneaks in and is struck out. |
| 48–58 | "Bring your own metal." / "Quotas. Audit. Trust report." / "Policy decides where work goes." | The diagram builds: your app, then differently-server (with Postgres), then three private pools filling with sandboxes. Orchestration badges pop above the server and requests flow along the wires. The pools are backend-neutral: the focus is orchestration across pools and providers, not one VM technology. |
| 58–68 | "External providers?" / "Fail closed." / "Open only if policy allows." | A gate with `external: "never"`. A request bounces with `422 no_eligible_pool`. The config flips to `external: "overflow"`, the lock opens and a burst flows to the external pool. |
| 68–70 | — | Hand-off: the server box's light fill grows to cover the screen while its mark glides to the center. |
| 70–74 | "Come build it with us." | The same mark lifts above the line. Pills: pre-release (filled), Apache-2.0, self-hosted, TS · Python · Go. Small print: "not production-ready yet". |
| 74–82 | "Your infrastructure. Your rules. One API." | The mark settles into the end-card lockup and the wordmark rises beside it. Then the tagline, the repo URL and the site URL. The last ~3 s are still. |

## Claims we checked

Sources: the pre-release README, ADR 0006, and a market scan dated 2026-10-28.

- **Pre-release.** Nothing has run on real KVM hardware or live provider accounts yet. The video never says "production-ready", quotes no performance numbers, and never claims Firecracker is unique: Firecracker is table stakes. It doesn't name a future VM foundation either.
- **Vendors are made up.** "Hosted only" and "contact sales" reflect the market scan: of 12 managed sandbox services checked, 7 publish no self-host option and 5 offer it only through enterprise or custom deals.
- **The policy vocabulary is the product's own.** It uses `external: never | overflow | allowed`, and a denied create returns HTTP 422 `no_eligible_pool`. An unset policy means `never`.
- **The SDK snippet matches the README** and `site/src/pages/index.astro`.
