.PHONY: build test vet image run

# Overridable via environment or command line. Program vars are empty so the
# binary's own defaults apply unless set (e.g. make run SOURCE_URLS=...).
KO_DOCKER_REPO ?= ko.local/strm-builder
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
	ko build --local --bare ./cmd/strm-builder

run: image
	docker run --rm \
		-e SOURCE_URLS -e ROOT_FOLDER -e MEDIA_EXTENSIONS -e CONCURRENCY \
		-e EMBED_CREDENTIALS -e PRUNE -e DRY_RUN -e TIMEOUT \
		$(KO_DOCKER_REPO) $(ARGS)
