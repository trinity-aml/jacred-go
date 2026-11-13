FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG SOURCE_REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X jacred/server.Version=1.4.3-home -X jacred/server.GitSha=$SOURCE_REVISION -X jacred/server.GitBranch=home-runtime" \
    -o /out/jacred ./cmd

FROM debian:bookworm-slim
ARG SOURCE_REVISION=unknown
LABEL org.opencontainers.image.revision=$SOURCE_REVISION
ENV TZ=Europe/Moscow HOME=/data XDG_CACHE_HOME=/data/.cache
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates curl tini tzdata xvfb xauth fonts-liberation ffmpeg \
    libasound2 libatk-bridge2.0-0 libatk1.0-0 libcups2 libdbus-1-3 libdrm2 \
    libgbm1 libglib2.0-0 libgtk-3-0 libnspr4 libnss3 libpango-1.0-0 \
    libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
    libxkbcommon0 libxrandr2 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 jacred && useradd -u 1000 -g jacred -d /data jacred \
    && mkdir /data && chown jacred:jacred /data
COPY --from=build /out/jacred /usr/local/bin/jacred
USER jacred
WORKDIR /data
VOLUME ["/data"]
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]
CMD ["xvfb-run", "-a", "-s", "-screen 0 1920x1080x24", "/usr/local/bin/jacred"]
