FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CMD permite empacotar outro binário do repo com o mesmo Dockerfile, ex:
#   docker build --build-arg CMD=./examples/mcp-sessoes -t mcp-sessoes .
ARG CMD=./cmd/mirante
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mirante ${CMD}

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mirante /mirante
EXPOSE 8080
# UID numérico: com runAsNonRoot o kubelet precisa provar que não é root
# ("USER nonroot" faz o pod falhar com CreateContainerConfigError)
USER 65532:65532
ENTRYPOINT ["/mirante"]
