# Payloads, Naming, Runbooks, Review Checklists, and Examples

Supports `SKILL.md`. Read this when writing alert payloads or runbooks, reviewing an alert set, or judging on-call health.

## Naming

Pattern: `ServiceConditionSeverity` or `ServiceSLOFastBurn`, CamelCase (community convention). The name alone should convey what broke and roughly how urgent it is.

Good: `CheckoutSLOFastBurn`, `KubeAPISLOFastBurn`, `CoreDNSHighErrorRatio`, `NodeFilesystemWillFillSoon`, `LoadBalancerNoHealthyBackends`, `BGPPeerDownProductionPath`.

Weak: `HighCPU`, `ServiceBad`, `Errors`, `CriticalAlert`, `TooManyFailures`.

Scope names multi-dimensionally: one `NodeFilesystemWillFillSoon` rule covering the fleet via labels, not `NodeFilesystemFullHost42` per host.

## Payload contract

Labels (routing/identity — stable, low-cardinality):

- Required: `alertname`, `severity`, `team`, `service`, `env`
- Useful when stable: `cluster`, `namespace`, `component`, `region`
- Forbidden in identity/routing: pod UID, container ID, request/trace/order/user/product IDs, emails, raw paths, raw error messages

Annotations (human context):

- `summary` — one sentence, what is broken
- `description` — threshold crossed, for how long, affected scope; use templated values (`{{ $labels.node }}`, `{{ $value }}`)
- `impact` — likely user/platform impact
- `action` — first diagnostic or mitigation step
- `dashboard_url` — dashboard focused on this alert
- `runbook_url` — mandatory for every page; an undocumented alert is toxic toil

## Fast review checklist

- [ ] The alert has one clear owner.
- [ ] The severity matches the required response.
- [ ] The query measures a symptom, SLO burn, or imminent hard failure.
- [ ] The threshold is based on SLO, baseline, capacity model, or known failure mode — not a guess.
- [ ] The rule has a pending period or multi-window logic; counters use `rate()`/`increase()`, no `irate()`.
- [ ] The alert avoids high-cardinality labels in identity and routing.
- [ ] The notification includes summary, impact, current value, threshold, dashboard, and runbook.
- [ ] Grouping / notification policy is defined for the unit of action.
- [ ] Parent/child inhibition is considered.
- [ ] Planned maintenance suppression works without disabling evaluation.
- [ ] There is a `promtool` unit test or historical query proving the rule behaves as expected.

Reject or downgrade alerts that only say a metric is unusual, lack an owner, lack a runbook, trigger on a single blip, or require no human action.

## Good vs poor example

Poor:

```yaml
- alert: HighCPU
  expr: avg(cpu_usage_percent) > 80
  labels:
    severity: critical
```

No owner, cause not symptom, no duration (flaps), no runbook/dashboard/impact, ambiguous severity, and `avg()` across the fleet hides the actual hot node.

Fixed, as a ticket:

```yaml
- alert: NodeCPUSaturationPersistent
  expr: |
    avg by (cluster, node) (1 - rate(node_cpu_seconds_total{mode="idle"}[5m])) > 0.95
  for: 30m
  labels:
    severity: ticket
    team: platform
    service: node
    env: prod
  annotations:
    summary: "Node CPU is persistently saturated"
    description: "Node {{ $labels.node }} has CPU utilization above 95% for 30 minutes."
    impact: "May reduce scheduling headroom or contribute to latency if workloads are affected."
    action: "Check workload placement, throttling, steal time, and node pressure before scaling or draining."
    dashboard_url: "https://grafana.example.com/d/node-overview"
    runbook_url: "https://runbooks.example.com/node/cpu-saturation"
```

For the corresponding page-worthy pattern (SLO fast burn), see `slo-and-thresholds.md`.

## Runbooks

A runbook must answer four questions in under a minute: what the alert means, how bad it is, what to check first, and which mitigations are safe before root-cause analysis. Principles (the five A's): **Actionable** (copy-pasteable commands, not narrative), **Accessible** (linked from the alert payload — never searched for mid-incident), **Accurate** (version-controlled, reviewed; one stale command destroys trust in the whole document), **Authoritative** (one source of truth per incident type), **Adaptable** (updated from every postmortem).

Template:

```text
Alert: <AlertName>
Owner: <team-oncall>
Severity: page|ticket

Meaning & impact
<What condition fired and the blast radius, e.g. "checkout error ratio
exceeded the fast-burn threshold; users in prod may fail to purchase.">

First 5 minutes
- Acknowledge; announce presence in the incident channel
- Confirm user impact on the SLO dashboard: <link>
- Check deploys/feature flags in the last 2h (statistically the most likely trigger)
- Check dependency health: auth, DB, queue, edge

Triage questions
- >10% of traffic affected? Data loss suspected? Single region or global?

Mitigate first (rollback beats live debugging)
- Roll back the latest deploy if correlated
- Shift traffic / fail over / enable degraded mode or circuit breaker

Escalate
- Secondary on-call after 15m unacked
- Incident Commander after 30m of persisting user impact
- Comms lead once impact is customer-facing

Exit criteria
- Burn rate back below threshold; probes recovered; status page updated

Follow-up
- Timeline, corrective actions, and review of threshold/routing/runbook quality
```

As a runbook matures, automate it: first scripts that gather context when the alert fires, then automated mitigation (feature-flag disable, pipeline-triggered rollback) before a human is paged. If the full resolution is reliably scriptable, the page should not exist.

## On-call health and program KPIs

| KPI | Target / why |
|---|---|
| Incidents per on-call shift | ≤2 (Google on-call workbook); sustained higher volume is itself a reliability defect |
| Alerts per incident | Drive noisy rules toward 1:1; ratio ≫1 means grouping/inhibition is broken |
| False-positive rate | Direct precision metric; every FP erodes trust |
| MTTA / MTTR | Acknowledgment and resolution speed per alert group |
| No-data / error-state frequency | Blind spots and query instability |
| Monitors long-muted or lacking recipients | Governance hygiene (Datadog Monitor Quality flags these) |

Review these monthly with on-call engineers; prune ruthlessly.

## Anti-patterns

- Paging on causes (CPU, memory %, single-host blips) without user impact or a hard-failure horizon.
- Paging when the only action is "watch it".
- `irate()` in alerts; `rate(sum(...))` instead of `sum(rate(...))`; alerting on averages instead of percentiles.
- Alerting on "free" memory instead of `MemAvailable`.
- Many near-duplicate rules instead of one multi-dimensional rule.
- No owner, no runbook, no dashboard, no pending period.
- High-cardinality labels in alert identity; per-pod notifications during one service incident.
- `group_by: ['...']` (accidentally disabled aggregation).
- Disabling rule evaluation during maintenance instead of suppressing notifications.
- Long-lived silences/mutes that become permanent blindness.
- Shipping a rule without a unit test or historical validation.
- Keeping noisy alerts because they are "sometimes useful".
- Not monitoring the alerting stack itself (no metamonitoring, no end-to-end notification probe).
