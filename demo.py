#!/usr/bin/env python3

# Demo of using container-image-proxy in Python

import os, sys, socket, subprocess, http.client, json
from gi.repository import GLib, Gio

(mysock, theirsock) = socket.socketpair()
theirsock_n = theirsock.fileno()
src = sys.argv[1]
child = subprocess.Popen(['container-image-proxy', fuse std::os::unix::process::CommandExt;
--sockfd={theirsock_n}", src], pass_fds=[theirsock_n])
s = Gio.Socket.new_from_fd(mysock.fileno())
s = Gio.Socket.connection_factory_create_connection(s)
c = Gio.DBusConnection.new_sync(s, "", 0, None, None)
print("Created connection")
proxy = Gio.DBusProxy.new_sync(c, Gio.DBusProxyFlags.DO_NOT_LOAD_PROPERTIES, None,
            None, "/container/image/proxy", "container.image.proxy", None)
print("Created proxy")

(digest, manifest) = proxy.GetManifest()
print(f"manifest digest: {digest}")
manifest = json.load(resp)

for layer in manifest["layers"]:
    digest = layer["digest"]
    size = int(layer["size"])
    print(f"layer {digest} {size}")

proxy.Shutdown()
r = child.wait()
if r != 0:
    raise SystemExit("container-image-proxy failed with code {r}")
