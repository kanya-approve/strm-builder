.PHONY: build test vet

build:
	go build ./...

test:
	go test -race -coverprofile=coverage.txt ./...

vet:
	go vet ./...
