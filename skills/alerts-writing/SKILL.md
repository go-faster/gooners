---
name: alert-writing
description: Create, review, rewrite, or tune monitoring alerts, alert rules, SLO/burn-rate alerts, recording rules, Alertmanager/Grafana/Datadog routing, notification policies, and runbooks. Use this skill whenever the user mentions alerting, paging, on-call noise, alert fatigue, PromQL alert expressions, thresholds, severities, error budgets, burn rates, inhibition, silences, maintenance windows, or asks to review an existing alert set — even if they don't say the word "alert" (e.g. "our on-call is getting woken up too much", "write a rule that fires when disk fills").
---

# Alert Writing Skill

Keep the alert set small, actionable, and owned. A good alert tells the right human about a real or imminent problem early enough to act, with enough context to start mitigation immediately. Paging a human is expensive: it costs sleep, focus (~23 minutes to recover from an interruption), and — when overused — trust in the entire alerting system. Err on the side of removing noisy alerts; over-monitoring is a harder operational problem than under-monitoring.

## Reference files — read the relevant ones before producing output

| File | Read when the task involves |
|---|---|
| [`references/slo-and-thresholds.md`](references/slo-and-thresholds.md) | SLO/burn-rate alerts, error budgets, threshold selection, windows, flapping, hysteresis, low-traffic or low-SLO services |
| [`references/queries-and-testing.md`](references/queries-and-testing.md) | PromQL expressions, recording rules, cardinality, `promtool` unit tests, CI/CD, canarying rule changes |
| [`references/infrastructure-catalog.md`](references/infrastructure-catalog.md) | Kubernetes, etcd, control plane, VMs/hosts, disks, hardware, DNS, BGP, Cilium, load balancers — concrete alerts with thresholds |
| [`references/routing-and-platforms.md`](references/routing-and-platforms.md) | Alertmanager/Grafana/Datadog config, grouping, inhibition, silences, maintenance windows, escalation, platform-specific gotchas |
| [`references/payloads-and-runbooks.md`](references/payloads-and-runbooks.md) | Labels, annotations, naming, runbook writing, on-call KPIs, review checklists, good/bad examples |

When reviewing a proposed alert, always run the checklist in `references/payloads-and-runbooks.md`.

## Core rule

Only create a paging alert when all of these are true:

1. The condition is user-impacting, SLO-impacting, or an imminent hard failure.
2. The responder can take a concrete action now.
3. The action cannot safely wait until business hours.
4. Automation cannot reliably fix it without human judgment — if the fix is scriptable, automate it instead of paging.
5. The alert has a clear owning team and a runbook or dashboard.

When any condition is false, route the signal to a ticket, dashboard, digest, or log stream instead of paging.

## Prefer symptoms over causes

Page on symptoms: failed requests, unavailable endpoints, elevated user-visible latency, failed critical jobs, DNS failures, control-plane SLO burn, or external probe failures. One symptom alert (e.g. elevated 5xx ratio) catches dozens of distinct underlying causes without a rule per cause.

Use cause signals mainly for diagnosis, tickets, or preventive warnings: CPU saturation, memory pressure, pod restarts, throttling, disk growth, queue depth, BGP peer loss, and load balancer target health. A layered design works best — symptom alerts page; cause alerts silently enrich the incident payload so the responder lands on the failing component immediately.

Exceptions where cause-based pages are acceptable:

- The cause predicts a hard failure with little or no symptom lead time: certificate expiry, disk/inode exhaustion, quota exhaustion, conntrack exhaustion, etcd database size approaching quota.
- The cause is itself a platform symptom: Kubernetes API unavailability, etcd quorum loss, DNS unavailability, or loss of a production network path.
- The service has low traffic and symptom metrics need support from synthetic probes or minimum-failure gates.
- A known defect causes failures below the symptom noise floor (e.g. 0.001% of requests in a 99.99% service) — alert on the specific causing event.

## Severity model

Use response-based severity. Do not use severity as emotional emphasis.

| Severity | Meaning | Destination |
|---|---|---|
| `page` | Immediate action required to stop user impact, fast error-budget burn, or imminent hard failure | On-call escalation |
| `ticket` | Actionable but can wait for business-hours triage | Issue tracker or low-urgency queue |
| `info` | Context only; useful for correlation or trend review | Dashboard, digest, or non-paging channel |

Every page should have a matching runbook. Every ticket should have an owner and triage expectation. Informational events should not notify anyone.

## Metric selection

Choose metrics in this order:

1. SLO / SLI burn-rate metrics for user-facing reliability (see `references/slo-and-thresholds.md`).
2. Direct symptom ratios, such as error ratio or latency over objective.
3. Saturation or capacity metrics with a predictable failure horizon (prefer `predict_linear` over static percentages).
4. Internal cause metrics used to enrich incidents, not to create more pages.

Frameworks: **Golden Signals** (latency, traffic, errors, saturation) for any user-facing system — measure latency of successes and failures separately, since fast-failing 500s deceptively lower averages. **RED** (rate, errors, duration) for request-driven services — it proxies user happiness and dictates *when* to alert. **USE** (utilization, saturation, errors) for infrastructure — it proxies machine happiness and dictates *where* to investigate. Alert on percentiles from histograms, never on bare averages.

## Thresholds and windows

Use time windows to reduce noise. Avoid single-sample alerts.

For Prometheus-like rules:

- Use `for` or pending periods, usually at least 5 minutes unless the alert is for a hard outage.
- Use `keep_firing_for` or recovery windows to reduce flap-resolve-flap loops. Where the platform supports it, use hysteresis: a recovery threshold stricter than the trigger threshold (fire at >500ms, resolve at <400ms).
- Use `rate()` or `increase()` for counters, not raw counter values.
- Apply `rate()` before aggregation: `sum(rate(metric[5m]))`, never `rate(sum(metric)[5m])` — the latter hides counter resets.
- Never use `irate()` in paging alerts; spikes reset the `for` clock.
- Use recording rules for expensive or reused alert expressions.

For SLO alerts, prefer multi-window, multi-burn-rate logic (short window ≈ 1/12 of the long window; both must breach). A common 99.9% SLO starting point:

| Destination | Long window | Short window | Burn rate | Budget consumed | Purpose |
|---|---:|---:|---:|---:|---|
| `page` | 1h | 5m | 14.4x | 2% | Fast severe burn |
| `page` | 6h | 30m | 6x | 5% | Sustained moderate burn |
| `ticket` | 1d | 2h | 3x | 10% | Slow degradation |
| `ticket` | 3d | 6h | 1x | 10% | Long-running budget drain |

Tune these for the service SLO, traffic volume, business impact, and tolerated detection time. **These numbers do not transfer to low SLOs** (max burn rate = 100/(100−SLO); at 85% SLO a 14.4x burn is mathematically impossible) — see `references/slo-and-thresholds.md` for the math, low-traffic gates, and low-SLO strategies.

## Labels and annotations

Labels are for routing, ownership, grouping, deduplication, and alert identity. Keep them stable and low-cardinality.

Required labels: `alertname`, `severity`, `team`, `service`, `env`. Useful when stable: `cluster`, `namespace`, `component`, `region`.

Avoid high-cardinality or unstable labels in alert identity and routing: pod UID, container ID, request path with IDs, user ID, email, order ID, trace ID, product ID, and raw error messages. When aggregating with `sum by (...)`, preserve the dimensions that routing needs (`service`, `cluster`, `env`) — dropping them breaks grouping and ownership routing.

Annotations are for human context: `summary`, `description`, `impact`, `action`, `dashboard_url`, `runbook_url`. Full payload contract, naming conventions, and examples: `references/payloads-and-runbooks.md`.

## Grouping, routing, and suppression

Group alerts by the unit of action. Good defaults: `alertname`, `service`, `env`, sometimes `cluster` or `region`. Never group by unstable source labels (pod, instance, path, status code) unless responders truly need separate notifications per value. `group_by: ['...']` disables aggregation entirely and is almost never what you want.

Use inhibition so broad parent alerts suppress derivative children (region outage suppresses per-node unreachable alerts). Use silences, mute timings, or maintenance windows for planned work — suppress notifications, never disable rule evaluation, so visibility is preserved. Route first to the owning team; escalate by time and role, never "anyone online".

Concrete Alertmanager/Grafana/Datadog configs and platform gotchas: `references/routing-and-platforms.md`.

## Alert payload quality bar

Before accepting an alert, verify it answers: What is broken? Who owns it? Who is affected? Page, ticket, or info? What exact evidence triggered it? How long has it been true? What is the likely impact? What should the responder do first? Where is the dashboard? Where is the runbook? How will related alerts be grouped or inhibited?

Reject or downgrade alerts that only say a metric is unusual, lack an owner, lack a runbook, trigger on a single blip, or require no human action.

## Infrastructure-specific guidance

Kubernetes pages should focus on control-plane SLO burn, API availability, etcd quorum/leader health, fleet-level kubelet reachability, critical workload replica shortfall, DNS availability, and persistent cluster-wide scheduling or networking failure. Per-pod crash loops, rollout skew, and resource pressure are usually tickets unless they affect critical workloads or user journeys.

VM and host alerts should focus on predicted filesystem/inode exhaustion, persistent memory pressure (use `MemAvailable`, never "free" memory), host unreachability with service impact, disk I/O saturation, CPU steal, clock skew, conntrack exhaustion, and hardware faults (EDAC, thermal, RAID). Raw CPU percentage is rarely a good page by itself.

Network alerts should combine black-box path checks with device/interface telemetry. Page on user-path loss confirmed from multiple vantage points, DNS failure, load balancer health collapse, BGP/session loss or flapping that removes production capacity, or packet loss/latency breaching service objectives.

Concrete alert matrices with upstream-default thresholds, PromQL, and remediation notes: `references/infrastructure-catalog.md`.

## Review cadence

Treat alert rules as production code:

- Store rules in version control; lint and unit-test them (`promtool check rules --lint=all`, `promtool test rules`).
- Canary new rules to a non-paging receiver before promoting them; compare would-have-fired vs did-fire.
- Metamonitor the alerting path itself, end to end (source → Prometheus → Alertmanager → notification endpoint).
- Track: pages per on-call shift (target ≤2 incidents/shift), alert-to-incident ratio (drive noisy rules toward 1:1), false-positive rate, MTTA, MTTR, long-muted or recipient-less monitors.
- Review new pages with service owners and on-call engineers; delete or downgrade noisy alerts after incidents and on-call reviews. "Sometimes useful" is not a reason to keep a noisy page.

Testing, CI pipeline, and canary details: `references/queries-and-testing.md`. KPI definitions and on-call sustainability: `references/payloads-and-runbooks.md`.

The best alerting system is not the one with the most rules. It is the one responders trust.
