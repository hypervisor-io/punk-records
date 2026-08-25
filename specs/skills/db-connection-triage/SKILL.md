---
name: db-connection-triage
description: >-
  Diagnose database connection saturation: max_connections pressure, pool
  exhaustion, connection leaks after deploys. Use when an incident shows
  connection errors, timeouts acquiring connections, or "too many clients".
license: LGPL-2.1-only OR LGPL-3.0-only
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
allowed-tools: incidents__get_incident incidents__list_incidents
---

# Connection saturation triage

1. Pull the incident timeline; note first alert time and any deploy events
   within the previous 60 minutes.
2. Compare active connection count against the configured ceiling; a
   sustained ratio above 0.85 is saturation, spikes that recover are load.
3. Check whether connections are held by one application (leak after
   deploy) or spread evenly (organic load growth).
4. Finding must state: saturation vs leak vs load, the evidence rows, and
   the confidence level.
5. If a matching remediation runbook exists (pool resize, rolling restart
   of the leaking service), propose it; never execute anything.
