# build stage
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS TARGETARCH

WORKDIR /build

# copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# copy source code
COPY . .

# build static binary for the target platform
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o ev-metrics .

# final stage: includes CA certificates and a nonroot user (uid 65532)
FROM gcr.io/distroless/static:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087

WORKDIR /app

COPY --from=builder /build/ev-metrics .

USER 65532:65532

ENTRYPOINT ["/app/ev-metrics"]
