.PHONY: build test clean

build:
	go build -o fluxion ./cmd/fluxion

test:
	go test ./...

clean:
	rm -f fluxion

verify: build
	./scripts/verify.sh
