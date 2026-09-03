# Builds the real reloop binary, installs it into a clean Linux image,
# and drives it with the e2e harness, the same path a user takes on a
# fresh box. Run from the repo root:
#
#   docker build -f tests/e2e/Dockerfile -t reloop-e2e .
#   docker run --rm reloop-e2e

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -o /out/reloop ./cmd/reloop

FROM debian:bookworm-slim
# procps supplies pkill for the harness's crash-orphan cleanup.
RUN apt-get update && apt-get install -y --no-install-recommends procps && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/reloop /usr/local/bin/reloop
COPY tests/e2e/run.sh /usr/local/share/reloop/e2e.sh
ENV RELOOP_BIN=/usr/local/bin/reloop
CMD ["bash", "/usr/local/share/reloop/e2e.sh"]
