# Two images from one build:
#   docker build --target laker       -t laker .
#   docker build --target laker-agent -t laker-agent .
FROM golang:1.27.3 AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOFLAGS=-mod=vendor
RUN go build -trimpath -o /out/laker ./cmd/laker \
 && go build -trimpath -o /out/laker-agent ./cmd/laker-agent

# the agent: runs next to the data, sends it to laker
# config in /etc/laker-agent/config.yaml, key and checkpoints in /var/lib/laker-agent
FROM gcr.io/distroless/static-debian12:nonroot AS laker-agent
COPY --from=build /out/laker-agent /laker-agent
USER nonroot
ENTRYPOINT ["/laker-agent"]
CMD ["serve"]

# the service: lake, search index, web UI, agent API (last: the default target)
# config in /etc/laker/config.yaml, state and index in /var/lib/laker
FROM gcr.io/distroless/static-debian12:nonroot AS laker
COPY --from=build /out/laker /laker
USER nonroot
EXPOSE 8090
ENTRYPOINT ["/laker"]
CMD ["serve"]
