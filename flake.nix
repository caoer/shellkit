{
  description = "A reproducible kit for shell/CLI tooling";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachSystem [
      "aarch64-darwin"
      "x86_64-darwin"
      "aarch64-linux"
      "x86_64-linux"
    ] (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          # Irreducible shell-tooling core.
          packages = with pkgs; [
            just
            git
            shellcheck
            shfmt

            # Go toolchain (shellkit is a Go program; go.mod requires >= 1.26.1).
            go
            gopls
            gotools
            golangci-lint # `just lint` / CI lint; keep local in sync with CI

            # Fast pre-commit quality gate (see lefthook.yml).
            lefthook
          ];

          # Wire the pre-commit hook on shell entry (idempotent, non-fatal).
          shellHook = ''
            lefthook install >/dev/null 2>&1 || true
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      }
    );
}
