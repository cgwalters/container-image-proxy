package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	_ "crypto/sha256"
	_ "crypto/sha512"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/godbus/dbus/v5"
	godbus "github.com/godbus/dbus/v5"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/pflag"
)

var Version = ""
var quiet bool
var defaultUserAgent = "cgwalters/container-image-proxy/" + Version

type blobFetch struct {
	w   *os.File
	wg  sync.WaitGroup
	err error
}

type proxyHandler struct {
	lock        sync.Mutex
	imageref    string
	sysctx      *types.SystemContext
	cache       types.BlobInfoCache
	imgsrc      *types.ImageSource
	img         *types.Image
	blobfetches map[string]*blobFetch
	shutdown    chan struct{}
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

func (h *proxyHandler) GetManifest() (string, []byte, error) {
	h.lock.Lock()
	defer h.lock.Unlock()
	if err := h.ensureImage(); err != nil {
		return "", nil, err
	}

	ctx := context.TODO()
	rawManifest, _, err := (*h.img).Manifest(ctx)
	if err != nil {
		return "", nil, err
	}
	digest, err := manifest.Digest(rawManifest)
	if err != nil {
		return "", nil, err
	}
	ociManifest, err := manifest.OCI1FromManifest(rawManifest)
	if err != nil {
		return "", nil, err
	}
	ociSerialized, err := ociManifest.Serialize()
	if err != nil {
		return "", nil, err
	}

	return digest.String(), ociSerialized, nil
}

func (h *proxyHandler) StartBlobFetch(digestStr string) (int64, dbus.UnixFD, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if err := h.ensureImage(); err != nil {
		return 0, 0, err
	}

	piper, pipew, err := os.Pipe()
	if err != nil {
		return 0, 0, err
	}

	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return 0, 0, err
	}
	blobr, blobSize, err := (*h.imgsrc).GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
	if err != nil {
		return 0, 0, err
	}

	// Maintain reference to pipe writer by fd
	f := blobFetch{
		w: pipew,
	}
	h.blobfetches[digestStr] = &f

	f.wg.Add(1)
	go func() {
		// Signal completion when we return
		defer f.wg.Done()
		// Hack - godbus should support taking proper ownership of returned FDs.
		defer piper.Close()
		verifier := d.Verifier()
		tr := io.TeeReader(blobr, verifier)
		_, err = io.Copy(f.w, tr)
		if err != nil {
			f.err = err
			return
		}
		if !verifier.Verified() {
			f.err = fmt.Errorf("Corrupted blob, expecting %s", d.String())
		}
	}()

	return blobSize, godbus.UnixFD(piper.Fd()), nil
}

func (h *proxyHandler) CompleteBlobFetch(digestStr string) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	f, ok := h.blobfetches[digestStr]
	if !ok {
		return fmt.Errorf("No active fetch for " + digestStr)
	}

	f.wg.Wait()
	err := f.err
	delete(h.blobfetches, digestStr)
	return err
}

func (h *proxyHandler) Shutdown() error {
	h.lock.Lock()
	defer h.lock.Unlock()
	close(h.shutdown)
	return nil
}

func run() error {
	var version bool
	var sockFd int

	pflag.IntVar(&sockFd, "sockfd", -1, "Serve on opened socket pair on this file decscriptor")
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
		shutdown: make(chan struct{}),
	}

	fd := os.NewFile(uintptr(sockFd), "sock")
	conn, err := godbus.NewConn(fd)
	if err != nil {
		return err
	}

	conn.Auth(nil)

	conn.Export(handler, "/container/image/proxy", "container.image.proxy")

	<-handler.shutdown
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}
