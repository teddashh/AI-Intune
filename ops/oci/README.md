# OCI Autopilot pack

`provision.sh` creates one Ubuntu Ampere VM and starts the Docker + Caddy Hub. `install-hub.sh` does the same host setup on a VM you already have. `host-setup.sh` is what both run on the VM.

Read [docs/DEPLOY-OCI.md](../../docs/DEPLOY-OCI.md). Agents follow [skills/clawctl-oci-deploy/SKILL.md](../../skills/clawctl-oci-deploy/SKILL.md).

```sh
bash ops/test-oci.sh
```

That uses a stub `oci` and does not create cloud resources.
