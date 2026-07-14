FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -trimpath -o tanuki ./cmd/tanuki
RUN echo "tanuki:x:65534:65534::/:" > /etc/passwd.minimal

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/passwd.minimal /etc/passwd
COPY --from=builder /build/tanuki /tanuki

USER 65534

ENV TANUKI_DATA="/data"
ENV TANUKI_PROXY_PORT="18080"

EXPOSE 18080

ENTRYPOINT ["/tanuki"]
CMD ["proxy"]
