.PHONY: check test build
check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race ./...
	python3 scripts/verify_seed.py
	python3 -m unittest discover -s scripts/tests -v
test:
	go test -race ./...
build:
	go build -o bin/council ./cmd/council
