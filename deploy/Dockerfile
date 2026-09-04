# ---- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy the manifests alone first. This layer only invalidates when dependencies
# change, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 is what makes the distroless stage possible: the sqlite driver
# is pure Go, so the binary needs no libc at all.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/sqlguard ./cmd/sqlguard

# ---- run --------------------------------------------------------------------
# base-debian12 rather than static: it carries CA certificates and /etc/passwd,
# and nothing else. No shell, so a compromised process has nothing to spawn.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/sqlguard /usr/local/bin/sqlguard

# 65532 is distroless's "nonroot" user. Declared here as well as in the pod spec
# so the image is safe to run outside Kubernetes too.
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sqlguard"]
CMD ["serve", "--db", "/data/shop.db", "--http", ":8080", \
     "--approvals", "/data/approvals", "--audit", "/data/audit.jsonl"]
