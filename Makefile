
VERSION:=$(shell git describe --dirty --always)

container-image-proxy: 
	go build -mod=vendor -ldflags "-X main.Version=$(VERSION)" -o bin/$@ cmd/main.go	
.PHONY: container-image-proxy

vendor: 
	@go mod vendor
	@go mod tidy
.PHONY: vendor 
