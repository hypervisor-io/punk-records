# Reviewer/integrator prompt: Punkrecords improvement pipeline

Use namespace punk-punkrecords-improvement on every Punk call. The live server at http://127.0.0.1:9090 must remain untouched except for coordination records.
Repository: /mnt/d/Hypervisor_Code/punkrecords. Integration branch: feat/punk-improvement-pipeline.

Register with a unique identity and role reviewer. Read /plan/summary, both conventions, the design brief, implementation plan and worker prompt. Use list_tasks and await_tasks(timeout_seconds=55); do not assume a worker's previous status is current.

For each review task:
1. Recall /tasks/<id> and /reviews/<id>/submission. Resolve its local worker branch and full commit. Confirm it descends from the appropriate integration base and contains only task changes.
2. Review against the contract, red proof, compatibility and acceptance. Run the checks from a disposable detached worktree at that commit. Never switch or reset a worker's checkout. Missing native UI evidence, PostgreSQL coverage or external checks cannot be called passing.
3. If changes are needed, record exact actionable feedback at /answers/<id>, record the existing worker branch/commit, then set status pending so the board makes it ready for correction. This is an explicit requeue, not completion; the next claimant reads /answers and reuses/rebases the prior work. If a missing external prerequisite prevents correction, use blocked with a precise question instead. Do not mark done.
4. If accepted, claim /coordination/integration, ttl_seconds=3600. Locate the primary integration worktree with git worktree list; it must be on feat/punk-improvement-pipeline with no unrelated edits. Workers never edit this worktree.
5. Re-check the latest integration tip and already-integrated task records. Cherry-pick the accepted one-task commit onto feat/punk-improvement-pipeline. Never merge to main. If conflicts or semantic overlaps appear, abort that cherry-pick and requeue the task as pending with the existing branch for rebasing and another review; do not invent conflict resolutions that were never tested.
6. Run relevant checks at the integrated tip; cross-cutting changes require the broader gate. Record /reviews/<id>/accepted with original and integrated SHAs, commands and outcome. Mark the task done using the integrated SHA and exact tests. Only this done state unlocks dependencies.
7. If integration checks fail, do not leave an unrecorded broken branch or mark done. Keep the integration lease while repairing/reverting only your just-integrated commit, document the outcome, and requeue the task as pending with the existing branch and feedback. Never reset away another worker's commits.
8. Release /coordination/integration and any task/file claims. Do not push or publish unless the user separately asks.

Lease expiration is not permission to interrupt an active git process. Inspect the holder/worktree and write a coordination question before recovering an abandoned integration.

Final gate:
After Z01's report is reviewed and integrated, verify all 23 task rows are done and their integrated SHAs are ancestors of the integration branch. Run the brief's complete gate in a detached review worktree. Write /plan/review with actual results and remaining limitations, then /plan/status = complete-punkrecords-improvement: <integrated tip SHA>. Update /plan/current pointers to say implementation is complete and awaiting the user's integration/release decision. Do not merge to main, tag, release, restart or deploy the live server.
