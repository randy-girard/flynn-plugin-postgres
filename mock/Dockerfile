# Local Compose image for the Redis add-on UI. Source is bind-mounted; Air rebuilds.
FROM golang:1.24.12-bookworm

WORKDIR /src
ENV CGO_ENABLED=0
ENV GOFLAGS="-mod=mod -buildvcs=false"
ENV GOTOOLCHAIN=local
ENV GOPRIVATE=github.com/randy-girard/*
ENV GOPROXY=direct
ENV GOSUMDB=off
ENV PATH="/go/bin:${PATH}"

RUN go install github.com/air-verse/air@v1.61.7

COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["air", "-c", "mock/air.toml"]
