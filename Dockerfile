# Builds llm-shape-proxy and the demo tools into one small image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ \
      ./cmd/llm-shape-proxy ./tools/fakeupstream ./tools/loadgen

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
# Replay recordings for the demo upstream and load generator.
COPY testdata/replay /replay
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/llm-shape-proxy"]
