{
  description = "laravel-dev-mcp - MCP server for local Laravel development (Go)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
  }:
    flake-utils.lib.eachDefaultSystem (
      system: let
        pkgs = nixpkgs.legacyPackages.${system};
      in {
        packages = rec {
          laravel-dev-mcp = pkgs.callPackage ./package.nix {};
          default = laravel-dev-mcp;
        };

        apps = rec {
          laravel-dev-mcp = flake-utils.lib.mkApp {drv = self.packages.${system}.laravel-dev-mcp;};
          default = laravel-dev-mcp;
        };

        checks.build = self.packages.${system}.laravel-dev-mcp;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            # Go toolchain. Pinned to the version go.mod declares: the
            # default `go` attribute lags behind, and with GOTOOLCHAIN=auto
            # it would silently download its own toolchain instead of
            # failing. gopls and golangci-lint are built with the same
            # nixpkgs Go, so they parse what the compiler accepts.
            go_1_27

            # Development tools
            gopls # Go language server
            golangci-lint # Linter behind `just lint`, config in .golangci.yml
            delve # Go debugger
            just # Task runner

            # Development workflow
            git # Version control
            gh # GitHub CLI
          ];

          shellHook = ''
            # Keep accidental cgo out, and keep the compiler the flake
            # provides: a module asking for a newer Go fails instead of
            # downloading one at first use.
            export CGO_ENABLED=0 GOTOOLCHAIN=local
          '';
        };

        formatter = pkgs.alejandra;
      }
    );
}
