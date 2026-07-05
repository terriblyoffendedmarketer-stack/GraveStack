# Single static binary with the PWA embedded. Host-agnostic.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /gravestack .

FROM gcr.io/distroless/static-debian12
COPY --from=build /gravestack /gravestack
# DATA_DIR should point at a mounted volume so the DB survives redeploys.
ENV DATA_DIR=/data
ENV PORT=8080
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/gravestack"]
