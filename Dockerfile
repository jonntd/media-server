FROM golang:1-alpine as builder

RUN apk --no-cache --no-progress add git ca-certificates tzdata make \
    && update-ca-certificates \
    && rm -rf /var/cache/apk/*

WORKDIR /app

# Download go modules
COPY go.mod .
COPY go.sum .
RUN GO111MODULE=on go mod download

COPY . .

RUN CGO_ENABLED=0 go build -a --trimpath --installsuffix cgo --ldflags="-s" -o media-server

# Create a minimal container to run a Golang static binary
FROM scratch
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/media-server .


ENV GIN_MODE=release
ENTRYPOINT ["/media-server"]
EXPOSE 9096
