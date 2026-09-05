# Worker pickup: Cognee borrowing into Punkrecords

Work in namespace `punk-punkrecords-improvement`; pass it explicitly on every Punk call.
Repository: `/mnt/d/Hypervisor_Code/punkrecords`.
Integration branch: local `feat/punk-improvement-pipeline`.

Implement one ready task from the existing Cognee borrowing workstream. These tasks are authorized by the user's improvement-pipeline request. Use the current plan; do not create a duplicate queue or restart architecture brainstorming.

The live Punk server at `127.0.0.1:9090` is for coordination only. Never restart, replace, migrate or test experimental code against it. Use temporary databases/configs/servers. Never change the user's installed binary, real Codex config or memory data. No push, main merge, release, tag or deployment.

1. Use punk-memory; register a unique worker identity. Recall `/plan/summary`, `/plan/cognee-borrowing`, `/conventions/repo`, `/conventions/live-server` and `/conventions/review`.
2. Read the current integration branch's files:
   - `docs/superpowers/plans/2026-09-06-cognee-borrowing.md`
   - `docs/superpowers/specs/2026-09-05-punk-improvement-pipeline-design.md`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.md`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.tasks.json`
   - `docs/superpowers/plans/2026-09-05-punk-improvement-pipeline-WORKER-PROMPT.md`
3. Call `list_tasks`. Prefer ready E01 (evaluation) or A01 (authorization), then other ready E/A/G/I/S/P/R/H tasks. Z01 requires all listed prerequisites including C06. The old global Codex-first barrier was removed; C02 remains blocked and C05 remains with its reviewer. Do not claim either for this assignment.
4. Claim `/tasks/<id>` with a 3600-second lease. Recall `/tasks/<id>`, `/answers/<id>` and existing submission evidence. If it is returned work, preserve/reuse the prior patch after checking ownership.
5. Create an isolated task worktree from the latest local integration tip; never switch/reset another checkout. Claim exact `/files/<relative-path>` entries in sorted order, releasing partial sets on conflict. All migration work additionally claims `/coordination/migrations`. E01/A01 share the CLI file and must serialize it.
6. Report `in_progress` with branch/path. Follow the task's red proof, narrow implementation and checks. Reuse Punk's existing Go/store/retrieval machinery; record pinned Cognee inspiration. No paid/model-backed runs without explicit configured budget; deterministic tests use fakes.
7. Run relevant tests, gofmt, vet/build and required database checks. Record unavailable prerequisites honestly. Create one task commit with the master plan's message, staging only owned files.
8. Write `/reviews/<id>/submission` FIRST: branch, full SHA, base, upstream mechanism/path, red/green checks, artifacts, compatibility and deviations. Then set status `review` and release task/file claims. A reviewer integrates and marks `done`; worker commits alone do not unlock dependents.
9. If blocked, write `/questions/<id>` with precise evidence, set `blocked`, release claims and select independent ready work. For continued assignment, re-read the board and use `await_tasks(timeout_seconds=50)` when no work is ready. Never reuse remembered readiness.

Starting recommendations: one worker takes E01, another A01, subject to claims. E02 and A02 follow their respective integrated prerequisites. The manifest, not this recommendation, decides readiness.
