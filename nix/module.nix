{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.kiseki;

  tokensFile = pkgs.writeText "kiseki-tokens.json" (
    builtins.toJSON (
      map (a: {
        inherit (a) name host sources;
        token_sha256 = a.tokenSha256;
      }) cfg.agents
    )
  );

  # Shared by the server and the snapshot job.
  hardening = {
    CapabilityBoundingSet = "";
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    NoNewPrivileges = true;
    PrivateDevices = true;
    PrivateTmp = true;
    ProtectClock = true;
    ProtectControlGroups = true;
    ProtectHome = true;
    ProtectHostname = true;
    ProtectKernelLogs = true;
    ProtectKernelModules = true;
    ProtectKernelTunables = true;
    ProtectSystem = "strict";
    RestrictNamespaces = true;
    RestrictRealtime = true;
    SystemCallArchitectures = "native";
    SystemCallFilter = [ "@system-service" ];
    UMask = "0077";
    User = "kiseki";
    Group = "kiseki";
    StateDirectory = "kiseki";
    StateDirectoryMode = "0700";
  };

  dbPath = "/var/lib/kiseki/kiseki.db";
in
{
  options.services.kiseki = {
    enable = lib.mkEnableOption "kiseki lifelog server";

    package = lib.mkOption {
      type = lib.types.package;
      description = "kiseki package to run.";
    };

    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8096";
      description = "Address the HTTP server binds to. Put a TLS-terminating reverse proxy in front of it.";
    };

    readTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        File containing the token for the web interface login. The web
        interface is disabled when null. Read through systemd credentials,
        so it may be owned by root.
      '';
    };

    timezone = lib.mkOption {
      type = lib.types.str;
      default = "Asia/Tokyo";
      description = "IANA time zone that decides which day is today in the web interface.";
    };

    agents = lib.mkOption {
      default = [ ];
      description = ''
        Agents allowed to push records. The token hash is not a secret, so
        these can live in the configuration. Issue one with
        `kiseki token new` and copy the printed entry here.
      '';
      type = lib.types.listOf (
        lib.types.submodule {
          options = {
            name = lib.mkOption {
              type = lib.types.str;
              description = "Label for the token.";
            };
            host = lib.mkOption {
              type = lib.types.str;
              description = "Host name recorded with every record pushed with this token.";
            };
            sources = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              description = "Sources this token may write.";
            };
            tokenSha256 = lib.mkOption {
              type = lib.types.strMatching "[0-9a-f]{64}";
              description = "SHA-256 of the token, in hex, as printed by `kiseki token new`.";
            };
          };
        }
      );
    };

    snapshot = {
      enable = lib.mkEnableOption ''
        the kiseki-snapshot job, which writes a consistent copy of the database
        for a file-level backup to pick up. It is not scheduled; start it from
        the backup job'';

      path = lib.mkOption {
        type = lib.types.str;
        default = "/var/lib/kiseki/snapshots/kiseki.db";
        readOnly = true;
        description = "Where the snapshot is written.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.kiseki = {
      isSystemUser = true;
      group = "kiseki";
    };
    users.groups.kiseki = { };

    systemd.services.kiseki = {
      description = "kiseki lifelog server";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = hardening // {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "serve"
            "-listen"
            cfg.listenAddress
            "-db"
            dbPath
            "-tokens-file"
            tokensFile
            "-timezone"
            cfg.timezone
          ]
          ++ lib.optionals (cfg.readTokenFile != null) [
            "-read-token-file"
            "%d/read-token"
          ]
        );
        LoadCredential = lib.optional (cfg.readTokenFile != null) "read-token:${cfg.readTokenFile}";
        Restart = "on-failure";
        RestartSec = 5;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
      };
    };

    systemd.services.kiseki-snapshot = lib.mkIf cfg.snapshot.enable {
      description = "Write a consistent copy of the kiseki database";
      serviceConfig = hardening // {
        Type = "oneshot";
        ExecStartPre = "${pkgs.coreutils}/bin/mkdir -p ${builtins.dirOf cfg.snapshot.path}";
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.package)
          "snapshot"
          "-db"
          dbPath
          "-out"
          cfg.snapshot.path
        ];
        RestrictAddressFamilies = [ "AF_UNIX" ];
      };
    };
  };
}
