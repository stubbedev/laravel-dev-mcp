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
          version = "0.0.12";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and hashes
          # the resulting vendor tree; `vendorHash` pins that hash so the
          # sandboxed build is reproducible. Bump after any `go get` / `go mod
          # tidy` that changes go.sum — `nix build` prints the expected hash on
          # mismatch, or run `just sync-flake`.
          # go-sum: 6a61e71b0ec64fa1fb28f0309c132c899ee8a7c203caa102ce6431e10825ad76
          vendorHash = "sha256-8Wjmrz/tvhoP0z8uKfPQKk41aGFX8PcP14duoXXqhtc=";
          subPackages = [ "." ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/laravel-dev-mcp/version.Version=0.0.12"
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
