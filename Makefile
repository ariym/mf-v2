.PHONY: build test race vet formats
build:
	go build -o mf ./cmd/mf
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
formats:
	python3 scripts/generate_formats.py
	gofmt -w internal/media/formats_generated.go
