.PHONY: build test vet image run

# Program vars are empty so the binary's own defaults apply unless overridden
# (e.g. make run SOURCE_URLS=https://user:pass@host/movies PRUNE=true).
KO_DOCKER_REPO ?= ko.local
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
	ko build --local ./cmd/strm-builder

run:
	docker run --rm \
		-e SOURCE_URLS -e ROOT_FOLDER -e MEDIA_EXTENSIONS -e CONCURRENCY \
		-e EMBED_CREDENTIALS -e PRUNE -e DRY_RUN -e TIMEOUT \
		$$(ko build --local ./cmd/strm-builder) $(ARGS)
