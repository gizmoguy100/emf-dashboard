FROM golang:1.24-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
COPY cmd ./cmd
COPY web ./web

RUN CGO_ENABLED=0 go build -o /out/emf-dashboard ./cmd/server

FROM alpine:3.20

WORKDIR /app

RUN adduser -D -H -u 10001 appuser

COPY --from=build /out/emf-dashboard /app/emf-dashboard
COPY web ./web

USER appuser

EXPOSE 8080

CMD ["/app/emf-dashboard"]
