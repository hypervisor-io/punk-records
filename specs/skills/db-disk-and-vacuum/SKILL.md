---
name: db-disk-and-vacuum
description: >-
  Diagnose disk pressure and vacuum/bloat problems: disk usage alerts,
  transaction ID age warnings, autovacuum starvation, WAL growth. Use
  when an incident shows disk-space alerts or slow queries with rising
  table bloat.
license: LGPL-2.1-only OR LGPL-3.0-only
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
allowed-tools: incidents__get_incident incidents__list_incidents
---

# Disk and vacuum triage

1. Split the symptom: raw disk-space exhaustion, WAL accumulation, or
   bloat-driven slowness. The alert source on the timeline usually says
   which.
2. For disk exhaustion: check growth rate against the alert history; a
   step change points at a new workload or stuck WAL archiving, a steady
   slope points at organic growth.
3. For WAL growth: look for replication slots referenced in alerts;
   an abandoned slot pins WAL forever.
4. For bloat: correlate with autovacuum settings changes or long-running
   transactions that block cleanup.
5. Finding must state which of the three shapes it is, the driving
   process, evidence rows, and confidence.
6. Propose the matching runbook (archive cleanup, slot drop, emergency
   vacuum) by slug; never execute.
