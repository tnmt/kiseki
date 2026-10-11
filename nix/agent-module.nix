# Home Manager module that runs `kiseki agent` on a timer.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.programs.kiseki-agent;
  json = pkgs.formats.json { };

  settings = {
    token_file = cfg.tokenFile;
    spool_dir = cfg.spoolDir;
    timezone = cfg.timezone;
    overlap_hours = cfg.overlapHours;
  }
  // lib.optionalAttrs (cfg.server != null) { server = cfg.server; }
  // lib.optionalAttrs (cfg.serverFile != null) { server_file = cfg.serverFile; }
  // lib.optionalAttrs cfg.browserHistory.enable {
    browser_history = {
      browsers = cfg.browserHistory.browsers;
      history_paths = cfg.browserHistory.historyPaths;
      exclude_profiles = cfg.browserHistory.excludeProfiles;
      exclude_domains = cfg.browserHistory.excludeDomains;
    };
  }
  // lib.optionalAttrs cfg.claudeCode.enable {
    claude_code = {
      projects_dir = cfg.claudeCode.projectsDir;
      exclude_cwd_prefixes = cfg.claudeCode.excludeCwdPrefixes;
    };
  };

  configFile = json.generate "kiseki-agent.json" settings;
  command = [
    (lib.getExe cfg.package)
    "agent"
    "-config"
    "${configFile}"
  ];
in
{
  options.programs.kiseki-agent = {
    enable = lib.mkEnableOption "the kiseki agent";

    package = lib.mkOption {
      type = lib.types.package;
      description = "kiseki package to run.";
    };

    server = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "https://kiseki.example.com";
      description = "Base URL of the kiseki server. Set this or `serverFile`.";
    };

    serverFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Path of a file holding the server's base URL, read at run time.
        Use it instead of `server` to keep the URL out of the Nix store
        and of a public configuration repository.
      '';
    };

    tokenFile = lib.mkOption {
      type = lib.types.str;
      description = ''
        Path of the file holding this agent's token, as printed once by
        `kiseki token new`. A runtime path: it must not be a store path.
      '';
    };

    spoolDir = lib.mkOption {
      type = lib.types.str;
      default = "${config.xdg.stateHome}/kiseki";
      defaultText = lib.literalExpression ''"''${config.xdg.stateHome}/kiseki"'';
      description = "Where undelivered batches and the agent's state are kept.";
    };

    timezone = lib.mkOption {
      type = lib.types.str;
      default = "Asia/Tokyo";
      description = "IANA time zone recorded with every record.";
    };

    overlapHours = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 48;
      description = "How far back to resend, to cover late-arriving data and server restores.";
    };

    interval = lib.mkOption {
      type = lib.types.ints.positive;
      default = 15;
      description = "Minutes between runs.";
    };

    browserHistory = {
      enable = lib.mkEnableOption "collecting Chromium browser history" // {
        default = true;
      };
      browsers = lib.mkOption {
        type = lib.types.listOf (
          lib.types.enum [
            "brave-origin"
            "chrome"
          ]
        );
        default = [ ];
        description = "Browsers whose profiles are found automatically.";
      };
      historyPaths = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "History files in non-default locations.";
      };
      excludeProfiles = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "brave-origin/Profile 1" ];
        description = ''
          Profiles that are never sent, as `<browser>/<profile directory>`.
          The directory is shown as the profile path on brave://version or
          chrome://version.
        '';
      };
      excludeDomains = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Domains (and their subdomains) that are never sent.";
      };
    };

    claudeCode = {
      enable = lib.mkEnableOption "collecting Claude Code sessions";
      projectsDir = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Defaults to ~/.claude/projects when empty.";
      };
      excludeCwdPrefixes = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Sessions run at or under these directories are never sent.";
      };
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      {
        assertions = [
          {
            assertion = (cfg.server == null) != (cfg.serverFile == null);
            message = "programs.kiseki-agent: set exactly one of server and serverFile.";
          }
        ];
      }

      (lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
        systemd.user.services.kiseki-agent = {
          Unit = {
            Description = "kiseki agent";
            After = [ "network-online.target" ];
          };
          Service = {
            Type = "oneshot";
            ExecStart = lib.escapeShellArgs command;
          };
        };
        systemd.user.timers.kiseki-agent = {
          Unit.Description = "Run the kiseki agent periodically";
          Timer = {
            OnBootSec = "2min";
            OnUnitActiveSec = "${toString cfg.interval}min";
            RandomizedDelaySec = "1min";
          };
          Install.WantedBy = [ "timers.target" ];
        };
      })

      (lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
        launchd.agents.kiseki-agent = {
          enable = true;
          config = {
            ProgramArguments = command;
            StartInterval = cfg.interval * 60;
            RunAtLoad = true;
            StandardErrorPath = "${cfg.spoolDir}/agent.log";
          };
        };
      })
    ]
  );
}
