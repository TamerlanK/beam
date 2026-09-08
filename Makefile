.PHONY: run dev build test race fmt lint loadtest docker clean

PRETTIER = npx -y prettier@3.6.2
JSTEST = node --test "web/test/**/*.test.mjs"

run:
	go run ./cmd/beam

dev:
	gow -v -e=go,html,css,js run ./cmd/beam

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o beam ./cmd/beam

test:
	go test ./...
	$(JSTEST)

race:
	go test -race ./...
	$(JSTEST)

fmt:
	gofmt -w .
	$(PRETTIER) --write web

lint:
	go vet ./...
	! gofmt -l . | grep .
	golangci-lint run
	$(PRETTIER) --check web

loadtest:
	go run ./cmd/loadtest

docker:
	docker build -t beam .

clean:
	rm -f beam beam.exe loadtest loadtest.exe
