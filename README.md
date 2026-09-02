# netbox-zone-labeler

Sets `topology.kubernetes.io/zone` on every Kubernetes node to the NetBox rack of
the machine it runs on, so zone-aware scheduling (pod topology spread,
anti-affinity, CNPG/Strimzi/Cassandra rack awareness) works on bare-metal
clusters the same way it does in a cloud.

## How it resolves a zone

For a node named `N`:

1. `GET /api/dcim/devices/?name=N` — a physical device: its `rack.name` is the zone.
2. Otherwise `GET /api/virtualization/virtual-machines/?name=N`, then
   `GET /api/dcim/devices/<vm.device.id>/` — a VM inherits the rack of its host.

The rack name is lower-cased and whitespace is collapsed into `-`
(`L130-B14` → `l130-b14`, `Rack 42` → `rack-42`), then validated as a label value.

| outcome | what happens |
|---|---|
| rack found, label differs | node is patched (`MergePatch` of `metadata.labels`) |
| rack found, label equal | nothing |
| not in NetBox / no rack / VM without host | logged at `info`, retried on the next full pass only |
| two devices or VMs share the name | logged at `error`, `errors_total{reason="ambiguous"}`, retried on the next full pass |
| rack name is not a valid label value | logged at `error`, `errors_total{reason="invalid_label"}`, retried on the next full pass |
| NetBox 5xx / 429 / network error | retried inside the client (3× exponential backoff), then re-queued with the controller's rate limiter |
| node patch fails | re-queued with the controller's rate limiter |

Nodes carrying `node-role.kubernetes.io/<role>` for any role in
`EXCLUDE_NODE_ROLES` are ignored entirely.

## How it runs

A metadata-only informer on `nodes` (name and labels only, never `status`)
feeds a rate-limited work queue with one worker:

- a node is queued when it appears and when its **labels** change (kubelet
  status heartbeats do not count);
- every `RECONCILE_PERIOD` (default 30m) every node is queued again, which is
  how a rack move in NetBox or a node newly added to NetBox is picked up;
- a single replica, no leader election: the chart deploys it with
  `strategy: Recreate`.

Timings: a new node is labeled within seconds of joining; a change in NetBox
lands within one `RECONCILE_PERIOD`.

NetBox is reached through a [tbot](https://goteleport.com/docs/machine-workload-identity/)
application tunnel running as a native sidecar in the same pod, listening on
`127.0.0.1:8080`. The labeler waits up to `NETBOX_WAIT` for it at start-up.

## Configuration (environment)

| variable | default | meaning |
|---|---|---|
| `NETBOX_URL` | required | NetBox base URL (the tunnel in-cluster) |
| `NETBOX_TOKEN` | required | NetBox API token |
| `NETBOX_TIMEOUT` | `10s` | per-request HTTP timeout |
| `NETBOX_WAIT` | `2m` | start-up wait for NetBox; on expiry the labeler starts anyway |
| `EXCLUDE_NODE_ROLES` | empty | comma-separated node roles to skip, e.g. `master,control-plane` |
| `RECONCILE_PERIOD` | `30m` | full pass interval |
| `LISTEN_ADDR` | `:8081` | `/healthz`, `/readyz`, `/metrics` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`; logs are JSON on stderr |
| `DRY_RUN` | `false` | log the patches, apply nothing |
| `KUBECONTEXT` | current context | kubeconfig context to use when running outside the cluster |

`/readyz` is 200 once the informer has listed the nodes. `/healthz` is 503 when
no full pass has been scheduled for three periods (a wedged ticker).

## Metrics

All under `netbox_zone_labeler_`:

| metric | type | meaning |
|---|---|---|
| `nodes_labeled_total` | counter | labels set or changed |
| `errors_total{reason}` | counter | `netbox` (transient, retried), `patch` (retried), `invalid_label`, `ambiguous` |
| `lookup_duration_seconds{result}` | histogram | one rack lookup incl. retries; `found`, `miss`, `error` |
| `nodes_without_zone` | gauge | non-excluded nodes with no zone label right now |
| `last_full_pass_timestamp_seconds` | gauge | when the last full pass was scheduled |
| `queue_depth` | gauge | nodes waiting for a lookup |

A useful alert: `netbox_zone_labeler_nodes_without_zone > 0` for longer than
one `RECONCILE_PERIOD`. Enable scraping with `metrics.vmPodScrape.enabled=true`
in the chart.

## Deployment

The chart in `charts/netbox-zone-labeler` expects:

- a Secret `netbox-zone-labeler` with key `token` in the release namespace
  (in `maestra-io/fluxcd` it is a `VaultStaticSecret` from
  `apps/infrastructure/netbox-zone-labeler`);
- a Teleport bot joinable with the `kubernetes` method using the token named in
  `tbot.joinToken`, holding a role that can access the application
  `tbot.appName`;
- the `image-pull` Secret for ECR (or your own `imagePullSecrets`).

Everything else (RBAC, NetworkPolicy, tbot config) is in the chart. Both
containers run as `65532`, with `requests.memory == limits.memory`.

## Releasing

Push a tag `vX.Y.Z`. The `Release` workflow runs the tests, builds the image,
pushes `X.Y.Z` and `X.Y` to ECR in `us-west-2`, and packages the chart with the
same version and pushes it there too. Since phase 6 of the ECR migration
(`maestra-io/issues-maestra#1354`) `us-west-2` is the only registry written to —
`eu-central-1` is frozen — so the two mirror steps are switched off rather than
copying anything. Then bump `spec.chart.spec.version` in the consuming
`HelmRelease` (`maestra-io/fluxcd/base/netbox-zone-labeler`).

## Local development

```sh
go test -race ./...
hack/chart-test.sh          # helm lint + kubeconform + invariants (needs helm, yq, kubeconform)

# Dry run against a real cluster and NetBox, patches nothing:
tsh proxy app netbox --port 8080 &
DRY_RUN=true NETBOX_URL=http://127.0.0.1:8080 NETBOX_TOKEN=... \
  KUBECONTEXT=teleport.maestra.io-us-omicron-lw-kube-common go run .
```
