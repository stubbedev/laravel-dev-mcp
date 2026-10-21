{
  perSystem = {
    pkgs,
    self',
    ...
  }: {
    devShells.default = pkgs.mkShell {
      # Go toolchain from the package: buildGo127Module pins the version
      # go.mod declares. The default `go` attribute lags behind, and with
      # GOTOOLCHAIN=auto it would silently download its own toolchain
      # instead of failing. gopls and golangci-lint are built with the same
      # nixpkgs Go, so they parse what the compiler accepts.
      inputsFrom = [self'.packages.default];

      packages = with pkgs; [
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
  };
}
