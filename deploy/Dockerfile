# syntax=docker/dockerfile:1
# NOTE: Use --network host on Linux for accurate RTT measurements.
# Bridge networking introduces NAT that distorts results.
FROM scratch
COPY tinytwamp /tinytwamp
ENTRYPOINT ["/tinytwamp", "-mode", "agent"]
