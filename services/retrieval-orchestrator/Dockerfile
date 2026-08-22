FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages/service-auth ./packages/service-auth
COPY services/retrieval-orchestrator ./services/retrieval-orchestrator
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/retrieval-orchestrator ./services/retrieval-orchestrator/cmd/server

FROM alpine:3.21
RUN addgroup -S scos && adduser -S -G scos scos
COPY --from=build /out/retrieval-orchestrator /usr/local/bin/retrieval-orchestrator
USER scos
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/retrieval-orchestrator"]
