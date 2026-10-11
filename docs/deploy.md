# Deploying kiseki

The server runs as a NixOS module. The agent runs on each machine from a Home Manager module, started by a timer.

## Overview

```
[machine] kiseki-agent.timer ─ VPN ─▶ reverse proxy (https://kiseki.example.com)
                                         └▶ kiseki.service (127.0.0.1:8096) ─▶ /var/lib/kiseki/kiseki.db
                                                                                │
          kiseki-snapshot.service ─▶ /var/lib/kiseki/snapshots/kiseki.db ◀─ restic-backups-kiseki (daily)
                                                                                └▶ restic repository (encrypted)
```

The flake provides:

| Output | Content |
| --- | --- |
| `packages.<system>.default` | The `kiseki` binary (`CGO_ENABLED=0`) |
| `nixosModules.default` | `services.kiseki` (server, token definitions, snapshot) |
| `homeManagerModules.kiseki-agent` | `programs.kiseki-agent` (a systemd timer on Linux, launchd on macOS) |

## Design choices

- **Back up a snapshot**: restic runs as root, and reading the live database directly can make root create the WAL `-shm` file and break kiseki's writes. `kiseki snapshot` uses `VACUUM INTO` to write a consistent copy, even while the server is running, as the `kiseki` user. Give the backup job `Requires` and `After` on `kiseki-snapshot.service` so every backup gets a fresh copy.
- **A static `kiseki` user**: the server and the snapshot job share the state directory, so the module uses a static system user instead of `DynamicUser`.
- **Token hashes live in the Nix configuration**: paste the entries printed by `kiseki token new` into `services.kiseki.agents`. The hashes are not secret.

## Status

- On a NixOS server: the server starts, the web login works and restic backups succeed.
- The agent's Home Manager module builds on Linux, and `kiseki agent` reads the generated configuration.
- macOS (launchd) has only been evaluated, not built or run.
- Restoring from a restic backup has not been tested.

## Steps

### 1. Add the flake input

```nix
inputs.kiseki = {
  url = "github:tnmt/kiseki";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

### 2. Prepare secrets

Create a read token for the web login and a password for the restic repository, and put them on the server with sops-nix or similar.

```
openssl rand -base64 32    # read token
openssl rand -base64 32    # restic password
```

Without the restic password the backup cannot be restored. Keep a copy outside the server.

### 3. Issue agent tokens

```
nix run github:tnmt/kiseki -- token new -name laptop -host laptop -sources browser-history,claude-code
```

- Save the raw token printed to standard error as `~/.config/kiseki/token` on that machine (`chmod 600`). It cannot be shown again.
- Paste the JSON printed to standard output into `services.kiseki.agents`, converting the keys to Nix attribute names.

### 4. Configure the server

```nix
{ config, inputs, pkgs, ... }:
{
  imports = [ inputs.kiseki.nixosModules.default ];

  services.kiseki = {
    enable = true;
    readTokenFile = config.sops.secrets.kiseki_read_token.path;
    snapshot.enable = true;
    agents = [
      {
        name = "laptop";
        host = "laptop";
        sources = [ "browser-history" "claude-code" ];
        tokenSha256 = "…";
      }
    ];
  };

  services.restic.backups.kiseki = {
    initialize = true;
    repository = "rclone:remote:/kiseki";
    passwordFile = config.sops.secrets.restic_kiseki_password.path;
    rcloneConfigFile = config.sops.secrets.rclone_config.path;
    paths = [ (builtins.dirOf config.services.kiseki.snapshot.path) ];
    timerConfig = {
      OnCalendar = "daily";
      Persistent = true;
    };
  };

  systemd.services.restic-backups-kiseki = {
    path = [ pkgs.rclone ];
    requires = [ "kiseki-snapshot.service" ];
    after = [ "kiseki-snapshot.service" ];
  };
}
```

Put a TLS-terminating reverse proxy in front of `127.0.0.1:8096`. The server accepts requests up to 8 MiB, so with nginx set `client_max_body_size` above that (for example `16m`).

To collect Withings measurements, set `services.kiseki.withings` and authorize once as described in [the Withings collector](withings.md).

### 5. Check the server

```
systemctl status kiseki
curl -s https://kiseki.example.com/healthz
```

Open `https://kiseki.example.com/` in a browser and log in with the read token.

### 6. Install the agent on each machine

Add this to the Home Manager configuration:

```nix
{ inputs, ... }:
{
  imports = [ inputs.kiseki.homeManagerModules.kiseki-agent ];

  programs.kiseki-agent = {
    enable = true;
    server = "https://kiseki.example.com";
    tokenFile = "/home/user/.config/kiseki/token";
    browserHistory.browsers = [ "brave-origin" ];
    claudeCode = {
      enable = true;
      excludeCwdPrefixes = [ ];
    };
  };
}
```

After switching, run it once by hand and check the result:

```
systemctl --user start kiseki-agent
journalctl --user -u kiseki-agent -e
```

The first run sends the whole history and takes some tens of seconds.

To keep the server URL out of a public configuration, set `serverFile` to a file holding the URL (decrypted by sops-nix, for example) instead of `server`.

### 7. Check the backup

```
sudo systemctl start restic-backups-kiseki
sudo journalctl -u restic-backups-kiseki -e
```

To test a restore, extract it with `restic restore latest --target /tmp/kiseki-restore` and count the records with `sqlite3 /tmp/kiseki-restore/var/lib/kiseki/snapshots/kiseki.db 'select count(*) from records'`.
