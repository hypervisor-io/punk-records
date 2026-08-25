---
name: db-replication-lag
description: >-
  Diagnose replication lag: replica delay alerts, read-after-write
  complaints, failover risk. Use when an incident shows replica lag
  metrics, stale reads, or replication-broken alerts.
license: LGPL-2.1-only OR LGPL-3.0-only
metadata:
  version: 0.1.0
  action_class: read
  approval_required: "false"
allowed-tools: incidents__get_incident incidents__list_incidents
---

# Replication lag triage

1. Determine whether lag is growing, flat, or sawtooth from the alert
   sequence on the timeline; growing lag with flat write volume points
   at the replica, sawtooth points at bursty writes.
2. Correlate lag onset with deploys, bulk jobs, and vacuum events on the
   primary within the previous hour.
3. Check for single-replica vs all-replicas lag across alerts; all
   replicas lagging is a primary-side or network problem.
4. Assess failover exposure: if lag exceeds the deployment's RPO note it
   explicitly in the finding.
5. Finding must state lag shape, suspected side (primary, replica,
   network), evidence rows, and confidence.
6. Propose the matching runbook (replica rebuild, job pause, failover
   readiness check) by slug; never execute.
