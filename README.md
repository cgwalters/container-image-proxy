# CLI to expose containers/image fetching via HTTP

This is a small CLI program which vendors the
[containers/image](https://github.com/containers/image/) Go library
and exposes a HTTP API to fetch manifests and blobs.

Eventually, this should probably be folded into [containers/skopeo](https://github.com/containers/skopeo/)
but for now we'll iterate here.

# Why?

First, assume one is operating on a codebase that isn't Go, but wants
to interact with container images - we can't just include the Go containers/image
library.

The primary intended use case of this is for things like
[ostree-containers](https://github.com/ostreedev/ostree-rs-ext/issues/18)
where we're using container images to encapsulate host operating system
updates, but we don't want to involve the [containers/image](github.com/containers/image/)
storage layer.

What we *do* want from the containers/image library is support for things like
signatures and offline mirroring.  More on this below.

Forgetting things like ostree exist for a second - imagine that you wanted to 
encapsulate a set of Debian/RPM/etc packages inside
a container image to ship for package-based operating systems.  You could use this to stream
out the layer containing those packages and extract them directly, rather than serializing
everything to disk in the containers/storage disk location, only to copy it out again and delete the first.

Another theoretical use case could be something like [krustlet](https://github.com/deislabs/krustlet),
which fetches WebAssembly blobs inside containers.  Here again, we don't want to involve
containers/storage.

# Desired containers/image features

There are e.g. Rust libraries like [dkregistry-rs](https://github.com/camallo/dkregistry-rs), and
similar for other languages.  However, the containers/image Go library has a lot of additional infrastructure
that will impose a maintenance burden to replicate:

 - Signatures (`man containers-auth.json`)
 - Mirroring/renaming (`man containers-registries.conf`)
 - Support for `~/.docker/config.json` for authentication as well as `/run`

# Status

We have a 0.1 release that works.  However, in the future this will hopefully
move into [skopeo](https://github.com/containers/skopeo/).

# Usage

The intended production use of this is:

- Parent process creates a [socketpair](https://man7.org/linux/man-pages/man2/socketpair.2.html) (e.g. [Rust tokio](https://docs.rs/tokio/1.12.0/tokio/net/struct.UnixStream.html#method.pair))
- Parent passes one half of socketpair to child via e.g. fd 3 - `container-image-proxy --sockfd 3 docker://quay.io/cgwalters/exampleos:latest`
- Parent makes HTTP (1.1) requests on its half of the socketpair

## Usage example via localhost TCP

The key advantage of using `socketpair()` is that communication is private
between the two processes - and you don't have to worry about the usual things
like TCP port conflicts/assignments.

However, it's easiest to demo this CLI by listening on a local TCP port:

```
$ ./bin/container-image-proxy --port 8080 docker://quay.io/cgwalters/exampleos:latest
```

### Fetch manifest

```
$ curl -L http://127.0.0.1:8080/manifest | jq .
{
  "architecture": "amd64",
  "os": "linux",
...
```

This will always convert the manifest into OCI format.  The original
manifest digest is available in a `Manifest-Digest` HTTP header.

### Fetch a blob

```
$ curl -L http://127.0.0.1:8080/blobs/sha256:661e87a > layer.tar
```

