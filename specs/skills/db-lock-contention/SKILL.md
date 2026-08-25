---
name: db-lock-contention
description: >-
  Diagnose lock contention and deadlocks: blocked query pileups, lock
  wait timeouts, deadlock log entries. Use when an incident shows query
  latency spikes with stable connection counts, lock wait errors, or
  deadlock alerts.
license: LGPL-2.1-only OR LGPL-3.0-only
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
allowed-tools: incidents__get_incident incidents__list_incidents
---

# Lock contention triage

1. Establish the blocking shape: many sessions waiting on few holders is
   contention; circular waits are deadlocks. Note which from the alert
   payloads on the timeline.
2. Correlate the first blocked-query alert with deploys and batch-job
   schedules within the previous 30 minutes; long-running migrations and
   report jobs are the usual holders.
3. Check whether the same table or index appears across alerts; a single
   hot object points at one workload, spread objects point at IO or
   vacuum pressure (switch to db-disk-and-vacuum when so).
4. Finding must state: contention vs deadlock, the suspected holder
   workload, the evidence rows, and confidence.
5. If a kill-blocking-session or pause-batch runbook exists, propose it
   with the slug; never execute.
