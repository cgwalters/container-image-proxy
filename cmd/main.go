package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

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
var defaultUserAgent = "cgwalters/container-image-proxy/" + Version

// A JSON request
type request struct {
	Method string        `json:"method"`
	Args   []interface{} `json:"args"`
}

// Like Rust's Result<T>, though we explicitly
// represent the success status to be doubly sure.
type reply struct {
	Success bool        `json:"success"`
	Value   interface{} `json:"value"`
	PipeID  uint32      `json:"pipeid"`
	Error   string      `json:"error"`
}

// Our internal deserialization of reply plus optional fd
type replyBuf struct {
	value  interface{}
	fd     *os.File
	pipeid uint32
}

type activePipe struct {
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
	activePipes map[uint32]*activePipe
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

func (h *proxyHandler) GetManifest(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	if len(args) != 0 {
		return ret, fmt.Errorf("invalid request, expecting zero arguments")
	}

	if err := h.ensureImage(); err != nil {
		return ret, err
	}

	ctx := context.TODO()
	rawManifest, _, err := (*h.img).Manifest(ctx)
	if err != nil {
		return ret, err
	}
	digest, err := manifest.Digest(rawManifest)
	if err != nil {
		return ret, err
	}
	ociManifest, err := manifest.OCI1FromManifest(rawManifest)
	if err != nil {
		return ret, err
	}
	ociSerialized, err := ociManifest.Serialize()
	if err != nil {
		return ret, err
	}

	piper, pipew, err := os.Pipe()
	if err != nil {
		return ret, err
	}
	f := activePipe{
		w: pipew,
	}
	h.activePipes[uint32(pipew.Fd())] = &f
	f.wg.Add(1)
	go func() {
		// Signal completion when we return
		defer f.wg.Done()
		_, err = io.Copy(f.w, bytes.NewReader(ociSerialized))
		if err != nil {
			f.err = err
		}
	}()

	r := replyBuf{
		value:  digest.String(),
		fd:     piper,
		pipeid: uint32(pipew.Fd()),
	}
	return r, nil
}

func (h *proxyHandler) GetBlob(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	if len(args) != 1 {
		return ret, fmt.Errorf("invalid request, expecting one blobid")
	}

	digestStr, ok := args[0].(string)
	if !ok {
		return ret, fmt.Errorf("expecting string blobid")
	}

	if err := h.ensureImage(); err != nil {
		return ret, err
	}

	piper, pipew, err := os.Pipe()
	if err != nil {
		return ret, err
	}

	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return ret, err
	}
	blobr, blobSize, err := (*h.imgsrc).GetBlob(ctx, types.BlobInfo{Digest: d, Size: -1}, h.cache)
	if err != nil {
		return ret, err
	}

	f := activePipe{
		w: pipew,
	}
	h.activePipes[uint32(pipew.Fd())] = &f

	f.wg.Add(1)
	go func() {
		// Signal completion when we return
		defer f.wg.Done()
		verifier := d.Verifier()
		tr := io.TeeReader(blobr, verifier)
		_, err = io.Copy(f.w, tr)
		if err != nil {
			f.err = err
			return
		}
		if !verifier.Verified() {
			f.err = fmt.Errorf("corrupted blob, expecting %s", d.String())
		}
	}()

	ret.value = blobSize
	ret.fd = piper
	ret.pipeid = uint32(pipew.Fd())
	return ret, nil
}

func (h *proxyHandler) FinishPipe(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	pipeidf, ok := args[0].(float64)
	if !ok {
		return ret, fmt.Errorf("finishpipe: expecting blobid, not %T", args[0])
	}
	pipeid := uint32(pipeidf)

	f, ok := h.activePipes[pipeid]
	if !ok {
		return ret, fmt.Errorf("finishpipe: no active pipe %d", pipeid)
	}

	f.wg.Wait()
	f.w.Close()
	err := f.err
	delete(h.activePipes, pipeid)
	return ret, err
}

func (buf replyBuf) send(conn *net.UnixConn, err error) error {
	replyToSerialize := reply{
		Success: err == nil,
		Value:   buf.value,
		PipeID:  buf.pipeid,
	}
	if err != nil {
		replyToSerialize.Error = err.Error()
	}
	serializedReply, err := json.Marshal(&replyToSerialize)
	if err != nil {
		return err
	}
	defer func() {
		if buf.fd != nil {
			buf.fd.Close()
		}
	}()
	fds := make([]int, 0)
	if buf.fd != nil {
		fds = append(fds, int(buf.fd.Fd()))
	}
	oob := syscall.UnixRights(fds...)
	n, oobn, err := conn.WriteMsgUnix(serializedReply, oob, nil)
	if err != nil {
		return err
	}
	if n != len(serializedReply) || oobn != len(oob) {
		return io.ErrShortWrite
	}
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
		return fmt.Errorf("exactly one IMAGE is required")
	}

	if sockFd == -1 {
		return fmt.Errorf("--sockfd is required")
	}

	handler := &proxyHandler{
		imageref:    args[0],
		sysctx:      sysCtx,
		cache:       blobinfocache.DefaultCache(sysCtx),
		activePipes: make(map[uint32]*activePipe),
	}

	fd := os.NewFile(uintptr(sockFd), "sock")
	fconn, err := net.FileConn(fd)
	if err != nil {
		return err
	}
	conn := fconn.(*net.UnixConn)

	buf := make([]byte, 32*1024)
out:
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break out
			}
			return fmt.Errorf("reading socket: %v", err)
		}
		readbuf := buf[0:n]
		var req request
		if err := json.Unmarshal(readbuf, &req); err != nil {
			rb := replyBuf{}
			rb.send(conn, fmt.Errorf("invalid request: %v", err))
		}
		switch req.Method {
		case "GetManifest":
			{
				rb, err := handler.GetManifest(req.Args)
				rb.send(conn, err)
			}
		case "FinishPipe":
			{
				rb, err := handler.FinishPipe(req.Args)
				rb.send(conn, err)
			}
		case "Shutdown":
			break out
		default:
			rb := replyBuf{}
			rb.send(conn, fmt.Errorf("unknown method: %s", req.Method))
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
