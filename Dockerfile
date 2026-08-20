# The image exists because most people who run something like this run it in
# compose, not because lookout needs a container: the binary is static and has
# no runtime dependency beyond a CA bundle.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/lookout ./cmd/lookout

FROM scratch
# Probes speak TLS, so the roots have to come along. The timezone database is
# compiled into the binary already, so `timezone:` works with nothing else here.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/lookout /lookout

# Nobody, numerically. The state directory has to be a volume the host makes
# writable for this uid; there is no shell in here to chown anything.
USER 65534:65534
EXPOSE 5665
VOLUME /var/lib/lookout

ENTRYPOINT ["/lookout"]
CMD ["run", "/etc/lookout/config.yaml"]
