# A fake Anthropic endpoint for the end-to-end injection test.
#
# Built from this repository rather than pulled, so the e2e proves the sidecar
# reaches a real upstream over real HTTP without needing an API key or egress
# from the test cluster.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY test/fixtures/ test/fixtures/
RUN CGO_ENABLED=0 go build -trimpath -o /out/fakeprovider ./test/fixtures/fakeanthropic/cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fakeprovider /fakeprovider
USER 65532:65532
ENTRYPOINT ["/fakeprovider"]
