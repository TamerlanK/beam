.PHONY: run dev build test race lint loadtest docker clean

run:
	go run ./cmd/beam

dev:
	gow -e=go,html,css,js run ./cmd/beam

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o beam ./cmd/beam

test:
	go test ./...

race:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

loadtest:
	go run ./cmd/loadtest

docker:
	docker build -t beam .

clean:
	rm -f beam beam.exe loadtest loadtest.exe
