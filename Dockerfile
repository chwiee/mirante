FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mirante ./cmd/mirante

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mirante /mirante
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/mirante"]
