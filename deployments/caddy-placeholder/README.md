# Caddy Placeholder

Caddy is intentionally not part of the initial Compose stack.

When production deployment is clearer, add either:

- a separate production Compose override that includes Caddy, or
- host-level Caddy config outside this public repo.

Production Caddy config should avoid committing private hostnames, email addresses, or certificate-related secrets unless they are generic examples.
