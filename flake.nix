{
  description = "A reproducible kit for shell/CLI tooling";

  # Embedded shellkit-runner binaries (see internal/rundaemon/embed.go, U7):
  # `just build-runners` cross-compiles 4 static targets (linux/amd64+arm64,
  # darwin/amd64+arm64) with CGO_ENABLED=0 — required because target hosts
  # include NixOS, which has no /lib64/ld-linux for a dynamically linked
  # binary. Each binary is gzip-compressed (~1.5MB) and go:embed'd into the
  # daemon; ~6-7MB gz total pushes the daemon binary to ~20MB. If the runner
  # is ever distributed via a Nix binary cache, omit `inputs.nixpkgs.follows`
  # on any consuming flake input — pinning it silently causes cache misses.
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
