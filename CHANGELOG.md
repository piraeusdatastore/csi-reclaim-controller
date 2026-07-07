# Changelog
All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial implementation: an optional CSI sidecar that deletes the backing
  storage of PersistentVolumes using `reclaimPolicy: Retain` once the PV object
  is deleted. Deploying it opts every `Retain` PV of the connected CSI driver
  into reclaiming: it places a finalizer and, on deletion, calls `DeleteVolume()`
  (reusing external-provisioner's deletion-secret annotations) before releasing
  the finalizer. Backing storage of still-bound PVs is never touched, failures
  are retried, and the driver name is discovered from the CSI endpoint. The skip
  annotation and finalizer default to keys namespaced by the CSI driver name.
- `<driver-name>/skip-reclaim` annotation (true value) that excludes a PV from
  management. Use it to keep a specific `Retain` volume re-appliable, or to let
  controllers such as linstor-affinity-controller delete and recreate a PV in
  place (e.g. to update node affinity) without its backing volume being
  reclaimed.
- `pkg/reclaim`, a dependency-light package exporting the annotation and
  finalizer keys (`SkipAnnotation`, `Finalizer`, `ShouldSkip`) so other projects
  can interoperate without hardcoding the strings.
- Multi-arch (linux/amd64, linux/arm64) container images, signed with
  [cosign](https://docs.sigstore.dev/) keyless signing and published from GitHub
  Actions.

[Unreleased]: https://github.com/piraeusdatastore/csi-reclaim-controller/commits/main
