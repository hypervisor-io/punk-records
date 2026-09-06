# Runbook: Database Failover

This runbook covers postgres failover.

## Detection

Watch for connection saturation.
Alerts fire on pg-1.

## Failover Steps

Promote the standby.
Update the service record.
