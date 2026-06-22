.PHONY: build test vet image run

KO_DOCKER_REPO ?= ko.local
export KO_DOCKER_REPO

build:
	go build ./...

test:
	go test -race -coverprofile=coverage.txt ./...

vet:
	go vet ./...

image:
	ko build --local ./cmd/strm-builder

run:
	docker run --rm $$(ko build --local ./cmd/strm-builder) $(ARGS)
