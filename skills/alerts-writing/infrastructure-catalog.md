# Infrastructure Alert Catalog

Supports `SKILL.md`. Concrete alert recommendations with thresholds for Kubernetes, hosts/VMs, and networking. Thresholds marked "upstream" match kube-prometheus / node-exporter mixin defaults; others are conservative starting points — tune to baselines and SLO after several weeks of data. Prefer starting from upstream mixins (kube-prometheus, node_exporter mixin, coredns mixin) and layering a small number of org-specific rules on top, rather than hand-writing the whole catalog.

## Kubernetes

### Pages

| Alert | Signal | Threshold / notes |
|---|---|---|
| API error-budget burn | `apiserver_request_total` burn-rate recording rules (mixin `KubeAPIErrorBudgetBurn`) | Multi-window 14.4x/1h+5m, 6x/6h+30m for a 99.9% control-plane SLO |
| etcd quorum / leader loss | `etcd_server_has_leader == 0`; even member count (`count(etcd_server_id) % 2 == 0`) | Immediate; quorum loss paralyzes the cluster |
| Kubelet fleet unreachable | `up{job="kubelet"} == 0` across many nodes (`KubeletDown`) | >15m (upstream); fleet-level only — one kubelet is not a page |
| Cluster DNS unavailable | CoreDNS panics/crashloop, SERVFAIL ratio breach | Any panic or repeated crashloop in prod DNS replicas |
| Critical workload replica shortfall | Deployment/STS available < required beyond rollout tolerance | Only for SLO-backing workloads |

### Tickets / warnings

| Alert | Signal | Threshold / notes |
|---|---|---|
| Node not ready | `kube_node_status_condition{condition="Ready",status="true"} == 0` guarded by `kube_node_spec_unschedulable == 0` | >15m (upstream); page only if capacity/SLO impacted |
| Node readiness flapping | `changes(...Ready...[15m]) > 2` | Upstream-style; signals intermittent kubelet/network trouble, not a one-off outage |
| Pod crash looping | `rate(kube_pod_container_status_restarts_total[10m]) * 300 > 0` sustained 10–15m | Ticket while SLO holds; exclude Jobs; transient crashes during rollouts are normal |
| Deployment replicas mismatch | `spec_replicas != status_replicas_available` **and** `changes(kube_deployment_status_replicas_updated[5m]) == 0` | >15m (upstream); the rollout-stall guard prevents paging on active rollouts |
| Too many pods per node | `kubelet_running_pod_count > 100` | 15m; hard support boundary is 110 pods/node |
| Pod pending/unknown | `kube_pod_status_phase{phase=~"Pending|Unknown"}` >15m | Check scheduling blockers, PVCs, taints, quota |
| CPU throttling high | `container_cpu_cfs_throttled_seconds_total` ratio | Correlation signal only (upstream inhibits it by default); never a page on its own |
| Overcommit | `KubeCPUOvercommit` / `KubeMemoryOvercommit` mixin rules | Warn when requests exceed N−1 node-failure capacity |
| OOMKilled containers | restart deltas + last-terminated-reason from kube-state-metrics | Ticket; page only if repeated and SLO-impacting |

### Control-plane depth (beyond the API server)

- **etcd commit latency**: high `etcd_disk_backend_commit_duration_seconds` translates directly to API-server slowness — writes can't be acked until fsync completes.
- **etcd leader churn**: rising `etcd_server_leader_changes_seen_total` indicates network latency between control-plane nodes, CPU starvation, or disk saturation causing missed heartbeats.
- **etcd DB size**: `etcd_mvcc_db_total_size_in_bytes` approaching the 2GB/8GB quota is a preventive page — hitting quota takes the control plane down.
- **API server latency**: segment `apiserver_request_duration_seconds_bucket` by verb and resource; LIST-all-namespaces is naturally slow, GET-one-pod is not — one blended threshold produces noise.
- **Admission webhooks**: `apiserver_admission_webhook_admission_duration_seconds` isolates policy engines (Gatekeeper, Kyverno) silently bottlenecking every write.
- **Scheduler / controller-manager**: `scheduler_pending_pods`, binding duration, and controller work-queue depth are early warnings of cloud-integration failures or API rate-limit exhaustion.

Managed clusters (EKS/GKE/AKS) don't expose full raw control-plane metrics — pair Prometheus rules with provider-native telemetry (CloudWatch/Container Insights, Cloud Monitoring, Azure Monitor) and route provider-owned failures differently from customer-owned ones.

## Hosts and VMs

### Pages

| Alert | Signal | Threshold / notes |
|---|---|---|
| Filesystem will fill soon | `predict_linear(node_filesystem_avail_bytes[6h], 4*3600) < 0` and low absolute floor | Upstream mixin: warn if full within 24h and <40%; critical within 4h and <20%. Exclude `tmpfs|overlay|fuse.*|nfs|cifs` |
| Inode exhaustion imminent | `node_filesystem_files_free` trajectory / low ratio | Same severity as block exhaustion — zero inodes = can't create files, identical blast radius, usually unmonitored |
| Conntrack near limit | `node_nf_conntrack_entries / node_nf_conntrack_entries_limit > 0.75` | Upstream; exhaustion silently drops new connections |
| Clock skew | NTP offset large enough to break TLS/auth/consensus | Hard-failure predictor |
| Hardware fault removing redundancy | `node_md_disks` failed state; `node_edac_uncorrectable_errors_total` > 0 | RAID member loss or uncorrectable memory errors |
| Kernel panic / node problem | Node Problem Detector conditions/events | Any confirmed panic; drain and collect evidence |

### Tickets / warnings

| Alert | Signal | Threshold / notes |
|---|---|---|
| CPU saturation persistent | `1 - rate(node_cpu_seconds_total{mode="idle"}[5m])` > 0.9 for 15–30m | Upstream-style; almost never a page by itself |
| CPU steal (noisy neighbor) | `avg without(cpu) (rate(node_cpu_seconds_total{mode="steal"}[5m])) * 100 > 10` | 5–10% sustained = hypervisor contention; fix is migration/instance-tier change, not profiling |
| I/O wait / disk saturation | `rate(node_disk_io_time_weighted_seconds_total[5m]) > 10` for 30m | Upstream; correlate with queue depth and batch/backup jobs |
| Memory pressure | `node_memory_MemAvailable_bytes` low ratio for 15m | **Never alert on "free" memory** — Linux uses free RAM for page cache by design; near-zero free is normal |
| Thrashing precursor | `rate(node_vmstat_pgmajfault[5m])` sustained spike (e.g. >1000/s) | Major-fault storms precede OOM-killer action |
| Swap filling rapidly | swap used >50–80% with sustained swap-in | Tune per host class; some swap is normal |
| NIC errors | `rate(node_network_receive_errs_total[2m]) / rate(node_network_receive_packets_total[2m]) > 0.01` (and transmit) | Upstream: >1% for 1h; points to cabling/MTU/duplex/offload/hypervisor |
| Hardware degradation trend | `node_hwmon_temp_celsius` vs `_max_celsius`; `node_edac_correctable_errors_total` rising | Correctable-error growth is a leading DIMM-failure indicator — evacuate before the uncorrectable one panics the host |

Node-exporter hygiene: default installs enable dozens of collectors; disable what you don't alert or diagnose on (`--collector.disable-defaults` + explicit enables) to control cardinality and cost.

## Network, DNS, BGP, load balancers

### Pages

| Alert | Signal | Threshold / notes |
|---|---|---|
| End-to-end probe failure | `probe_success == 0` (blackbox_exporter) | 2–5m, confirmed from ≥2 vantage points; classify DNS/TCP/TLS/HTTP failure stage before paging app teams |
| DNS failure ratio | `coredns_dns_responses_total{rcode="SERVFAIL"}` ratio; p95 of `coredns_dns_request_duration_seconds` over objective | SERVFAIL spike = CoreDNS or upstream forwarder failing. NXDOMAIN spikes are usually app misconfig → ticket |
| LB no healthy backends | AWS `HealthyHostCount`/`UnHealthyHostCount`, GCP backend health, Azure health probe status | Page when healthy < quorum on production services |
| LB rejection / port exhaustion | AWS NLB `RejectedFlowCount`, `PortAllocationErrorCount`; Azure data-path availability | Any sustained non-zero on critical LBs — this is dropped traffic |
| BGP session down on production path | `cilium_bgp_session_state == 0` / `bgpPeerState != established(6)` | >3m for core peering; a single redundant peer down is a ticket |
| BGP flapping | `changes(metallb_bgp_session_up[10m]) > 4` (or equivalent) | Oscillation destabilizes routing tables before hard loss — catch it early |

### Tickets / warnings

| Alert | Signal | Threshold / notes |
|---|---|---|
| Path latency high | `probe_duration_seconds` / latency histograms | Warn at p95 over SLO or 2x geographic baseline — 150ms EU↔AU is physics, 150ms intra-AZ is congestion |
| Packet loss | probe loss / retransmit-derived | Warn >1% 5–10m; critical >5% on critical paths; TCP absorbs isolated drops — alert only on sustained loss |
| Interface errors/discards | `ifInErrors`/`ifOutErrors` (IF-MIB) or node-exporter equivalents | >1% of packets or sustained baseline deviation |
| Interface oper-status down | `ifOperStatus != up` | Page only for core/uplinks or failure-domain collapse |
| Route count anomalies | Cilium `advertised_routes` / `received_routes` deltas | Sudden zero or strong baseline deviation → verify intended withdrawal vs broken reconciliation |
| Bandwidth saturation | `node_network_receive_bytes_total` vs `node_network_speed_bytes` | >80–90% of physical link speed sustained; never static byte limits |
| Cilium eBPF map pressure | `cilium_bpf_map_pressure` approaching 1.0 | Full maps drop new connections indiscriminately — escalate to page near 1.0 |
| Cilium datapath drops | `cilium_drop_count_total` excluding `Policy denied` | Policy drops are expected in zero-trust setups; alert on invalid-state/missing-identity/datapath-error reasons |

Jitter matters for real-time workloads (VoIP/video): persistent latency variance indicates congested intermediate buffers even when average latency looks fine.
