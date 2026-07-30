# goku's own container image (dogfood): the same file goku uses to deploy
# any project. Multi-stage: Vite web build + Go gokud, final image runs
# gokud honoring the goku env contract (PORT, DATABASE_URL, GOKU_DATA).

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /gokud ./cmd/gokud

FROM alpine:3.21
# docker-cli + postgresql client + caddy make a containerized gokud a full
# control plane: it can build/run app containers via the host docker socket,
# provision app databases, and reload the host Caddy (admin API).
RUN apk add --no-cache git git-daemon ca-certificates docker-cli docker-cli-buildx postgresql16-client openssh-client openssl
COPY --from=caddy:2 /usr/bin/caddy /usr/local/bin/caddy
# Mounted repos are owned by the host user; this is a single-purpose container.
RUN git config --system safe.directory '*'

COPY --from=build /gokud /usr/local/bin/gokud
COPY --from=web /src/web/dist /opt/goku/web/dist
ENV WEB_DIST=/opt/goku/web/dist GOKU_DATA=/data
EXPOSE 8080
CMD ["gokud"]
