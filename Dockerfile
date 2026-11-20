FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go source.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/reddit-bridge .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/reddit-bridge /usr/local/bin/reddit-bridge
EXPOSE 8788
ENTRYPOINT ["/usr/local/bin/reddit-bridge"]
