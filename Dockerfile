FROM okteto/okteto:stable AS okteto-cli

FROM golang:1.27-bookworm AS builder

WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ .
RUN CGO_ENABLED=0 go build -trimpath -o /out/users-cleanup .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home app

# The service runs "okteto kubeconfig" to get Kubernetes credentials from Okteto.
COPY --from=okteto-cli /usr/local/bin/okteto /usr/local/bin/okteto
COPY --from=builder /out/users-cleanup /usr/local/bin/users-cleanup

USER 10001
ENV HOME=/home/app
EXPOSE 8080

CMD ["/usr/local/bin/users-cleanup"]
