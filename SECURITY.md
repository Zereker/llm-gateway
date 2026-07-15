[English](SECURITY.md) | [简体中文](SECURITY.zh-CN.md)

# Security Policy

## Supported versions

Before the project publishes its first stable release, security fixes target
the latest release and the default branch. Older commits and unmaintained
deployment examples are not supported independently.

After stable releases begin, this table will list the supported release lines
and their security-maintenance windows.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability or include real API
keys, prompts, provider credentials, database contents, or exploit details in
public discussions.

Report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/Zereker/llm-gateway/security/advisories/new).
Include, when available:

- the affected commit or release;
- the deployment shape and configuration required to reproduce the issue;
- minimal reproduction steps or a proof of concept using synthetic secrets;
- the expected impact and any known mitigations.

The maintainer will acknowledge a complete report as soon as practical,
normally within three business days. Validation, remediation, disclosure, and
release timing depend on severity and reproducibility. Please allow a
reasonable remediation window before public disclosure.

## Security boundaries

`llm-gateway` handles upstream provider credentials and may process sensitive
prompt or response content. Production operators are responsible for TLS
termination, secret management, network policy, persistent storage, database
and broker access control, log retention, and selecting an appropriate content
logging policy. Example and quickstart credentials are development-only.
