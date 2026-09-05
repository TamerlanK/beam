.PHONY: run dev build test race fmt lint loadtest docker clean

PRETTIER = npx -y prettier@3.6.2

run:
	go run ./cmd/beam

# -v also makes gow kill every descendant on restart; without it (Windows) only go.exe dies and the old beam.exe keeps port 8080
dev:
	gow -v -e=go,html,css,js run ./cmd/beam

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o beam ./cmd/beam

test:
	go test ./...

race:
	go test -race ./...

fmt:
	gofmt -w .
	$(PRETTIER) --write web/static

# same checks as CI
lint:
	go vet ./...
	! gofmt -l . | grep .
	golangci-lint run
	$(PRETTIER) --check web/static

loadtest:
	go run ./cmd/loadtest

docker:
	docker build -t beam .

clean:
	rm -f beam beam.exe loadtest loadtest.exe
