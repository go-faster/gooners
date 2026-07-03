# Routing, Grouping, Suppression, and Platform Specifics

Supports `SKILL.md`. Read this when configuring Alertmanager, Grafana notification policies, or Datadog monitors, or when mapping a design across platforms.

## Design primitives are the same everywhere

| Capability | Prometheus Alertmanager | Grafana Alerting | Datadog Monitors |
|---|---|---|---|
| Grouping | `group_by`, `group_wait`, `group_interval` | Label grouping + notification timings | Simple vs Multi Alert; `notify_by` |
| Deduplication | Built in at Alertmanager layer | Grouped notifications via policy tree | Aggregation and notification grouping |
| Suppression | Silences, inhibition, mute/active time intervals | Silences, mute timings | Downtimes |
| Routing | Route tree + matchers | Notification policy tree + label matching | Notification rules + tags |
| Escalation | Downstream receiver integrations | IRM / contact points / policy tree | `renotify_*`, `escalation_message`, On-Call |
| Instance identity | Alert label set | Label set uniquely identifies instance | Query groups + tags |
| Maintenance | Suppress notifications, keep evaluating | Same | Same |

Whatever the platform: Prometheus/rules decide **what** fires; the routing layer decides **how, when, and to whom**.

## Alertmanager reference config

```yaml
route:
  receiver: default
  group_by: ['alertname', 'service', 'env', 'cluster']
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - matchers: [severity="page"]
      receiver: pager
    - matchers: [severity="ticket"]
      receiver: ticket-queue
    - matchers: [severity="info"]
      receiver: low-noise-channel
    - matchers: [team="networking"]
      receiver: netops-chat
      continue: true                     # also match later routes
    - matchers: [service="batch"]
      receiver: batch-daytime
      mute_time_intervals: [offhours]

inhibit_rules:
  - source_matchers: [severity="page"]
    target_matchers: [severity=~"ticket|info"]
    equal: ['service', 'env', 'cluster']
  - source_matchers: [alertname="ClusterDown"]
    target_matchers: [severity=~"page|ticket"]
    equal: ['cluster']

time_intervals:
  - name: offhours
    time_intervals:
      - weekdays: ['monday:friday']
        times:
          - {start_time: '18:00', end_time: '24:00'}
          - {start_time: '00:00', end_time: '09:00'}
        location: 'UTC'
```

Rules of thumb:

- Group by the **unit of action**. `['alertname','service','env','cluster']` is a durable default; for physical-topology incidents `['datacenter','rack']` turns hundreds of pages into one.
- `group_by: ['...']` disables aggregation entirely — a documented footgun, almost never intended.
- Inhibition encodes incident topology: parent outage present → mute derivative children (`equal` on the shared scope labels). Warning duplicates of an already-firing critical should be inhibited by default.
- Maintenance = mute/active time intervals or silences. **Never disable rule evaluation** — you lose the record of what happened during the window and risk forgetting to re-enable.
- Escalation is time- and role-based, in configuration: primary on-call → secondary after unacked SLA (e.g. 15m) → incident commander if impact persists (e.g. 30m). Never "whoever is online".
- `repeat_interval` prevents both silence-after-first-page and page-spam; 4h is a sane default for pages.

## Grafana Alerting specifics

Grafana implements the Prometheus model (one rule → many alert instances via label sets) plus its own semantics. Cautions that bite in practice:

- **The label set uniquely identifies an alert instance.** Two rules producing identical label sets collide and one instance is silently discarded.
- **No Data and Error are explicit states** — configure them intentionally per rule (alerting vs OK vs keep-last) rather than accepting defaults, especially for sparse metrics.
- **Template variables are not supported in alert queries** (rules evaluate in a backend context, not a dashboard).
- **Provisioning overwrites the notification policy tree atomically** — treat the tree as a single IaC object.
- **Export format ≠ provisioning API format.** Export a known-good object from the UI/API, normalize it into IaC, and evolve it in pull requests — don't hand-author complex `data.model` payloads (they're datasource- and version-specific).
- Mute timings = recurring suppression; silences = fixed-window suppression. Both suppress notifications without stopping evaluation.

## Datadog specifics

- Prefer **one multi-alert (grouped) monitor** over many near-duplicate simple monitors; `notify_by` controls notification granularity.
- Flap/noise controls map as: `for`-equivalent = evaluation window + `new_group_delay` (avoid startup spikes on new groups); delayed cloud metrics = `evaluation_delay`; sparse metrics = `require_full_window: false`; hysteresis = native recovery thresholds; escalation = `renotify_interval`, `renotify_statuses`, `renotify_occurrences`, `escalation_message`.
- Governance: manage monitors via API/Terraform; Audit Trail records changes; **Monitor Quality** flags long-muted monitors, missing recipients, missing delay, high alert volume, stuck states — review these regularly, since Datadog has no native rule unit tests.

Monitor template:

```json
{
  "name": "Checkout error rate high",
  "type": "query alert",
  "query": "avg(last_5m):avg:app.http.error_rate{service:checkout,env:prod} by {service} > 0.02",
  "message": "## What\n5xx ratio above 2%.\n## Impact\nCustomer requests may be failing.\n## First actions\n- Open checkout dashboard\n- Check recent deploys\n## Runbook\nhttps://runbooks.example.com/checkout/high-error-rate\n\n@pagerduty-payments",
  "tags": ["team:payments", "service:checkout", "env:prod", "severity:page"],
  "options": {
    "thresholds": {"critical": 0.02, "warning": 0.01},
    "notify_by": ["service"],
    "new_group_delay": 60,
    "evaluation_delay": 60,
    "require_full_window": false,
    "notify_no_data": false,
    "renotify_interval": 30,
    "renotify_statuses": ["Alert", "No Data"],
    "escalation_message": "Still firing after 30m. Escalate to @payments-secondary.",
    "include_tags": true
  }
}
```

## Prometheus rule template

```yaml
groups:
  - name: <service>.alerts
    interval: 30s
    rules:
      - alert: <ServiceConditionSeverity>
        expr: |
          <promql expression>
        for: 5m
        keep_firing_for: 5m
        labels:
          severity: page|ticket|info
          team: <owning-team>
          service: <service-name>
          env: prod
          component: <optional-component>
        annotations:
          summary: "<what is broken>"
          description: "<current condition, threshold, duration, affected scope>"
          impact: "<expected user/platform impact>"
          action: "<first diagnostic or mitigation step>"
          dashboard_url: "<dashboard>"
          runbook_url: "<runbook>"
```

## Intent → platform mapping

| Intent | Prometheus | Grafana | Datadog |
|---|---|---|---|
| Target down | `up == 0` | Same query, Grafana-managed rule | Host/service-check monitor |
| Error-rate page | `sum(rate(errors))/sum(rate(total))` | Same PromQL or expression + threshold | Metric monitor on ratio |
| Latency page | Histogram percentile / bucket SLI | PromQL or Flux + threshold | Metric/APM latency monitor |
| Missing signal | `absent()` or blackbox probe | No Data state | `notify_no_data` + `no_data_timeframe`, synthetics |
| Maintenance | `time_intervals` / silences | Mute timings / silences | Downtimes |
| Fan-out suppression | Inhibition | Policy grouping / silences | Composite monitors, notification rules |
| SLO alerting | Recording rules + multi-window burn | SLO features / PromQL | Native SLO burn-rate monitors |
