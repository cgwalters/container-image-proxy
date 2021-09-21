
VERSION:=$(shell git describe --dirty --always)

ostree-container-backend: 
	go build -mod=vendor -ldflags "-X main.Version=$(VERSION)" -o bin/$@ cmd/main.go	
.PHONY: ostree-container-backend

vendor: 
	@go mod vendor
	@go mod tidy
.PHONY: vendor 
