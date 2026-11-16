FROM node:24-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN mkdir -p /src/internal/ui && npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /telepathy ./cmd/telepathy

FROM alpine:3.22
RUN adduser -D -u 10001 telepathy && mkdir /data && chown telepathy /data
USER telepathy
WORKDIR /app
COPY --from=build /telepathy /usr/local/bin/telepathy
COPY dashboards ./dashboards
ENV TELEPATHY_DB=/data/telepathy.db
VOLUME /data
EXPOSE 4318
ENTRYPOINT ["telepathy"]
CMD ["serve"]
