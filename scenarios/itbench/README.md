# ITBench SRE scenarios

Offline, snapshot-based SRE incident scenarios for `punk itbench run`. Each
subdirectory is one scenario in the ITBench-Lite layout (`alerts.json`,
`metrics`, `k8s_*_raw.tsv`, `otel_*_raw.tsv`, `ground_truth.yaml`). The agent
must identify the faulty entity named in `ground_truth.yaml`.

See [../../docs/ITBENCH.md](../../docs/ITBENCH.md) for how to run, import
ITBench-Lite scenarios, and wire the live IBM sandbox.

- `pool-exhaustion/` - checkout-service exhausts its DB connection pool after a
  deploy; cart-service and web-frontend error as victims.
