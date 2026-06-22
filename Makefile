.PHONY: build test vet image run

KO_DOCKER_REPO ?= ghcr.io/kanya-approve/strm-builder
# Program vars are empty so the binary's own defaults apply unless overridden
# (e.g. make run SOURCE_URLS=https://user:pass@host/movies PRUNE=true).
SOURCE_URLS ?=
ROOT_FOLDER ?=
MEDIA_EXTENSIONS ?=
CONCURRENCY ?=
EMBED_CREDENTIALS ?=
PRUNE ?=
DRY_RUN ?=
TIMEOUT ?=
export KO_DOCKER_REPO SOURCE_URLS ROOT_FOLDER MEDIA_EXTENSIONS CONCURRENCY EMBED_CREDENTIALS PRUNE DRY_RUN TIMEOUT

build:
	go build ./...

test:
	go test -race -coverprofile=coverage.txt ./...

vet:
	go vet ./...

image:
	ko build --bare ./cmd/strm-builder

run:
	go run ./cmd/strm-builder $(ARGS)
