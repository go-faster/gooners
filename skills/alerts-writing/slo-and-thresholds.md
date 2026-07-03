# SLOs, Burn Rates, and Threshold Design

Supports `SKILL.md`. Read this when designing SLO alerts, choosing thresholds or windows, or fixing flapping.

## Why static thresholds fail for reliability alerting

A static "error rate > 1% for 10 minutes" rule has no temporal awareness: that event consumes only ~0.02% of a monthly 99.9% budget, yet it pages. Static rules are disconnected from the business's actual failure tolerance and flap on transient anomalies. Burn-rate alerting fixes both by measuring the *velocity* of error-budget consumption.

## Error budget and burn rate math

- SLO = target reliability over a compliance window (usually 28–30 days). Error budget = 1 − SLO. A 99.9% SLO over 30 days permits 0.1% failures ≈ 43.2 minutes of full downtime.
- **Burn rate = observed error ratio / budget ratio.** Burn rate 1 exhausts the budget exactly at the end of the window. Burn rate 2 → 15 days; 10 → 3 days; 14.4 → ~2 days; 1000 (total outage at 99.9%) → 43 minutes.
- Detection time for a total outage ≈ `long_window × burn_rate × (1 − SLO)`... practically: with a 1h window and 14.4x threshold at 99.9%, a full outage fires in ~4.3 minutes (2% of monthly budget consumed by fire time).
- Budget consumed when the alert fires = `burn_rate × long_window / compliance_window`. This is the knob to reason about: "page when 2% of the month's budget went up in smoke in an hour."

## Multi-window, multi-burn-rate

Single-window alerting forces a bad trade: short windows detect fast but false-positive on blips; long windows are precise but detect slowly and *reset* slowly (the alert stays firing on smoothed historical data long after mitigation). The fix: require the burn threshold on **both** a long window and a short window (≈ 1/12 of the long window). The long window proves it's sustained; the short window proves it's happening *right now* — which also gives fast reset.

Canonical 99.9% configuration (Google SRE Workbook):

| Severity | Long | Short | Burn rate | Error ratio (99.9%) | Budget consumed |
|---|---:|---:|---:|---:|---:|
| page | 1h | 5m | 14.4x | >1.44% | 2% |
| page | 6h | 30m | 6x | >0.60% | 5% |
| ticket | 1d | 2h | 3x | >0.30% | 10% |
| ticket | 3d | 6h | 1x | >0.10% | 10% |

Prometheus implementation — recording rules per window, then combine:

```yaml
groups:
  - name: checkout.slo
    interval: 30s
    rules:
      - record: checkout:slo_errors_per_request:ratio_rate5m
        expr: |
          sum(rate(http_requests_total{service="checkout",code=~"5.."}[5m]))
          /
          sum(rate(http_requests_total{service="checkout"}[5m]))
      - record: checkout:slo_errors_per_request:ratio_rate1h
        expr: |
          sum(rate(http_requests_total{service="checkout",code=~"5.."}[1h]))
          /
          sum(rate(http_requests_total{service="checkout"}[1h]))

      - alert: CheckoutSLOFastBurn
        expr: |
          (checkout:slo_errors_per_request:ratio_rate1h > (14.4 * 0.001))
          and
          (checkout:slo_errors_per_request:ratio_rate5m > (14.4 * 0.001))
        for: 2m
        keep_firing_for: 5m
        labels: {severity: page, team: payments, service: checkout, env: prod}
        annotations:
          summary: "Checkout is burning error budget quickly"
          description: "Error ratio exceeds the 14.4x burn threshold over both 1h and 5m windows."
          impact: "Users may be unable to complete purchases."
          action: "Check the checkout SLO dashboard, recent deploys, and upstream dependency errors."
          dashboard_url: "https://grafana.example.com/d/checkout-slo"
          runbook_url: "https://runbooks.example.com/checkout/slo-fast-burn"
```

If a burn-rate alert is still flaky, enlarge the short window slightly — the cost is slower recovery detection, not slower firing.

## Low-SLO services: the math breaks

The 14.4x table assumes a high SLO. **Maximum possible burn rate = 100 / (100 − SLO%).** At 85% SLO the max is 6.67x — a 14.4x rule can literally never fire, even during a total outage (a full hour of 100% errors consumes only ~1.4% of an 85% monthly budget). Options:

1. Alert on much smaller budget-consumed fractions (lower burn thresholds).
2. Split into dual SLOs: a strict short-horizon SLO (e.g. 85% over a rolling 24h) for tactical paging, plus a looser long-horizon SLO (e.g. 95% over 30d) for reporting.

## Low-traffic services: statistics break

With little traffic, one failed request can spike the ratio to 100% and instantly trip a fast-burn page. Mitigations, combinable:

- **Minimum-failure gate** — require an absolute failure count alongside the ratio:

```promql
(
  sum(rate(http_requests_total{code=~"5.."}[30m]))
  /
  sum(rate(http_requests_total[30m]))
) > 0.05
and
sum(increase(http_requests_total{code=~"5.."}[30m])) >= 20
```

- **Synthetic traffic** — probes provide a continuous success baseline and an independent user-path signal.
- **Service consolidation** — compute the SLI over a logical group of small related services.
- **Client-side retries** with backoff+jitter so transient blips don't register as hard failures.

## Threshold hierarchy for everything else

1. **SLO burn-rate thresholds** for customer-facing reliability.
2. **Rate/ratio thresholds** for counters and error signals.
3. **Absolute thresholds** for capacities, quotas, queue depth, and "last successful run" batch signals.
4. **Predictive thresholds** (`predict_linear`) where failure jumps suddenly from healthy to catastrophic — disks, inodes, quotas, certificate expiry, conntrack. Prefer prediction over static percentages: 10% free on a multi-TB array is hundreds of safe gigabytes; 10% free on a fast-filling log partition is minutes.
5. Baseline-relative thresholds (e.g. 2x normal p95) for signals with no natural absolute limit — always tuned from several weeks of real data, not invented.

## Flapping control toolbox

Apply in combination, not as alternatives:

- **Pending period** (`for` / evaluation count): never trust a single sample. ≥5m unless it's a hard-outage detector.
- **Keep-firing / recovery window** (`keep_firing_for`): absorbs resolve→refire oscillation.
- **Hysteresis**: different trigger and resolve thresholds (fire >500ms, resolve <400ms) create a deadband so a metric hovering at 450ms can't toggle. Native in Datadog (recovery thresholds); emulate in Prometheus with two expressions or accept `keep_firing_for` as the poor man's version.
- **Multi-location verification**: require ≥2 geographically distinct probes to agree before paging on external paths — one vantage point failing is a path problem, not an outage.
- **Dependency logic / inhibition**: switch down → suppress the hundreds of "unreachable" children behind it.
- **Missing-data tuning**: for sparse or delayed metrics, configure No Data behavior deliberately (Grafana No Data state, Datadog `require_full_window: false` + `evaluation_delay`, Prometheus `absent()`) instead of blindly notifying on gaps.
- **Grouping**: one incident → one notification bundle (see `routing-and-platforms.md`).

## Error budget policy (organizational teeth)

Burn alerts only matter if budget depletion drives behavior. A workable policy: at 33% monthly budget burned, dedicate engineers to investigate; at 66%, swarm; at 100%, feature freeze + postmortem + reliability-only work until recovered. Encode the policy next to the SLO definition in version control.
