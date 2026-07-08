# csi-reclaim-controller

An **optional** CSI sidecar that deletes the backing storage of
PersistentVolumes using `reclaimPolicy: Retain` once the PV object is deleted.

## Background

Kubernetes supports two reclaim policies for dynamically provisioned volumes:

- **`Delete`** — when the bound PVC is deleted, the
  [external-provisioner](https://github.com/kubernetes-csi/external-provisioner)
  sidecar calls `DeleteVolume()` and removes both the PV and the backing storage.
- **`Retain`** — the PV is kept (moves to `Released`) and the backing storage is
  **never** touched automatically. Even deleting the PV object by hand only
  removes the Kubernetes object; the volume lives on in the storage backend and
  must be cleaned up manually.

This is by design, but some operators want a middle ground: keep `Retain`'s
safety (the volume survives PVC deletion) while making a **deliberate `kubectl
delete pv`** also free the backing storage — no separate backend cleanup step.

`csi-reclaim-controller` provides exactly that, as a component you opt into by
deploying it.

## What it does

**Deploying this controller is the opt-in.** Once it runs, every `Retain` PV of
the connected driver is reclaimed when its PV object is deleted. This is the
whole point: it turns `kubectl delete pv` into a full teardown of the backing
volume for that driver. If you rely on a `Retain` PV being *re-appliable* (delete
the PV object, keep the volume, recreate the PV later), exclude that PV with the
[skip annotation](#coordinating-with-controllers-that-recreate-pvs).

For every PV whose `spec.csi.driver` matches the connected driver **and** whose
`persistentVolumeReclaimPolicy` is `Retain` **and** which is not skip-annotated,
the controller:

1. Adds a finalizer (default `csi-reclaim-controller/<sanitized-driver-name>`)
   so a delete of the PV is intercepted rather than completing immediately.
2. When such a PV is deleted (`deletionTimestamp` set), calls the driver's
   `DeleteVolume()` for `spec.csi.volumeHandle`, using the same deletion-secret
   annotations that external-provisioner records on the PV.
3. Removes the finalizer once `DeleteVolume()` succeeds, letting the PV object
   disappear.

`DeleteVolume()` is idempotent per the CSI spec, so re-deleting an
already-gone volume is a safe no-op.

```
kubectl delete pvc  ──►  (Retain) PV becomes Released, backing volume kept
        │
        ▼
kubectl delete pv   ──►  finalizer holds the PV
                          │
                          ├─ controller calls DeleteVolume() on the driver
                          └─ finalizer removed ──► PV gone, backing volume gone
```

The skip annotation and finalizer default to keys namespaced by the CSI driver
name, so they are unique per driver and don't collide when several drivers run
this sidecar. Both are overridable (`--skip-annotation`, `--finalizer`).

## Safety properties

- **Excludable per PV.** A PV annotated `<driver-name>/skip-reclaim=true` is
  excluded from management entirely — never reclaimed, finalizer released. Use it
  to keep a specific `Retain` volume re-appliable, or to let another controller
  delete-and-recreate a PV in place (see below). The annotation is evaluated
  client-side, so a PV unmarked *while terminating* is still observed and its
  finalizer released rather than being stranded.
- **Only `Retain` PVs of the connected driver** are eligible. `Delete` policy
  PVs, other drivers, and non-CSI volumes are ignored.
- **Bound PVs are never reclaimed.** If a terminating PV is still `Bound`, the
  controller waits (Kubernetes' `pv-protection` finalizer keeps it alive) and
  only proceeds once the PV leaves the `Bound` phase — i.e. after the PVC is
  gone. This matches the intended workflow: delete the PVC first, then the PV.
- **Failures are retried, not swallowed.** If `DeleteVolume()` fails, the
  finalizer stays and the item is requeued with backoff, so the backing volume
  is never orphaned by a transient error.
- **Startup capability check.** The controller refuses to start if the driver
  does not advertise the `CREATE_DELETE_VOLUME` controller capability.

> ⚠️ With this controller deployed, `kubectl delete pv` on any `Retain` volume of
> the driver becomes destructive — it also deletes the backing volume. That is
> the intended behavior. Protect volumes you may want to re-apply later with the
> `<driver-name>/skip-reclaim=true` annotation.

## Coordinating with controllers that recreate PVs

Some controllers delete and recreate a PV in place to change immutable fields —
notably [`linstor-affinity-controller`](https://github.com/piraeusdatastore/linstor-affinity-controller),
which patches a PV to `Retain`, deletes it, and re-applies it (same
`volumeHandle`) to update node affinity. To such a controller the backing volume
must survive the delete; to this one, a deleted `Retain` PV normally means
"reclaim it". Left uncoordinated, an affinity update would race into a
`DeleteVolume()` and destroy live data.

The contract that prevents this is the **`<driver-name>/skip-reclaim=true`
annotation** (e.g. `linstor.csi.linbit.com/skip-reclaim=true`; overridable with
`--skip-annotation`).
Any controller doing an in-place PV recreate must set it *in the same patch that
switches the PV to `Retain`, before deleting the PV*. While the annotation is
present this controller treats the PV as unmanaged: it never calls
`DeleteVolume()` and releases its own finalizer. Because the recreated PV is
applied fresh (without the annotation), normal management resumes automatically.

Setting it together with the `Retain` switch is what makes it race-free: the
annotation is therefore always visible on the PV by the time it carries a
`deletionTimestamp`, even accounting for informer cache lag.

No configuration is needed on either side beyond running a
`linstor-affinity-controller` version that sets the annotation. Because the key
is derived from the driver name, that controller builds it from the LINSTOR CSI
driver name it already knows.

### Importing the keys

So other projects don't hardcode these strings, the annotation and finalizer
keys are exported from a dependency-light package:

```go
import "github.com/piraeusdatastore/csi-reclaim-controller/pkg/reclaim"

skip := reclaim.SkipAnnotation(pv.Spec.CSI.Driver) // "<driver>/skip-reclaim"
fin  := reclaim.Finalizer(pv.Spec.CSI.Driver)      // "csi-reclaim-controller/<sanitized-driver>"
if reclaim.ShouldSkip(pv.Annotations, pv.Spec.CSI.Driver) { /* excluded */ }
```

The package has no Kubernetes or third-party dependencies.

## Flags

Component-specific flags:

The CSI driver name is always discovered from the connected CSI endpoint via
`GetPluginInfo`; only PVs whose `spec.csi.driver` matches it are managed.

| Flag | Default | Description |
| --- | --- | --- |
| `--skip-annotation` | `<driver>/skip-reclaim` | PV annotation that, when true, excludes the PV from reclaiming. |
| `--finalizer` | `csi-reclaim-controller/<sanitized-driver>` | Finalizer placed on managed PVs. |
| `--timeout` | `60s` | Timeout for each CSI `DeleteVolume`/`Probe` call. |
| `--resync-period` | `10m` | PV informer resync interval. |
| `--worker-threads` | `5` | Concurrent reconcile workers. |

Common CSI-sidecar flags (from `csi-lib-utils/standardflags`) also apply, including:

| Flag | Default | Description |
| --- | --- | --- |
| `--csi-address` | `/run/csi/socket` | Path to the driver's CSI UNIX socket. |
| `--kubeconfig` | *(in-cluster)* | Kubeconfig path for out-of-cluster runs. |
| `--leader-election` | `false` | Enable leader election for HA deployments. |
| `--leader-election-namespace` | *(pod ns)* | Namespace for the leader-election Lease. |
| `--http-endpoint` | *(disabled)* | Address (e.g. `:8080`) serving `/metrics` and the leader-election health check at `/healthz/leader-election`. |
| `--metrics-path` | `/metrics` | Path for Prometheus metrics. |
| `--kube-api-qps` / `--kube-api-burst` | `5` / `10` | API server client rate limits. |
| `--version` | | Print version and exit. |

`klog` flags such as `--v` are also available.

## Deploying

Released, multi-arch images are published to
[`quay.io/piraeusdatastore/csi-reclaim-controller`](https://quay.io/repository/piraeusdatastore/csi-reclaim-controller).

Run it as an extra sidecar container in your CSI driver's controller Pod so it
shares the driver's CSI socket volume — the same emptyDir the driver and the
other sidecars (external-provisioner, etc.) mount at `/csi`:

```yaml
- name: csi-reclaim-controller
  image: quay.io/piraeusdatastore/csi-reclaim-controller:v0.1.0
  args:
    - --csi-address=/csi/csi.sock
    - --leader-election
    - --http-endpoint=:8080  # serves /metrics and /healthz/leader-election
    - --timeout=60s
    - --v=2
  volumeMounts:
    - name: socket-dir  # the volume that holds csi.sock
      mountPath: /csi
```

No extra RBAC or ServiceAccount is required: the controller reuses the CSI
controller Pod's existing ServiceAccount, whose external-provisioner rules
already grant everything it needs — `get/list/watch/update` on
`persistentvolumes`, `get` on `secrets`, event creation, and (for
`--leader-election`) `leases` in the Pod's namespace.

### Excluding a volume

Once deployed, the sidecar reclaims every `Retain` PV of the driver on PV
deletion. To keep a specific volume re-appliable (delete the PV, keep the backing
volume), annotate that PV so it is skipped (substitute your driver name):

```sh
kubectl annotate pv <pv-name> linstor.csi.linbit.com/skip-reclaim=true
```

Remove the annotation to return the PV to normal reclaim behavior.

## Development

```sh
make build   # build the binary into bin/ (version stamped via -ldflags)
make test    # run unit tests
make vet     # go vet
make image   # multi-arch container image via buildx
```

The reconcile logic in `pkg/controller` is covered by table-style unit tests
using fake Kubernetes clients and a mock `VolumeDeleter`, so no cluster is
needed to test the state machine.

## Releasing

The changelog follows [Keep a Changelog](https://keepachangelog.com/); add
user-facing changes under the `## [Unreleased]` heading in
[`CHANGELOG.md`](CHANGELOG.md). To cut a release:

```sh
make release VERSION=1.2.3
git push origin HEAD v1.2.3
```

`make release` rolls `[Unreleased]` into a dated section, updates the comparison
links, and creates a signed commit and tag. Pushing the tag triggers the GitHub
Actions workflows, which build and cosign-sign the multi-arch image and open a
draft GitHub release with the extracted release notes.

## License

Apache License 2.0. See [LICENSE](LICENSE).
