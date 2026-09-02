# Security

## Supported version

Only the latest release is supported.

## Deployment guidance

Codex Remote Win can control a local Codex Desktop session and must be treated as a privileged local service.

- Do not expose port `8787` directly to the public Internet.
- Put FRP behind TLS, authentication and source-IP restrictions.
- Prefer an HTTPS reverse proxy for browser access.
- Keep the Windows desktop account protected and do not run the service on a shared machine.
- Delete the local `data` directory to revoke all remembered browser sessions.

## Reporting a vulnerability

Open a GitHub security advisory or contact the repository owner privately. Do not include active tokens, pairing data, uploaded private files or public exploit details in an issue.
