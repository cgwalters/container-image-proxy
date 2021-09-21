# CLI to expose containers/image fetching via HTTP

This is a small CLI program which vendors the
[containers/image](https://github.com/containers/image/) Go library
and exposes a HTTP API to fetch manifests and blobs.

Eventually, this should probably be folded into [containers/skopeo](https://github.com/containers/skopeo/)
but for now we'll iterate here.

# Why?

The primary intended use case of this is for things like
[ostree-containers](https://github.com/ostreedev/ostree-rs-ext/issues/18)
where we're using container images to encapsulate host operating system
updates, but we don't want to involve the [containers/image](github.com/containers/image/)
storage layer.

What we *do* want from the containers ecosystem is support for things like
signatures and offline mirroring

Another theoretical use case could be something like [krustlet](https://github.com/deislabs/krustlet).

# Status

Totally experimental but works.

```
$ ./bin/container-image-proxy --port 8080
```

### Fetch manifest

```
$ curl -L http://127.0.0.1:8080/docker.io/library/busybox/manifests/latest | jq .
{
  "architecture": "amd64",
  "os": "linux",
...
```

### Fetch a blob

```
$ curl -L http://127.0.0.1:8080/docker.io/library/busybox/blobs/sha256:661e87a > layer.tar
```

