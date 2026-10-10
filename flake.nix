{
  description = "Personal lifelog collection and archive";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gitleaks
            gopls
            sqlite
          ];
          env.CGO_ENABLED = "0";
        };
      });

      nixosModules.default =
        { pkgs, ... }:
        {
          imports = [ ./nix/module.nix ];
          services.kiseki.package =
            nixpkgs.lib.mkDefault
              self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };

      homeManagerModules.kiseki-agent =
        { pkgs, ... }:
        {
          imports = [ ./nix/agent-module.nix ];
          programs.kiseki-agent.package =
            nixpkgs.lib.mkDefault
              self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
