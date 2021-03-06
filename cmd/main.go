package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "crypto/sha256"
	_ "crypto/sha512"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/pflag"
)

var Version = ""
var quiet bool
var defaultUserAgent = "ostree-container-backend/" + Version

type proxyHandler struct {
	imageref string
	sysctx   *types.SystemContext
	cache    types.BlobInfoCache
	imgsrc   *types.ImageSource
	img      *types.Image
	shutdown bool
}

func (h *proxyHandler) ensureImage() error {
	if h.img != nil {
		return nil
	}
	imgRef, err := alltransports.ParseImageName(h.imageref)
	if err != nil {
		return err
	}
	imgsrc, err := imgRef.NewImageSource(context.Background(), h.sysctx)
	if err != nil {
		return err
	}
	img, err := image.FromUnparsedImage(context.Background(), h.sysctx, image.UnparsedInstance(imgsrc, nil))
	if err != nil {
		return fmt.Errorf("failed to load image: %w", err)
	}
	h.img = &img
	h.imgsrc = &imgsrc
	return nil
}

func (h *proxyHandler) implManifest(w http.ResponseWriter, r *http.Request) error {
	if err := h.ensureImage(); err != nil {
		return err
	}

	_, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		return err
	}
	ctx := context.TODO()
	rawManifest, _, err := (*h.img).Manifest(ctx)
	if err != nil {
		return err
	}
	digest, err := manifest.Digest(rawManifest)
	if err != nil {
		return err
	}
	w.Header().Add("Manifest-Digest", digest.String())

	ociManifest, err := manifest.OCI1FromManifest(rawManifest)
	if err != nil {
		return err
	}
	ociSerialized, err := ociManifest.Serialize()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(ociSerialized)))
	w.WriteHeader(200)
	_, err = io.Copy(w, bytes.NewReader(ociSerialized))
	if err != nil {
		return err
	}
	return nil
}

func (h *proxyHandler) implBlob(w http.ResponseWriter, r *http.Request, digestStr string) error {
	if err := h.ensureImage(); err != nil {
		return err
	}

	_, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		return err
	}

	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return err
	}
	blobr, blobSize, err := (*h.imgsrc).GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", blobSize))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	verifier := d.Verifier()
	tr := io.TeeReader(blobr, verifier)
	_, err = io.Copy(w, tr)
	if err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("Corrupted blob, expecting %s", d.String())
	}
	return nil
}

// ServeHTTP handles two requests:
//
// GET /manifest
// GET /blobs/<digest>
// POST /quit
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if r.URL.Path == "/quit" {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(200)
			h.shutdown = true
			return
		}
	}

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
	if err != nil {

	}

	if r.URL.Path == "/manifest" {
		err = h.implManifest(w, r)
	} else if strings.HasPrefix(r.URL.Path, "/blobs/") {
		blob := filepath.Base(r.URL.Path)
		err = h.implBlob(w, r, blob)
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
	out     io.Writer
	headers http.Header
}

func (rw SockResponseWriter) Header() http.Header {
	return rw.headers
}

func (rw SockResponseWriter) Write(buf []byte) (int, error) {
	return rw.out.Write(buf)
}

func (rw SockResponseWriter) WriteHeader(statusCode int) {
	rw.out.Write([]byte(fmt.Sprintf("HTTP/1.1 %d OK\r\n", statusCode)))
	rw.headers.Write(rw.out)
	rw.out.Write([]byte("\r\n"))
}

func run() error {
	var version bool
	var sockFd int

	pflag.IntVar(&sockFd, "sockfd", -1, "Serve on opened socket pair")
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

	handler := &proxyHandler{
		imageref: args[0],
		sysctx:   sysCtx,
		cache:    blobinfocache.DefaultCache(sysCtx),
	}

	var buf *bufio.ReadWriter
	if sockFd != -1 {
		fd := os.NewFile(uintptr(sockFd), "sock")
		buf = bufio.NewReadWriter(bufio.NewReader(fd), bufio.NewWriter(fd))
	} else {
		buf = bufio.NewReadWriter(bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout))
	}

	for {
		req, err := http.ReadRequest(buf.Reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := SockResponseWriter{
			out:     buf,
			headers: make(map[string][]string),
		}
		handler.ServeHTTP(resp, req)
		err = buf.Flush()
		if err != nil {
			return err
		}

		if handler.shutdown {
			break
		}
	}

	if handler.img != nil {
		if err := (*handler.imgsrc).Close(); err != nil {
			return err
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
