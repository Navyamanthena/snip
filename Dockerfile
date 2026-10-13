FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /snip ./cmd/snip

# Tiny final image: just the static binary.
FROM gcr.io/distroless/static-debian12
COPY --from=build /snip /snip
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/snip"]
