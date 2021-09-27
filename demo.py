#!/usr/bin/env python3

# Demo of using container-image-proxy in Python

import os, sys, socket, subprocess, http.client, json

# Shim class to perform HTTP to an already connected socket
class UnixConnection(http.client.HTTPConnection):
    def __init__(self, sock):
        super().__init__("localhost")
        self.sock = sock

    def connect(self):
        pass

(mysock, theirsock) = socket.socketpair()
theirsock_n = theirsock.fileno()
myconn = UnixConnection(mysock)
src = sys.argv[1]
child = subprocess.Popen(['container-image-proxy', f"--sockfd={theirsock_n}", src], pass_fds=[theirsock_n])
myconn.request('GET', '/manifest')
resp = myconn.getresponse();
print(resp.status, resp.reason)
for (k, v) in resp.getheaders():
    print(f"{k}: {v}")
manifest = json.load(resp)

for layer in manifest["layers"]:
    digest = layer["digest"]
    size = int(layer["size"])
    print(f"layer {digest} {size}")
    # Could fetch the layer as a tarball via:
    # myconn.request('GET', '/blobs/{digest})
    # ... do something with myconn.getresponse() ...
