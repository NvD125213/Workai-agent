# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET=api
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app

COPY --from=builder /out/app /app/app

EXPOSE 8080

ENTRYPOINT ["/app/app"]
