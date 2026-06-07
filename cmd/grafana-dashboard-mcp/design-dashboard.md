You are designing a production-ready Grafana dashboard. Build the dashboard with the available MCP tools; do not just describe it.

**Design Goals:**
1. Start with the user's outcome: what service, workload, or infrastructure should be monitored, and who will use the dashboard.
2. Prefer a concise operational dashboard: top-level health first, drill-down detail later. Avoid noisy or redundant panels.
3. Use RED (Rate, Errors, Duration) for request-serving services and USE (Utilization, Saturation, Errors) for infrastructure. Add saturation and dependency panels when they explain user-visible symptoms.
4. Do not invent metric names. Discover available metrics, labels, and metadata before writing queries when Grafana access is configured.

**Recommended Workflow:**
1. Resolve the datasource with `resolve_datasource` when the user did not provide an exact datasource UID.
2. Use `search_metrics`, `lookup_labels`, `lookup_label_values`, and `lookup_metric_metadata` to choose real metrics and useful template variables.
3. Call `add_dashboard` and pass your model name in the `model` field. Add common variables with `add_param`, for example cluster, namespace, service, instance, and datasource.
4. Use rows to separate overview, RED/USE, dependencies, and resource details. Put SLIs and summary stats in the first row.
5. Prefer `add_panels_batch` for related panels; use `add_panel` plus `add_query` for incremental edits. Pick units, decimals, reduce calculations, and thresholds deliberately.
6. Verify every Prometheus or Loki query with `verify_query` before exporting. If a query fails, inspect labels/metadata and fix it instead of leaving placeholders.
7. Call `get_dashboard_state` to review layout and coverage, then `export_dashboard`. Use `save` only when the user explicitly wants to push to Grafana.

**Panel Quality Rules:**
1. Use consistent units and time windows across comparable panels.
2. Prefer rates for counters, quantiles or histograms for latency, and clear legends with stable label sets.
3. Keep high-cardinality breakdowns lower on the dashboard or behind variables.
4. Add descriptions when a panel's query or operational interpretation is not obvious.
