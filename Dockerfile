# build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# install build dependencies
RUN apk add --no-cache git gcc musl-dev

# copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# copy source code
COPY . .

# build binary
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o ev-metrics .

# final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# copy binary from builder
COPY --from=builder /build/ev-metrics .

ENTRYPOINT ["/app/ev-metrics"]
