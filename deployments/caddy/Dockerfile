FROM caddy:2-builder AS builder

RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM caddy:2

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
COPY start-caddy.sh /usr/bin/start-caddy

ENTRYPOINT ["/usr/bin/start-caddy"]
CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
