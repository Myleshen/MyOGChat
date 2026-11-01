# Security policy

MyOGChat is self-hosted software; there is no hosted service to report issues
against. If you run your own instance, you are responsible for its deployment
(TLS/reverse proxy, host hardening, key management).

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's
[private vulnerability reporting](https://github.com/Myleshen/MyOGChat/security/advisories/new)
or email the maintainer at the address in the git history.

Include the affected version/commit, a description, and a minimal reproduction.
You can expect an initial response within a week.

## Scope notes for this codebase

- Per-user upstream API keys and integration tokens are encrypted at rest with
  AES-GCM (`internal/secret`); the encryption key lives in the SQLite
  `settings` table and is generated on first run.
- `fetch_url` refuses loopback/private/link-local targets (SSRF guard), but
  user-configured integration URLs (ComfyUI, Linkwarden, mail, CalDAV) are
  trusted operator configuration and are not filtered.
- The bash/python sandbox is a convenience feature, not a container boundary.
  Leave `sandbox_enabled` off on untrusted multi-user deployments.
- Keep secrets in the environment file (mode `600`) or the per-user settings —
  never in the repository.
