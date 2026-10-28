# Intro video: storyboard

41 s at 60 fps. The 16:9 cut is 1920×1080 and the 1:1 cut is 1080×1080, re-laid out rather than letterboxed. There is no voiceover: the on-screen type tells the story, because X autoplays muted.

Every cut lands on a 120 BPM grid, where one beat is 30 frames. Scene start beats live in `src/scenes.json`, and both the picture and the score read them.

| Beats | Card (on screen) | Picture |
|---|---|---|
| 0–4 | "Everyone's building a sandbox company." | Frame 0 is the poster: the headline alone, dead-center. Made-up vendor cards fly in from every edge, faster and faster (40 in under two seconds). Early ones ring the headline and later, bigger ones pile right on top of it until it's buried. Each landing nudges the camera, and the pops climb in pitch. |
| 4–8 | "Their cloud." / "Their SDK." | On the downbeat the chaos snaps into a walled grid and the spare cards get flung off-screen. Every card is "hosted only". "Their SDK." punches into one card while every SDK call lights up, each one different. |
| 8–10 | "Their rules." | Padlocks ripple across the grid as the borders thicken. |
| 10–14 | "Self-hosting?" / "Contact sales." | Some cards flip to a black "contact sales" tag, then the grid falls away. |
| 14–18 | "I just want sandboxes on my own servers." | You, your servers and your cluster. |
| 18–25 | "Firecracker." "Kubernetes." "Postgres." "Glue." "More glue." "So. Much. Glue." | One part drops per beat, glue wires tangle into a knot, and the screen shakes. |
| 26–30 | "So we did it…" | Black. The two squares fly in from opposite corners. |
| 30–34 | (logo) | The squares lock together on the downbeat (boom) and the wordmark rises. |
| 34–40 | "One open-source API" / "for sandboxes and microVMs." | The lockup lifts and a small mark sits in the corner. |
| 40–46 | "Ask for a shape." / "And a policy." / "Never a backend." | The real SDK call types out, with `profile` and `placement` highlighted. A `backend: "sandbox.co"` line sneaks in and is struck out. |
| 46–56 | "Bring your own metal." / "Quotas. Audit. Trust report." / "Policy decides where work goes." | The diagram builds: your app, then differently-server (with Postgres), then three private pools filling with sandboxes. Orchestration badges pop above the server and requests flow along the wires. The pools are backend-neutral: the focus is orchestration across pools and providers, not one VM technology. |
| 56–66 | "External providers?" / "Fail closed." / "Open only if policy allows." | A gate with `external: "never"`. A request bounces with `422 no_eligible_pool`. The config flips to `external: "overflow"`, the lock opens and a burst flows to the external pool. |
| 66–68 | — | Zoom through the open square of the server's mark into white. |
| 68–72 | "Come build it with us." | Pills: pre-release (filled), Apache-2.0, self-hosted, TS · Python · Go. Small print: "not production-ready yet". |
| 72–82 | "Your infrastructure. Your rules. One API." | End card: mark + wordmark, the tagline, then the repo and site URLs. The last ~3 s are still. |

## Claims we checked

Sources: the pre-release README, ADR 0006, and a market scan dated 2026-10-28.

- **Pre-release.** Nothing has run on real KVM hardware or live provider accounts yet. The video never says "production-ready", quotes no performance numbers, and never claims Firecracker is unique: Firecracker is table stakes. It doesn't name a future VM foundation either.
- **Vendors are made up.** "Hosted only" and "contact sales" reflect the market scan: of 12 managed sandbox services checked, 7 publish no self-host option and 5 offer it only through enterprise or custom deals.
- **The policy vocabulary is the product's own.** It uses `external: never | overflow | allowed`, and a denied create returns HTTP 422 `no_eligible_pool`. An unset policy means `never`.
- **The SDK snippet matches the README** and `site/src/pages/index.astro`.
