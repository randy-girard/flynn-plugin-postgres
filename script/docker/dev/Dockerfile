# Unit-test / plugin-build image for macOS (Docker Desktop).
# Postgres behavior tests are an in-memory state machine and do not start a server.
FROM ubuntu:24.04

ARG GO_VERSION=1.24.12
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    xz-utils \
    build-essential \
    git \
    sudo \
    python3 \
    squashfs-tools \
    && rm -rf /var/lib/apt/lists/*

RUN arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz \
    && rm -rf /usr/local/go \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz

ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV GOPATH=/go
ENV CGO_ENABLED=0
ENV GOFLAGS="-mod=mod -buildvcs=false"

RUN echo 'root ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/root-nopasswd \
    && chmod 0440 /etc/sudoers.d/root-nopasswd

WORKDIR /src

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
