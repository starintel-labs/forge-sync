{
  description = "Deterministic GitHub <-> Forgejo synchronization service";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        forge-sync = pkgs.buildGo124Module {
          pname = "forge-sync";
          version = "0.3.0";
          src = pkgs.lib.cleanSource self;
          vendorHash = "sha256-ZG+eQhOHW5J1WLm2WZ57ywXA+NobgMorRJkR2Mkb2fY=";
          doCheck = true;
          nativeBuildInputs = [ pkgs.git ];
          checkInputs = [ pkgs.git ];
          meta = with pkgs.lib; {
            description = "Deterministic bidirectional GitHub <-> Forgejo synchronization daemon";
            license = licenses.mit;
            mainProgram = "forge-sync";
          };
        };
      in
      {
        packages = {
          inherit forge-sync;
          default = forge-sync;
          docker = pkgs.dockerTools.buildLayeredImage {
            name = "forge-sync";
            tag = "latest";
            contents = [
              forge-sync
              pkgs.git
              pkgs.cacert
              pkgs.tzdata
            ];
            config = {
              Entrypoint = [ "${forge-sync}/bin/forge-sync" ];
              Cmd = [ "serve" ];
              User = "900:900";
              Env = [ "FORGE_SYNC_STATE_PATH=/var/lib/forge-sync/forge-sync.db" ];
              ExposedPorts = { "8080/tcp" = { }; };
            };
          };
        };
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [ go_1_24 gopls gotools git ];
        };
      });
}
