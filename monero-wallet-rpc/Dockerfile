# monero-wallet-rpc, one binary per container.
# The cryptopay container itself stays a single Go binary; this
# service is a second container in the same compose stack, started
# only when Monero is enabled (docker-compose.monero.yml).
# It downloads the official Monero CLI for the build arch and
# verifies its SHA256 against the hashes published at
# https://www.getmonero.org/downloads/hashes.txt (pinned here for
# v0.18.5.4, so a future release must update both the version and
# the hashes below, never just the version).
FROM debian:bookworm-slim
ARG MONERO_VERSION=0.18.5.4
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
    bzip2 ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN set -e; \
  case "$TARGETARCH" in \
    amd64) SUFFIX=x64; HASH=ba2928962c101b609434bf8c1b43ad59ebe96c5525977284ca599d8bf0f02d10;; \
    arm64) SUFFIX=armv8; HASH=d20d71e6522b41eb8e549c3425f7f4622c9c9a448fc7147525393fd0a6b59703;; \
    arm) SUFFIX=armv7; HASH=124883ebd8b3e19fb9000bc051dd9a28958a1a383455af78b539362b0d59094f;; \
    *) echo "Unsupported arch for Monero: $TARGETARCH" >&2; exit 1;; \
  esac; \
  FILE="monero-linux-${SUFFIX}-v${MONERO_VERSION}.tar.bz2"; \
  curl -fsSL -o "/tmp/$FILE" "https://downloads.getmonero.org/cli/$FILE"; \
  echo "$HASH  /tmp/$FILE" | sha256sum -c -; \
  tar -xjf "/tmp/$FILE" -C /tmp; \
  cp /tmp/monero-*/monero-wallet-rpc /usr/local/bin/; \
  cp /tmp/monero-*/monero-wallet-cli /usr/local/bin/; \
  rm -rf "/tmp/$FILE" /tmp/monero-*
EXPOSE 18088
CMD ["monero-wallet-rpc"]
