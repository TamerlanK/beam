FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /beam ./cmd/beam

FROM scratch
COPY --from=build /beam /beam
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/beam"]
