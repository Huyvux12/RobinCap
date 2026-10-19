# syntax=docker/dockerfile:1
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=1.0.0
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/capcut-tts-go .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/capcut-tts-go /capcut-tts-go
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/capcut-tts-go"]
