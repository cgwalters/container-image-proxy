package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/pflag"
)

var Version = ""
var defaultUserAgent = "ostree-container-backend/" + Version

type proxyHandler struct {
	cache  types.BlobInfoCache
	sysctx *types.SystemContext
}

func (h *proxyHandler) implRequest(w http.ResponseWriter, imgname, reqtype, ref string) error {
	ctx := context.TODO()
	imgref, err := docker.ParseReference(imgname)
	if err != nil {
		return err
	}
	imgsrc, err := imgref.NewImageSource(ctx, h.sysctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := imgsrc.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "could not close image: %v\n", err)
		}
	}()
	if reqtype == "manifests" {
		rawManifest, _, err := imgsrc.GetManifest(ctx, nil)
		if err != nil {
			return err
		}
		digest, err := manifest.Digest(rawManifest)
		if err != nil {
			return err
		}
		w.Header().Add("manifest-digest", digest.String())
		img, err := image.FromUnparsedImage(ctx, h.sysctx, image.UnparsedInstance(imgsrc, nil))
		if err != nil {
			return fmt.Errorf("failed to parse manifest for image: %w", err)
		}
		config, err := img.OCIConfig(ctx)
		if err != nil {
			return err
		}
		out, err := json.Marshal(config)
		if err != nil {
			return err
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
		r := bytes.NewReader(out)
		_, err = io.Copy(w, r)
		if err != nil {
			return err
		}
	} else if reqtype == "blobs" {
		d, err := digest.Parse(ref)
		if err != nil {
			return err
		}
		r, blobSize, err := imgsrc.GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", blobSize))
		_, err = io.Copy(w, r)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unhandled request %s", reqtype)
	}

	return nil
}

// ServeHTTP handles two requests:
//
// GET /<host>/<name>/manifests/<reference>
// GET /<host>/<name>/blobs/<digest>
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "" || !strings.HasPrefix(r.URL.Path, "/") {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 6 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	imgref := fmt.Sprintf("//%s/%s/%s", parts[1], parts[2], parts[3])
	reqtype := parts[4]
	ref := parts[5]

	err := h.implRequest(w, imgref, reqtype, ref)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
}

type SockResponseWriter struct {
	out          io.Writer
	headers      http.Header
	wroteHeaders bool
}

func (rw SockResponseWriter) Header() http.Header {
	return rw.headers
}

func (rw SockResponseWriter) Write(buf []byte) (int, error) {
	if !rw.wroteHeaders {
		rw.WriteHeader(200)
	}
	return rw.out.Write(buf)
}

func (rw SockResponseWriter) WriteHeader(statusCode int) {
	if rw.wroteHeaders {
		panic("Already invoked WriteHeader")
	}
	rw.wroteHeaders = true
	rw.out.Write([]byte(fmt.Sprintf("HTTP/1.1 %d OK\r\n", statusCode)))
	rw.headers.Write(rw.out)
	rw.out.Write([]byte("\r\n"))
}

func run() error {
	var version bool
	var quiet bool
	var sockFd int
	var portNum int

	var err error

	pflag.IntVar(&sockFd, "sockfd", -1, "Serve on opened socket pair")
	pflag.IntVar(&portNum, "port", -1, "Serve on TCP port (localhost)")
	pflag.BoolVarP(&quiet, "quiet", "q", false, "Suppress output information when copying images")
	pflag.BoolVar(&version, "version", false, "show the version ("+Version+")")

	pflag.Parse()
	if version {
		fmt.Printf("%s\n", Version)
		os.Exit(0)
	}

	sysCtx := &types.SystemContext{
		DockerRegistryUserAgent: defaultUserAgent,
	}

	handler := &proxyHandler{
		cache:  blobinfocache.DefaultCache(sysCtx),
		sysctx: sysCtx,
	}

	if sockFd != -1 {
		fd := os.NewFile(uintptr(sockFd), "sock")
		defer fd.Close()
		bufr := bufio.NewReader(fd)
		bufw := bufio.NewWriter(fd)

		for {
			req, err := http.ReadRequest(bufr)
			if err != nil {
				return err
			}
			resp := SockResponseWriter{
				out:          bufw,
				headers:      make(map[string][]string),
				wroteHeaders: false,
			}
			handler.ServeHTTP(resp, req)
			err = bufw.Flush()
			if err != nil {
				return err
			}
		}
	} else {
		var listener net.Listener
		addr := net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: portNum,
			Zone: "",
		}
		listener, err = net.ListenTCP("tcp", &addr)
		if err != nil {
			return err
		}
		defer listener.Close()
		srv := &http.Server{
			Handler: handler,
		}
		err = srv.Serve(listener)
		if err != nil {
			return fmt.Errorf("failed to serve: %w", err)
		}
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}
