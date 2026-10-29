FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod .
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /cryptopay .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && mkdir -p /data && chown nobody:nobody /data
COPY --from=build /cryptopay /cryptopay
USER nobody
ENV DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 18777
HEALTHCHECK --interval=30s --timeout=5s CMD wget -q -O- http://127.0.0.1:18777/healthz || exit 1
CMD ["/cryptopay"]
