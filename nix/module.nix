{ config, lib, pkgs, ... }:

let
  cfg = config.services.forge-sync;
  inherit (lib) mkEnableOption mkIf mkOption types;

  secretEnvironmentNames = [
    "FORGE_SYNC_GITHUB_TOKEN"
    "FORGE_SYNC_FORGEJO_TOKEN"
    "FORGE_SYNC_GITHUB_WEBHOOK_SECRET"
    "FORGE_SYNC_FORGEJO_WEBHOOK_SECRET"
  ];
in
{
  options.services.forge-sync = {
    enable = mkEnableOption "forge-sync GitHub <-> Forgejo synchronization daemon";

    package = mkOption {
      type = types.package;
      description = "forge-sync package to run.";
    };

    environmentFile = mkOption {
      type = types.str;
      example = "/run/secrets/forge-sync.env";
      description = ''
        Absolute path to a systemd EnvironmentFile containing runtime secrets.
        Keep this file outside the Nix store. At minimum it should provide
        FORGE_SYNC_GITHUB_TOKEN, FORGE_SYNC_FORGEJO_TOKEN,
        FORGE_SYNC_GITHUB_WEBHOOK_SECRET, and FORGE_SYNC_FORGEJO_WEBHOOK_SECRET.
      '';
    };

    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      example = {
        FORGE_SYNC_FORGEJO_API = "https://forge.example.org";
        FORGE_SYNC_NAMESPACES = "starintel-labs,lost-rob0t";
      };
      description = ''
        Non-secret environment variables passed to forge-sync. Secret token and
        webhook variables are rejected here because Nix option values may be
        copied into the Nix store; use environmentFile for secrets instead.
      '';
    };

    user = mkOption {
      type = types.str;
      default = "forge-sync";
      description = "System user that runs forge-sync.";
    };

    group = mkOption {
      type = types.str;
      default = "forge-sync";
      description = "System group that runs forge-sync.";
    };

    stateDirectory = mkOption {
      type = types.strMatching "[A-Za-z0-9_.-]+";
      default = "forge-sync";
      description = "Name of the systemd-managed directory under /var/lib.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = lib.hasPrefix "/" cfg.environmentFile;
        message = "services.forge-sync.environmentFile must be an absolute runtime path.";
      }
      {
        assertion = !(lib.hasPrefix "/nix/store/" cfg.environmentFile);
        message = "services.forge-sync.environmentFile must not point into the Nix store.";
      }
      {
        assertion = lib.all (name: !(builtins.hasAttr name cfg.environment)) secretEnvironmentNames;
        message = "forge-sync secrets must be supplied through services.forge-sync.environmentFile, not services.forge-sync.environment.";
      }
    ];

    users.groups.${cfg.group} = { };
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      home = "/var/lib/${cfg.stateDirectory}";
    };

    systemd.services.forge-sync = {
      description = "Deterministic GitHub <-> Forgejo synchronization service";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];

      path = [ pkgs.git ];

      environment = {
        FORGE_SYNC_STATE_PATH = "/var/lib/${cfg.stateDirectory}/forge-sync.db";
      } // cfg.environment;

      serviceConfig = {
        Type = "simple";
        ExecStart = "${lib.getExe cfg.package} serve";
        User = cfg.user;
        Group = cfg.group;
        EnvironmentFile = cfg.environmentFile;

        StateDirectory = cfg.stateDirectory;
        StateDirectoryMode = "0750";

        Restart = "on-failure";
        RestartSec = "5s";

        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        LockPersonality = true;
        RestrictSUIDSGID = true;
        CapabilityBoundingSet = "";
      };
    };
  };
}
