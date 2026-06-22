.PHONY: build test vet image run

build:
	go build ./...

test:
	go test -race -coverprofile=coverage.txt ./...

vet:
	go vet ./...

image:
	KO_DOCKER_REPO=ko.local ko build --local ./cmd/strm-builder

run:
	docker run --rm $$(KO_DOCKER_REPO=ko.local ko build --local ./cmd/strm-builder) $(ARGS)
