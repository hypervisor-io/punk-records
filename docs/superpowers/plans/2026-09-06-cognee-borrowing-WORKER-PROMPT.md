# Worker pickup: Cognee borrowing into Punkrecords

Work in namespace `punk-punkrecords-improvement`; pass it explicitly on every Punk call.
Repository: `/mnt/d/Hypervisor_Code/punkrecords`.
Integration branch: local `feat/punk-improvement-pipeline`.

Implement one ready task from the existing Cognee borrowing workstream. These tasks are authorized by the user's improvement-pipeline request. Use the current plan; do not create a duplicate queue or restart architecture brainstorming.

The live Punk server at `127.0.0.1:9090` is for coordination only. Never restart, replace, migrate or test experimental code against it. Use temporary databases/configs/servers. Never change the user's installed binary, real Codex config or memory data. No push, main merge, release, tag or deployment.

1. Use punk-memory; register a unique worker identity. Recall `/plan/summary`, `/plan/cognee-borrowing`, `/conventions/repo`, `/conventions/live-server` and `/conventions/review` and `/conventions/mcp-waits`.
2. Read the current integration branch's files:
   - `docs/superpowers/plans/2026-09-06-cognee-borrowing.md`
   - `docs/superpowers/specs/2026-09-05-punk-improvement-pipeline-design.md`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.md`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.tasks.json`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline-WORKER-PROMPT.md`
3. Call `list_tasks` before waiting. Choose a currently ready E/A/G/I/S/P/R/H task; completed tasks need no new worker. Z01 requires all listed prerequisites including C06. C02 needs the original terminal verification; it does not block unrelated ready borrowing tasks. A wait watches future changes and does not substitute for inspecting work already ready.
4. Claim `/tasks/<id>` with a 3600-second lease. Recall `/tasks/<id>`, `/answers/<id>` and existing submission evidence. If it is returned work, preserve/reuse the prior patch after checking ownership.
5. Create an isolated task worktree from the latest local integration tip; never switch/reset another checkout. Claim exact `/files/<relative-path>` entries in sorted order, releasing partial sets on conflict. All migration work additionally claims `/coordination/migrations`. Read `/conventions/overlapping-worker-files`: siblings must coordinate shared files even when they share one claim holder; isolated worktrees alone do not prevent incompatible changes.
6. Report `in_progress` with branch/path. Follow the task's red proof, narrow implementation and checks. Reuse Punk's existing Go/store/retrieval machinery; record pinned Cognee inspiration. No paid/model-backed runs without explicit configured budget; deterministic tests use fakes.
7. Run relevant tests, gofmt, vet/build and required database checks. Record unavailable prerequisites honestly. Create one task commit with the master plan's message, staging only owned files.
8. Write `/reviews/<id>/submission` FIRST: branch, full SHA, base, upstream mechanism/path, red/green checks, artifacts, compatibility and deviations. Then set status `review` and release task/file claims. A reviewer integrates and marks `done`; worker commits alone do not unlock dependents.
9. If blocked, write `/questions/<id>` with precise evidence, set `blocked`, release claims and select independent ready work. For continued assignment, re-read the board and use `await_tasks(timeout_seconds=45)` when no work is ready. Never reuse remembered readiness.

Returned tasks already have implementation branches and reviewer regressions. Read `/answers/<id>` before rerunning gates or resubmitting; a rejected unchanged commit is not a new submission. Preserve reviewer-marked `done` when a delayed handoff names the already accepted worker commit. The live board decides readiness.

Timeout recovery: use explicit 45-second waits. Earlier sessions canceled longer calls at about 60 seconds. OpenCode 1.18.29 supports `mcp.punk.timeout=330000` milliseconds; this host now resolves that value in a fresh process, but a running session must reload before relying on it. Longer waits require a verified effective client deadline with response margin; the server maximum of 300 seconds does not extend it. A timed-out call returns no task board: call `list_tasks` once, inspect ready work and `/answers/<id>`, then wait again only if nothing is ready. Never repeat an oversized wait. C08 is integrated locally; the live Punk server has not been replaced.
