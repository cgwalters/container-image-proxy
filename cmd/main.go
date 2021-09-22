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
	"path/filepath"
	"strings"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/types"

	// Ensure all transports are registered
	"github.com/containers/image/v5/transports/alltransports"
	_ "github.com/containers/image/v5/transports/alltransports"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/pflag"
)

var Version = ""
var quiet bool
var defaultUserAgent = "ostree-container-backend/" + Version

type proxyHandler struct {
	sysctx *types.SystemContext
	cache  types.BlobInfoCache
	imgsrc types.ImageSource
}

func (h *proxyHandler) implManifest(w http.ResponseWriter) error {
	ctx := context.TODO()
	rawManifest, _, err := h.imgsrc.GetManifest(ctx, nil)
	if err != nil {
		return err
	}
	digest, err := manifest.Digest(rawManifest)
	if err != nil {
		return err
	}
	w.Header().Add("manifest-digest", digest.String())
	img, err := image.FromUnparsedImage(ctx, h.sysctx, image.UnparsedInstance(h.imgsrc, nil))
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
	return nil
}

func (h *proxyHandler) implBlob(w http.ResponseWriter, digestStr string) error {
	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return err
	}
	r, blobSize, err := h.imgsrc.GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", blobSize))
	_, err = io.Copy(w, r)
	if err != nil {
		return err
	}
	return nil
}

// ServeHTTP handles two requests:
//
// GET /manifest
// GET /blobs/<digest>
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

	var err error
	if r.URL.Path == "/manifest" {
		err = h.implManifest(w)
	} else if strings.HasPrefix(r.URL.Path, "/blobs/") {
		blob := filepath.Base(r.URL.Path)
		err = h.implBlob(w, blob)
	} else {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		if !quiet {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
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

	args := pflag.Args()
	if len(args) != 1 {
		return fmt.Errorf("Exactly one IMAGE is required")
	}
	imgRef, err := alltransports.ParseImageName(args[0])
	if err != nil {
		return err
	}
	imgsrc, err := imgRef.NewImageSource(context.Background(), sysCtx)
	if err != nil {
		return err
	}
	defer func() {
		if err := imgsrc.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "could not close image: %v\n", err)
		}
	}()

	handler := &proxyHandler{
		imgsrc: imgsrc,
		sysctx: sysCtx,
		cache:  blobinfocache.DefaultCache(sysCtx),
	}

	if sockFd != -1 {
		fd := os.NewFile(uintptr(sockFd), "sock")
		defer fd.Close()
		bufr := bufio.NewReader(fd)
		bufw := bufio.NewWriter(fd)

		for {
			req, err := http.ReadRequest(bufr)
			if err != nil {
				if err == io.EOF {
					return nil
				}
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
