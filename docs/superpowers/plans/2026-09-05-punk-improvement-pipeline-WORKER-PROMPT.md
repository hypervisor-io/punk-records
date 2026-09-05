# Worker prompt: Punkrecords improvement pipeline

Coordination namespace: **punk-punkrecords-improvement**. Pass it explicitly on every Punk call.
Repository: /mnt/d/Hypervisor_Code/punkrecords.
Integration branch: feat/punk-improvement-pipeline.
Compatibility target: Codex 0.153.4.

## Protect the live coordination server

http://127.0.0.1:9090 is the live shared memory/coordination server. Never stop, restart, replace, deploy to, migrate or run experimental imports against it. Do not kill processes by name or overwrite an installed punk binary. Do not edit the user's real Codex hooks/config or memory data. The live server is for coordination facts and claims only.

Use httptest and t.TempDir wherever possible. A manual dev server uses a fresh temporary directory, a new SQLite DB and an unused loopback port such as 19393. Build the test binary into that directory. Write a minimal configuration with http.addr=127.0.0.1:19393, db.driver=sqlite, db.dsn=<temporary absolute path>, ai.enabled=false and specs.dir pointing at this task worktree's specs. Run migrations and serve with that exact config; stop only the PID you started and remove only your temporary directory. Never use production credentials or a default config.yaml for a test server. Parallel workers must choose different ports/DBs.

## Setup

1. Use punk-memory and the task-relevant coding/debugging skills. Call whoami for diagnostics, then register with a unique stable agent identity and role worker in punk-punkrecords-improvement.
2. Recall /plan/summary, /conventions/repo and /conventions/live-server in this namespace.
3. Read:
   - docs/superpowers/specs/2026-09-05-punk-improvement-pipeline-design.md
   - docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.md
   - docs/superpowers/plans/2026-09-05-punk-improvement-pipeline.tasks.json
4. Inspect git status and worktree list. Never switch branches in someone else's checkout. Confirm local feat/punk-improvement-pipeline exists. The initial plan is local; do not assume origin has it. If on a different machine without it, request the plan/branch artifact instead of fabricating a branch from stale origin/main.
5. These are authorized implementation tasks when the user gives you this prompt. Complete the selected contract; do not restart brainstorming about the entire pipeline. Ask only for concrete missing input that blocks the task.

## Pick work safely

6. Call list_tasks(namespace="punk-punkrecords-improvement"). Pick next or another ready task. Respect the Codex-first gate. A dependency is satisfied only by reviewer-marked done with a commit present in the integration branch.
7. Claim /tasks/<id> with your agent identity as holder and ttl_seconds=3600; only the successful claimant works it. Recall that exact task and its plan section.
8. Read /answers/<id> and any existing /reviews/<id>/submission first; a returned pending task may already have a worker branch that needs correction. Reuse/rebase that work only after checking ownership and recording the new holder; do not lose the prior patch. Create an isolated git worktree and task branch based on the current integration branch, named work/punk-improve-<task-id>-<unique-worker-suffix>. Keep the worktree outside the primary checkout. Record the branch and absolute path in task status.
9. Expand directory/glob/new-file entries into concrete files you will edit and claim each normalized repository-relative path under /files/<repo-relative-path> in sorted order before edits. Punk claims match exact keys: a directory claim does NOT protect child files. All schema tasks additionally claim the identical allocation mutex /coordination/migrations; keep it until the task's migration filenames are settled and submitted. If one claim fails, release acquired file claims and wait/reselect; do not hold a partial set and deadlock another worker. Migration numbers must be checked again against the integration tip before integration. Add newly discovered files to the complete sorted claim set without holding a partial conflicting set.
10. Use set_task_status state=in_progress, phase=red with the selected task ID. Renew the task and file leases before expiry. A stale local belief never overrides a fresh task board or claim rejection.

## Build and verify

11. Write and run a meaningful failing test first, implement the smallest change, then run the same test and the task checks. Use existing test helpers. Keep stdout/stderr protocol semantics and both database dialects in mind.
12. Codex UI task C02 requires actual version-pinned host evidence. The screenshot shows repeated project-name text after a static transcript footer; prioritize the title on/off versus hooks on/off A/B matrix and raw terminal output, not an assumed memory injection bug. We have NOT reproduced the exact transcript spam. Do not call duplicate skill catalogs its root cause without proof; do not add unsupported suppressOutput flags. Healthy context-only hooks should be hidden by the upstream CLI renderer. Record host/app build as well as CLI version.
13. No unrelated refactors, new services, paid benchmark calls by default, destructive entity merges, live config repair or data migrations. New optional behavior retains old defaults.
14. If current code differs from plan names/signatures, adapt at the narrow boundary and record the deviation. Do not distort the implementation just to match a speculative filename.
15. Run gofmt on changed Go files, the task's tests, and go vet/go build on affected packages. Cross-cutting changes must run the full checks in /conventions/repo. Inspect the final diff.
16. Create one task commit with the plan's exact message. Stage only your files; never git add an unrelated dirty tree. No Co-Authored-By or session attribution trailers. Do not push, merge to main, tag, release or deploy.

## Submit and continue

17. Set task status review, including worker branch, full commit SHA, exact checks, red/green evidence and deviations. Use /reviews/<id>/submission for details that do not fit in the status line. Release your task and file claims. Do NOT mark done just because your private branch passed tests.
18. The reviewer integrates the accepted commit and marks done with the integrated SHA; this is what unlocks dependencies. Inspect /answers/<id> for requested corrections. Reclaim before editing and amend/recreate the task commit as the reviewer directs.
19. If blocked, write the precise question, evidence and attempted resolution under /questions/<id>, set status blocked and release claims. Check /answers/<id> before retrying. A missing external UI reproduction is a real blocker; unrelated ready work may continue.
20. Re-read list_tasks before taking another task. If nothing is ready, use await_tasks(timeout_seconds=55) and inspect its fresh board. Do not busy-poll.
21. Stop when your assigned work is submitted or the user stops you. Only the final reviewer writes /plan/status complete after every task is integrated and the final acceptance gate passes. Never merge, tag, release or deploy as a worker.
