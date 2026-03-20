# netbox-zone-labeler

Labels Kubernetes nodes with `topology.kubernetes.io/zone` based on rack data from NetBox.

Supports both bare metal devices (`dcim/devices`) and virtual machines (`virtualization/virtual-machines`). For VMs, the zone is resolved via the host device's rack.

## Configuration

| Env variable | Required | Description |
|---|---|---|
| `NETBOX_URL` | yes | NetBox API base URL |
| `NETBOX_TOKEN` | yes | NetBox API token |
| `EXCLUDE_NODE_ROLES` | no | Comma-separated list of node roles to skip (e.g. `master,control-plane`) |

## Release process

Releases are fully automated via the `release.yaml` GitHub Actions workflow. To create a new release:

1. Merge all changes into `main`
2. Create and push a semver tag:
   ```bash
   git checkout main
   git pull origin main
   git tag v0.4.0
   git push origin v0.4.0
   ```
3. The workflow will automatically:
   - Build and push the Docker image to ECR (`515260921971.dkr.ecr.eu-central-1.amazonaws.com/netbox-zone-labeler`)
   - Package and push the Helm chart to ECR OCI registry (`helm-charts/netbox-zone-labeler`)
   - The chart version and appVersion are set from the git tag (the `Chart.yaml` values are overwritten at build time)
4. Update the chart version in the [fluxcd repo](https://github.com/maestra-io/fluxcd) at `base/netbox-zone-labeler/helm-release.yaml`

## Development

```bash
# Run tests
go test ./...

# Lint
go vet ./...
```
