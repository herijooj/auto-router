{
  description = "Auto Router for llama-swap that automatically selects available models";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {nixpkgs, ...}: let
    systems = ["x86_64-linux" "aarch64-linux"];
    forAllSystems = fn:
      nixpkgs.lib.genAttrs systems (system:
        fn nixpkgs.legacyPackages.${system});
  in {
    packages = forAllSystems (pkgs: {
      default = pkgs.buildGoModule {
        pname = "auto-router";
        version = "1.0.0";
        src = ./.;
        vendorHash = null;
        ldflags = ["-s" "-w"];
        meta = with pkgs.lib; {
          description = "Auto router for llama-swap that automatically selects available models";
          license = licenses.mit;
          maintainers = [];
        };
      };
    });

    nixosModules.default = import ./module.nix;
  };
}
