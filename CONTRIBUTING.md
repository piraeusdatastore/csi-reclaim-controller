# Contributing to csi-reclaim-controller

The [Piraeus contributing guide](https://github.com/piraeusdatastore/piraeus/blob/master/CONTRIBUTING.md) describes how to report issues, write commits and changelog entries, and open pull requests.
The sections that follow describe what is specific to this repository.

## Target branch

Open pull requests against the `main` branch.

## Build and test

Install [Go](https://go.dev/) in the version declared in `go.mod`.
The [Development](./README.md#development) section of the README lists the Makefile targets for building and testing.

On every pull request, CI runs `go test ./...`, [`golangci-lint`](https://golangci-lint.run/) on new issues, and a build of the container image.
To run the linter locally:

```
$ golangci-lint run
```

## Testing on a cluster

To try a change on a cluster, build an image and push it to your registry:

```
$ make push REGISTRY=registry.example.com/myname TAG=dev
```

By default, the image is built for `linux/amd64` and `linux/arm64`.
To build for a single platform, set `PLATFORMS`, for example `PLATFORMS=linux/amd64`.
Replace the image of the sidecar container in the controller Pod of your CSI driver with the new image.
