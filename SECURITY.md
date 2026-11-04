# Security

**MicroVM Sandbox is a research and teaching project. It has not been security audited.** Do not use it to protect real secrets or to run untrusted code for other people in production.

## What it is designed to defend against

See [docs/threat-model.md](docs/threat-model.md) for the attacker model, the trust boundaries, the mitigations and what is out of scope. Isolation rests on KVM and Firecracker, with the Firecracker jailer as a second layer. This project does not change either of them.

## Reporting a problem

If you find a way out of the sandbox or another security issue, please open a GitHub security advisory on this repository instead of a public issue. Include the policy file, the command and the audit record of the run.
