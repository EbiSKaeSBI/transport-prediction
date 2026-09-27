# Backend: Go-гейтвей NDTP+HTTP+WS. Multi-stage: статический бинарь без CGO.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/transportctl ./cmd/transportctl

FROM alpine:3.20
RUN adduser -D -H app
COPY --from=build /out/transportctl /usr/local/bin/transportctl
USER app
EXPOSE 9201 8080
# --plan/--binding перечитываются по --plan-watch 1s: файл может появиться
# позже старта (real-feed перегенерирует его) — контейнер не обязан умирать.
CMD ["transportctl", "serve", \
  "--listen", ":9201", "--http", ":8080", \
  "--plan", "/data/plan.csv", "--binding", "/data/binding.csv", \
  "--plan-watch", "1s", "--capture-out", "/data/.cache/capture", \
  "--ml", "http://ml:8000", \
  "--tick", "5s", "--grid", "5s", "--stats", "30s"]
