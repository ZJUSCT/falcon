# falcon Helm chart

Deploys the Falcon stack into a single namespace (one full stack per
namespace: controller, admin web UI, metrics, mirrorz/ui HTTPRoutes).

Documentation:

- Project README: <https://github.com/ZJUSCT/falcon>

## ZFS dashboard

Enable the optional Grafana sidecar ConfigMap:

```yaml
zfsAgent:
  dashboard:
    enabled: true
    datasourceUid: prometheus # Use the UID of your Prometheus/Mimir datasource.
    labels:
      grafana_dashboard: "1"
    annotations:
      k8s-sidecar-target-directory: /tmp/dashboards/Falcon
```

The ConfigMap is installed in the release namespace. Configure Grafana's
dashboard sidecar to watch that namespace and these labels; adapt the folder
annotation to its configuration. The dashboard UID and filename are stable
per release/namespace, so multiple releases do not overwrite one another.
Dashboard provisioning can be enabled independently of the agent DaemonSet.

The dashboard selects all hosts by default. Dataset IOPS/bandwidth and ARC
memory are stacked by host. ARC hit ratios have independent host lines and a
lookup-weighted aggregate. Disk panels show each whole ZFS backing disk's
read/write latency independently, labeled by host and device, with no host
or cluster aggregation. Legend Max is the peak sampled interval mean, not the
slowest individual request. Idle disks have no latency sample.

Required metrics in the selected datasource:

- zfs-agent's OTLP dataset and ARC counters (`zfs_dataset_*`, `zfs_arc_*`),
  with the service job ending in `/falcon-zfs-agent`.
- node-exporter (`job="node-exporter"`): disk counters, `node_uname_info`, and
  `node_disk_filesystem_info{type="zfs_member"}` from readable host udev data.
  Its `nodename` must match zfs-agent's `node` label.

Node-exporter's default diskstats filter hides some member partitions, so
override it with `--collector.diskstats.device-exclude=^(z?ram|loop|fd)[0-9]+$`
to expose their metadata. The queries map sd/hd/vd/xvd and NVMe/MMC/MD member
partitions to whole devices, deduplicate them, and use only whole-device I/O.
Dedicated ZFS disks are assumed: shared disks include other workloads, and
exported pools with remaining ZFS signatures are still discoverable.

Keep partition metadata but drop partition I/O samples during ingestion to
avoid double counting in other dashboards. For a Prometheus ServiceMonitor:

```yaml
metricRelabelings:
  - sourceLabels: [__name__, device]
    regex: 'node_disk_(read.*|writ.*|io.*|discard.*|flush.*);((sd|hd|vd|xvd)[a-z]+[0-9]+|.+[0-9]p[0-9]+)'
    action: drop
```

Falcon does not install node-exporter or Grafana. Pool/vdev metrics formerly
collected through `zpool iostat` are no longer emitted by zfs-agent; use the
dataset and node-exporter views instead. ZFS usage/snapshot reporting is
unaffected. The dashboard contains no site-specific credentials or links.
