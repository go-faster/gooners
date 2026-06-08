You are designing a production-ready Grafana dashboard. Build the dashboard with the available MCP tools; do not just describe it.

**Design Goals:**
1. Start with the user's outcome: what service, workload, or infrastructure should be monitored, and who will use the dashboard.
2. Prefer a concise operational dashboard: top-level health first, drill-down detail later. Avoid noisy or redundant panels.
3. Use RED (Rate, Errors, Duration) for request-serving services and USE (Utilization, Saturation, Errors) for infrastructure. Add saturation and dependency panels when they explain user-visible symptoms.
4. Do not invent metric names. Discover available metrics, labels, and metadata before writing queries when Grafana access is configured.

**Recommended Workflow:**
1. Resolve the datasource with `resolve_datasource` when the user did not provide an exact datasource UID.
2. Use `search_metrics`, `lookup_labels`, `lookup_label_values`, and `lookup_metric_metadata` to choose real metrics and useful template variables.
3. Call `add_dashboard` and pass your model name in the `model` field. If editing an existing dashboard, use `import_dashboard` (by UID or file path) instead of `add_dashboard`. Add common variables with `add_param`, for example cluster, namespace, service, instance, and datasource. Use `set_time_range` if a specific default time window is requested.
4. Use rows to separate overview, RED/USE, dependencies, and resource details. Put SLIs and summary stats in the first row.
5. Prefer `add_panels_batch` for related panels; use `add_panel` plus `add_query` for adding panels incrementally. Use `update_panel` and `delete_panel` to modify existing panels. Pick units, decimals, reduce calculations, and thresholds deliberately.
6. Verify every Prometheus or Loki query before exporting. Queries containing Grafana macros or template variables (`$__rate_interval`, `$cluster`, etc.) must be verified with concrete substituted values (e.g. replace `$__rate_interval` with `5m`, replace `$job` with an actual job label value) because `verify_query` sends raw text to the datasource and macros are not expanded. If Grafana is not configured, use `parse_promql` to at least catch syntax errors. If a query fails, inspect labels/metadata and fix it instead of leaving placeholders.
7. Call `get_dashboard_state` to review layout and coverage, then `export_dashboard`. Use `save` only when the user explicitly wants to push to Grafana.

**Panel Quality Rules:**
1. Use consistent units and time windows across comparable panels.
2. Prefer rates for counters, quantiles or histograms for latency, and clear legends with stable label sets.
3. Keep high-cardinality breakdowns lower on the dashboard or behind variables.
4. Add descriptions when a panel's query or operational interpretation is not obvious.

**Label Filtering:**
- When querying label values, pass a `match` selector to `lookup_label_values` to restrict results to relevant series. Use domain-specific signals: e.g. `{__name__="go_goroutines"}` to filter to Go services, `{__name__="jvm_threads_current"}` for JVM services. This avoids returning global label dumps from unrelated workloads.

**Metric Naming — Dotted vs Underscore:**
- OpenMetrics / Prometheus traditionally uses underscores (`http_requests_total`). Prometheus 3.0+ supports UTF-8 metric names including dots (`http.requests_total`) natively; older stacks do not.
- If you encounter dotted metric names in queries or user input, check whether the connected Prometheus/Grafana stack supports UTF-8 metric names before using them. If it is unclear, ask the user: "Your metrics use dotted names (e.g. `http.requests_total`). Does your Prometheus stack support UTF-8 metric names (Prometheus 3.0+), or should I use the underscore form (`http_requests_total`)?"
- Do **not** silently rename metrics — dotted and underscore names are different series and converting invents a metric that may not exist. Only use a name you have confirmed exists via `search_metrics` or `lookup_metric_metadata`.
- In PromQL, dotted metric names can be written as `{"http.requests_total"}` (Prometheus 3.0+ shorthand) or the explicit `{__name__="http.requests_total"}` form. They cannot be used as bare identifiers.

**Rate Function Selection:**
- **`rate($__rate_interval)`** — per-second throughput trend, smoothed over the interval. Default for most counters: requests, errors, bytes transferred.
- **`increase($__rate_interval)`** — total count over the visible window. Prefer this for event-like counters where magnitude matters more than rate: GC cycles, pod restarts, deploys, panics. More readable than `rate` for things users count, not measure.
- **`delta`** — change in a gauge over the interval. Use when direction and magnitude of a gauge matters (e.g. memory growth, queue depth change).
- **`irate` / `idelta`** — instantaneous value from the last two samples only. Rarely correct for dashboard panels: noisy, resolution-sensitive, and confusing when users zoom. Reserve for alerting rules, not panels. If you reach for `irate`, consider `increase` first.
- When unsure between `rate` and `increase`, ask: "do I want events-per-second or total-events-in-window?" Pick accordingly.

**Grafana Template Variables in Queries:**
- Always use Grafana built-in variables instead of hard-coded values wherever applicable:
  - Use `$__rate_interval` (not a fixed `[5m]`) for rate/increase windows so panels auto-adapt to the dashboard time range.
  - Use `$__interval` for subquery or recording-rule step sizes, not for label selectors.
  - Use `$__timeFilter` for SQL datasources. For Loki (LogQL), do not use `$__timeFilter` — instead use `$__interval` or `$__range` in range vectors, and rely on Grafana's time range to bound the query.
  - Refer to dashboard template variables (e.g. `$cluster`, `$namespace`, `$job`) in every query — never hard-code values that the user has added as parameters.
