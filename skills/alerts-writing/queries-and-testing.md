# Query Patterns, Recording Rules, and Testing Alert Rules

Supports `SKILL.md`. Read this when writing PromQL for alerts, designing recording rules, or setting up CI/canary workflows for rule changes.

## PromQL rules that are non-negotiable

- Counters: always `rate()` or `increase()`, never raw cumulative values.
- **Rate before aggregate**: `sum(rate(m[5m]))`, never `rate(sum(m)[5m])` — aggregating first hides counter resets and produces garbage after restarts.
- **Never `irate()` in alerting.** It's for graphing volatile counters; in alerts, a single spike resets the `for` clock and a single dip falsely resolves.
- Latency: `histogram_quantile()` over `rate()` of buckets, aggregated with `sum by (le, ...)`. Never alert on averages — they mathematically hide the long tail. Let the SLO drive bucket layout for classic histograms; prefer native histograms for new instrumentation.
- Preserve routing dimensions in aggregations: `sum by (service, cluster, env)` — dropping them breaks Alertmanager grouping and team routing.
- Missing-signal detection: `absent()` / `absent_over_time()` for scrape-target or metric disappearance; better yet, a black-box probe of the actual user path.

## Canonical query snippets

Error ratio:

```promql
sum by (service) (rate(http_requests_total{code=~"5.."}[5m]))
/
sum by (service) (rate(http_requests_total[5m]))
```

Fraction of requests over the latency objective (bucket-based SLI — cheaper and more SLO-aligned than a quantile):

```promql
1 - (
  sum by (service) (rate(http_request_duration_seconds_bucket{le="0.5"}[5m]))
  /
  sum by (service) (rate(http_request_duration_seconds_count[5m]))
)
```

p95 from a histogram (for dashboards or non-SLO latency alerts):

```promql
histogram_quantile(0.95,
  sum by (le, service) (rate(http_request_duration_seconds_bucket[5m])))
```

Predictive filesystem exhaustion (pair a low-space floor with a trajectory check):

```promql
(
  node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|fuse.*|nfs|cifs"} < 20 * 1024 * 1024 * 1024
)
and
(
  predict_linear(node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|fuse.*|nfs|cifs"}[6h], 4 * 3600) < 0
)
```

Flap/instability detection (readiness, BGP sessions, etc.):

```promql
changes(kube_node_status_condition{condition="Ready",status="true"}[15m]) > 2
```

## Recording rules

Use recording rules whenever an alert expression is expensive, reused, or multi-window. The alert then evaluates a cheap precomputed series (`expr: checkout:error_ratio:rate5m > 0.02`), which keeps rules readable and evaluation fast. Follow the `level:metric:operations` naming convention (e.g. `job:slo_errors_per_request:ratio_rate1h`). Deploy recording rules **before** the alert rules that depend on them.

## Cardinality discipline

Unbounded labels (user IDs, emails, request IDs, ephemeral pod UIDs, raw URL paths, error strings) explode series counts, exhaust TSDB memory, slow every alert evaluation, and destabilize alert identity. Keep alerting-metric cardinality low and investigate alternatives once a label can grow into the hundreds. Operating rule: **route on stable ownership labels; alert on coarse dimensions; diagnose on fine dimensions** (fine-grained data belongs in logs/traces/exemplars, not alert labels).

## CI pipeline for Prometheus rules

Rules are production code. A high-confidence pipeline:

```
promtool check rules --lint=all rules/*.yml
promtool check config prometheus.yml
promtool test rules tests/*.yml
# optional: promtool query instant against a staging Prometheus
```

Then: deploy recording rules before dependent alert rules; canary; revert via Git + redeploy of the previous validated bundle for rollback.

### Rule unit test example

`promtool test rules` asserts exactly which alerts fire at a given evaluation time against synthetic series:

```yaml
rule_files:
  - ../rules/checkout-slo.yml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'checkout:slo_errors_per_request:ratio_rate1h'
        values: '0.02+0x30'   # 2% error ratio, flat for 30 samples
      - series: 'checkout:slo_errors_per_request:ratio_rate5m'
        values: '0.02+0x30'
    alert_rule_test:
      - eval_time: 5m
        alertname: CheckoutSLOFastBurn
        exp_alerts:
          - exp_labels: {severity: page, team: payments, service: checkout, env: prod}
```

Also test the negative case (values below threshold → `exp_alerts: []`) and the pending boundary (just before `for` elapses → no alert).

## Canarying rule changes

Poorly tuned rules cause immediate operational overload, so stage them:

1. Route new/changed alerts to a **non-paging receiver** for at least a week.
2. Limit scope first: one env, one service, or one region.
3. Build a comparison dashboard: **would-have-fired** vs **did-fire** vs actual incidents.
4. Confirm end-to-end behavior with synthetics/black-box probes.
5. Promote only after false-positive rate and missing-data behavior look healthy.

## Metamonitoring

Monitor the alerting system itself: a black-box test that exercises the full path (metric source → Prometheus → Alertmanager → notification endpoint) is the only way to know a silent alerting stack is healthy rather than dead. Alertmanager HA is designed to fail open — duplicate delivery is preferred over missed pages; don't "fix" the duplicates by breaking clustering. Grafana exposes meta-monitoring metrics and alert-state history; Datadog's Monitor Quality flags long-muted monitors, missing recipients, and stuck states — wire those into the review cadence.

## Historical validation

Before shipping any threshold, run the expression against weeks of historical data (Grafana explore / `promtool query range` / recorded ratios) and count how many times it would have fired versus how many of those were real incidents. Treat alert creation as unfinished until this is done.
