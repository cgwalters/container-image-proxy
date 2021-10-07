
VERSION:=$(shell git describe --dirty --always)

TAGS ?= exclude_graphdriver_devicemapper exclude_graphdriver_btrfs

container-image-proxy: 
	go build -mod=vendor -ldflags "-X main.Version=$(VERSION)" -tags "$(TAGS)" -o bin/$@ cmd/main.go
.PHONY: container-image-proxy

vendor: 
	@go mod vendor
	@go mod tidy
.PHONY: vendor 

install:
	install -m 0755 -D -t $(DESTDIR)$(PREFIX)/usr/bin bin/container-image-proxy
