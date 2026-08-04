FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY services/okf-synchroniser ./services/okf-synchroniser
COPY packages/database ./packages/database
COPY packages/tlsserve ./packages/tlsserve
COPY packages/auth ./packages/auth
COPY services/readiness ./services/readiness
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/okf-synchroniser ./services/okf-synchroniser/cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -S scos && adduser -S -G scos scos \
    && mkdir -p /var/lib/scos/okf
COPY --from=build /out/okf-synchroniser /usr/local/bin/okf-synchroniser
# findOKFRoot() (services/okf-synchroniser/cmd/server/main.go) resolves the
# relative "okf" candidate against the container's working directory ("/"),
# and readinessCheck requires that directory to be writable. A host bind
# mount would keep host ownership (not the scos user), so the approved OKF
# content is baked into the image here instead -- consistent with how every
# other internal package dependency in this Dockerfile is provided, and it
# gives okf-synchroniser a real, indexable copy instead of an empty
# synthesised directory. GOVERNANCE.md §26: Git remains authoritative for
# approved content; this baked copy is what gets indexed and synced from,
# refreshed on each image rebuild.
COPY okf /okf
RUN chown -R scos:scos /okf /var/lib/scos
USER scos
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/okf-synchroniser"]
