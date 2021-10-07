package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"

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
var defaultUserAgent = "cgwalters/container-image-proxy/" + Version

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

type handlerFuncError func(http.ResponseWriter, *http.Request) error

func wrapServeFuncForError(f handlerFuncError) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := f(w, r)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	}
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
	_, err = io.Copy(w, bytes.NewReader(ociSerialized))
	return err
}

func (h *proxyHandler) implBlob(w http.ResponseWriter, r *http.Request) error {
	if err := h.ensureImage(); err != nil {
		return err
	}

	_, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		return err
	}

	digestStr := path.Base(r.URL.Path)

	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return err
	}
	blobr, blobSize, err := (*h.imgsrc).GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
	if err != nil {
		return err
	}
	if blobSize != -1 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", blobSize))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
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

type dummyListener struct {
	sock *net.Conn
}

func (l dummyListener) Accept() (net.Conn, error) {
	if l.sock != nil {
		return *l.sock, nil
	}
	return nil, fmt.Errorf("closed")
}

func (l dummyListener) Close() error {
	if l.sock != nil {
		err := (*l.sock).Close()
		l.sock = nil
		return err
	}
	return nil
}

func (l dummyListener) Addr() net.Addr {
	if l.sock != nil {
		return (*l.sock).LocalAddr()
	}
	return &net.IPAddr{}
}

func run() error {
	var version bool
	var sockFd int

	pflag.IntVar(&sockFd, "sockfd", -1, "Serve on opened socket pair on this file decscriptor")
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

	if sockFd == -1 {
		return fmt.Errorf("--sockfd is required")
	}

	handler := &proxyHandler{
		imageref: args[0],
		sysctx:   sysCtx,
		cache:    blobinfocache.DefaultCache(sysCtx),
	}

	fd := os.NewFile(uintptr(sockFd), "sock")
	fdconn, err := net.FileConn(fd)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	server := http.Server{
		Handler: mux,
	}

	mux.HandleFunc("/manifest", wrapServeFuncForError(handler.implManifest))
	mux.HandleFunc("/blobs", wrapServeFuncForError(handler.implBlob))
	mux.HandleFunc("/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("OK"))
		os.Exit(0)
	})

	listener := dummyListener{
		sock: &fdconn,
	}
	// Note ordinarily, we expect to exit via /quit
	return server.Serve(listener)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}
