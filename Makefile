.PHONY: build dist clean test race vet formats

export VERSION

build:
	sh scripts/build.sh local
dist:
	sh scripts/build.sh dist
clean:
	rm -rf dist
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
formats:
	python3 scripts/generate_formats.py
	gofmt -w internal/media/formats_generated.go
