# ビルド用
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
# 依存だけ先に取得(キャッシュが効いて2回目以降を速くするため)
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /gochat .

# 実行用
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /gochat ./gochat
COPY templates ./templates
COPY static ./static
EXPOSE 4001
USER nonroot:nonroot
ENTRYPOINT ["./gochat"]
