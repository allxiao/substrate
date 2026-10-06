# Azure adapter validation

Runs the actual Go Azure Blob and OCI-cache implementations inside the supplied
AKS/VNet using projected Workload Identity. No account keys, SAS, registry
passwords, or database passwords are used.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go -C tools/validate-azure build -o .bin/azure-validate .
docker build -t "$ACR/azure-validate:$VERSION" tools/validate-azure
docker push "$ACR/azure-validate:$VERSION"
```

Set `VERIFY_IMAGE` to the pushed digest, `VERIFY_MODE` to `storage` or
`postgres`, `VERIFY_SERVICE_ACCOUNT` to the matching federated ServiceAccount,
and `VERIFY_POD_NAME` to a unique test name. Render `pod.yaml.tmpl` with
`envsubst` and apply it to the intended development cluster. The included
noncredential endpoint settings target `menxiao-substrate-1005` only.

The storage mode verifies streaming upload/download, server-side block copy
with OAuth source authorization, paginated listing and idempotent deletion. It
uses a unique `verification/<uuid>/` prefix and deletes only its own objects.
The same mode uploads a HOME-only tar through the existing zstd helpers,
commits its manifest last, downloads/extracts it, verifies history, and checks
that an unrelated rootfs sentinel was not captured.
PostgreSQL mode validates fresh token-authenticated connections, verified TLS,
and runtime/owner role selection without changing SQL data.

For image mode, use `image-pod.yaml.tmpl` on each target node. Set `NODE_NAME`,
`VERIFY_POD_NAME`, `VERIFY_IMAGE` and `DATA_AGENT_IMAGE`. The dedicated
`substrate-acr` ServiceAccount needs a federated identity with only `AcrPull` on
the supplied ACR. The probe fills the custom cache at
`/var/lib/ate/image-cache`, not containerd's image store. Do not run it against
a cache concurrently owned by a running atelet; use atelet's authenticated
PreloadImage RPC when Substrate is installed.

Logs contain digests, node names and timing, never tokens. Timings describe
image preparation only, not full Actor activation. New federation bindings may
take time to propagate; retain and inspect a failed probe before retrying it.

These probes deliberately do not emulate the missing Kubernetes certificate
APIs. They are not native Substrate Actor lifecycle verification.

## Native continuation commands

`--mode=upload-assets --asset-dir=/assets` stages the four pinned amd64 runtime
files through Blob WI, checks each source SHA-256 and reads back the upload to
verify it. `assets.Dockerfile` accepts a BuildKit context named `assets` holding
the output of `ARCH=amd64 hack/microvm-assets/assemble.sh`. Asset object names
include the hash; `sandboxconfig-azure-data-dev.yaml` references those exact
private Blob locations.

`--mode=inspect-snapshot --snapshot-uri=azblob://...` reads a real Actor snapshot
without changing it. It requires exactly the HOME Data tar and manifest,
verifies scope and Actor UID, extracts the archive and validates the historical
query records. It must run with Blob WI inside the private network.

`--mode=drain-worker --worker=<worker-name> --kubeconfig=<dedicated-path>` calls
the native authenticated DrainWorker API. Only use it on an empty verification
worker to force cross-node placement; replace the drained empty Pod afterwards
to restore capacity. It is not a database mutation or a certificate bypass.
