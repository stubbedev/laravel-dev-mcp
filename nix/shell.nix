{
  perSystem = {
    pkgs,
    self',
    ...
  }: {
    devShells.default = pkgs.mkShell {
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
