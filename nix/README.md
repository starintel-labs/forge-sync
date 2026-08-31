# NixOS module

The flake exports `nixosModules.default` and `nixosModules.forge-sync`.

```nix
{
  inputs.forge-sync.url = "github:starintel-labs/forge-sync";

  outputs = { self, nixpkgs, forge-sync, ... }: {
    nixosConfigurations.sync-host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        forge-sync.nixosModules.default
        {
          services.forge-sync = {
            enable = true;
            environmentFile = "/run/secrets/forge-sync.env";
            environment = {
              FORGE_SYNC_FORGEJO_API = "https://forge.example.org";
              FORGE_SYNC_NAMESPACES = "starintel-labs,lost-rob0t";
              FORGE_SYNC_LISTEN_ADDR = "127.0.0.1:8080";
            };
          };
        }
      ];
    };
  };
}
```

`environmentFile` is deliberately a string path, not a Nix path. Point it at a
runtime-managed secret file such as an agenix/sops-nix output under
`/run/secrets`, or another root-readable file outside `/nix/store`.

The file uses systemd `EnvironmentFile` syntax:

```sh
FORGE_SYNC_GITHUB_TOKEN=github-token-here
FORGE_SYNC_FORGEJO_TOKEN=forgejo-token-here
FORGE_SYNC_GITHUB_WEBHOOK_SECRET=github-webhook-secret-here
FORGE_SYNC_FORGEJO_WEBHOOK_SECRET=forgejo-webhook-secret-here
```

Do **not** put token values in `services.forge-sync.environment`; Nix values can
become visible in the Nix store. That option is intended for non-secret runtime
configuration only.

The module creates the `forge-sync` system user/group, manages
`/var/lib/forge-sync` through systemd `StateDirectory`, starts the daemon after
`network-online.target`, restarts it on failure, and applies a basic systemd
hardening profile.
