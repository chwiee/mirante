# Compila na arquitetura do builder e gera o binário da arquitetura alvo
# (compilação cruzada do Go): imagem multi-arch sem emulação QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CMD permite empacotar outro binário do repo com o mesmo Dockerfile, ex:
#   docker build --build-arg CMD=./examples/mcp-sessoes -t mcp-sessoes .
ARG CMD=./cmd/mirante
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mirante ${CMD}

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/chwiee/mirante" \
      org.opencontainers.image.description="Painel em tempo real dos agentes de IA (proxy LLM + MCP)" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/mirante /mirante
EXPOSE 8080
# UID numérico: com runAsNonRoot o kubelet precisa provar que não é root
# ("USER nonroot" faz o pod falhar com CreateContainerConfigError)
USER 65532:65532
ENTRYPOINT ["/mirante"]
