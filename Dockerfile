# Builds both binaries that run in the cluster.
#
# The waveoff CLI is distributed as a release binary, not here: nothing in the
# cluster runs it.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGET=manager
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
