{
  description = "laravel-dev-mcp — MCP server for local Laravel development (Go)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        laravel-dev-mcp = pkgs.buildGoModule {
          pname = "laravel-dev-mcp";
          version = "0.0.10";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and hashes
          # the resulting vendor tree; `vendorHash` pins that hash so the
          # sandboxed build is reproducible. Bump after any `go get` / `go mod
          # tidy` that changes go.sum — `nix build` prints the expected hash on
          # mismatch, or run `just sync-flake`.
          # go-sum: ead6c61b049894264997b14a7139f67263873b22720a3c70b736f7d2248b0173
          vendorHash = "sha256-1JfIMVibtsXDYtydwiW/omscnQf/Ciua2vqa0WveQuQ=";
          subPackages = [ "." ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/laravel-dev-mcp/version.Version=0.0.10"
          ];
          doCheck = true;

          meta = with pkgs.lib; {
            description = "MCP server for local Laravel development (DB, logs, routes, config, models, Telescope)";
            homepage = "https://github.com/stubbedev/laravel-dev-mcp";
            license = licenses.mit;
            mainProgram = "laravel-dev-mcp";
            platforms = platforms.unix;
          };
        };
      in
      {
        packages = {
          default = laravel-dev-mcp;
          laravel-dev-mcp = laravel-dev-mcp;
        };

        apps.default = {
          type = "app";
          program = "${laravel-dev-mcp}/bin/laravel-dev-mcp";
          meta = laravel-dev-mcp.meta;
        };

        checks.build = laravel-dev-mcp;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            golangci-lint
            just
            git
          ];
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
