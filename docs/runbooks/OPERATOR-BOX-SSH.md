# Operator box SSH

A box running tailscaled with `--tun=userspace-networking` has no kernel route
to tailnet IPs. Use `tailscale nc` through its LocalAPI socket as the SSH transport.
This uses the remote host's OpenSSH service, not Tailscale SSH.

```bash
bash ops/fleet/net/ssh-config-tailscale-nc.sh \
  --name host-a --ip 100.64.0.10 --user ops \
  --identity "$HOME/.ssh/id_ed25519" \
  --socket /var/run/tailscale/tailscaled.sock
```

Review the block, then repeat with `--write`. It writes `~/.ssh/config` mode 0600
between `# >>> fleet:host-a` and `# <<< fleet:host-a`. Existing config receives a
UTC-stamped backup. Repeat writes replace that block without duplication.
`--config FILE` selects another file. Paths for the identity and socket must be
absolute and contain only letters, digits, underscores, dots, slashes, or hyphens.
The block sets User, IdentityFile, IdentitiesOnly, StrictHostKeyChecking accept-new,
and `ProxyCommand /usr/bin/tailscale --socket=... nc %h %p`.
New blocks precede existing defaults, since SSH uses the first value it finds.

```bash
bash ops/fleet/net/ssh-config-tailscale-nc.sh --check host-a
ssh host-a
```

The check runs a batch SSH command with a ten-second connection timeout.
An unmarked `Host host-a` is refused. Use `--rename-existing host-old` to rename
that existing Host entry while creating the managed `host-a` block. Other aliases
on the same Host line are kept. Review included config files for conflicting aliases.

After the box sleeps, `tailscale ping` can succeed via DERP while `tailscale nc`
stalls. Check the socket and tailscaled health. Keep a separately configured
Cloudflare-tunnel jump host as a fallback. `--jump-fallback host-b` prints a note;
it does not activate or install a tunnel. In a separate alias, use the same target
identity and `ProxyJump host-b`, without ProxyCommand. Configure and test the jump
host's SSH transport first. Do not put ProxyJump and this ProxyCommand together.
