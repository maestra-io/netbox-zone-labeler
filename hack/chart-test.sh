#!/usr/bin/env bash
# Renders the chart the way CI and the release pipeline do, validates the
# manifests against the Kubernetes schema and asserts the invariants that
# matter operationally. Run from the repo root; needs helm, yq, kubeconform.
set -euo pipefail

CHART=charts/netbox-zone-labeler
K8S_VERSION=${K8S_VERSION:-1.34.0}
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

helm lint --strict "$CHART"

render() { # name, extra helm args...
  local name=$1; shift
  helm template netbox-zone-labeler "$CHART" --namespace netbox-zone-labeler --kube-version "$K8S_VERSION" "$@" > "$OUT/$name.yaml"
  kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version "$K8S_VERSION" "$OUT/$name.yaml"
}

render default
render full --set metrics.vmPodScrape.enabled=true --set-string 'excludeNodeRoles=master\,control-plane' --set priorityClassName=production-high

q() { yq eval "select(.kind == \"$1\") | $2" "$OUT/$3.yaml"; }

# Selector is immutable on a Deployment: it must stay exactly what 0.x shipped.
[ "$(q Deployment '.spec.selector.matchLabels | to_entries | length' default)" = 1 ] || fail "selector must carry exactly one label"
[ "$(q Deployment '.spec.selector.matchLabels."app.kubernetes.io/name"' default)" = netbox-zone-labeler ] || fail "selector label changed"
[ "$(q Deployment '.spec.strategy.type' default)" = Recreate ] || fail "strategy must be Recreate"

# tbot is a native sidecar gated on its own readiness.
[ "$(q Deployment '.spec.template.spec.initContainers[0].name' default)" = tbot ] || fail "tbot must be the init container"
[ "$(q Deployment '.spec.template.spec.initContainers[0].restartPolicy' default)" = Always ] || fail "tbot must be a native sidecar (restartPolicy: Always)"
[ "$(q Deployment '.spec.template.spec.initContainers[0].startupProbe.httpGet.path' default)" = /readyz ] || fail "tbot startup probe must hit /readyz"

# Both containers: nonroot 65532, memory requests == limits (omega VAP).
for c in '.spec.template.spec.initContainers[0]' '.spec.template.spec.containers[0]'; do
  [ "$(q Deployment "$c.securityContext.runAsUser" default)" = 65532 ] || fail "$c must run as 65532"
  [ "$(q Deployment "$c.securityContext.runAsNonRoot" default)" = true ] || fail "$c must set runAsNonRoot"
  req=$(q Deployment "$c.resources.requests.memory" default); lim=$(q Deployment "$c.resources.limits.memory" default)
  [ "$req" = "$lim" ] || fail "$c memory requests ($req) != limits ($lim)"
  [ "$(q Deployment "$c.resources.limits.cpu" default)" = null ] || fail "$c must not set a CPU limit"
done

# The tunnel is loopback-only; the labeler dials the same port.
grep -q 'listen: tcp://127.0.0.1:8080' "$OUT/default.yaml" || fail "tbot tunnel must listen on 127.0.0.1"
grep -q 'diag_addr: 0.0.0.0:3001' "$OUT/default.yaml" || fail "tbot diag_addr missing"
[ "$(q Deployment '.spec.template.spec.containers[0].env[] | select(.name == "NETBOX_URL") | .value' default)" = http://127.0.0.1:8080 ] || fail "NETBOX_URL must point at the loopback tunnel"

# Scrape object only when asked for.
[ -z "$(q VMPodScrape '.metadata.name' default)" ] || fail "VMPodScrape rendered by default"
[ "$(q VMPodScrape '.metadata.name' full)" = netbox-zone-labeler ] || fail "VMPodScrape missing when enabled"
[ "$(q VMPodScrape '.spec.podMetricsEndpoints[0].port' full)" = health ] || fail "VMPodScrape must scrape the health port"

# Env plumbing.
[ "$(q Deployment '.spec.template.spec.containers[0].env[] | select(.name == "EXCLUDE_NODE_ROLES") | .value' full)" = master,control-plane ] || fail "EXCLUDE_NODE_ROLES not rendered"
[ "$(q Deployment '.spec.template.spec.priorityClassName' full)" = production-high ] || fail "priorityClassName not rendered"

echo "chart-test: ok"
