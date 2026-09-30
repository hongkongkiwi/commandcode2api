# build: CGO-free static binary (modernc.org/sqlite is pure Go)
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags 2>/dev/null || echo dev)" -o /out/commandcode2api .

FROM alpine:3.20
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app
COPY --from=build /out/commandcode2api /usr/local/bin/commandcode2api
USER app
WORKDIR /app
VOLUME /app/data
EXPOSE 3050
ENTRYPOINT ["commandcode2api"]
CMD ["-config", "/app/config.json", "-state-dir", "/app/data"]
