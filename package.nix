{
  lib,
  buildGo127Module,
}:
buildGo127Module rec {
  pname = "laravel-dev-mcp";
  # Release-managed: `just sync-flake <version>` rewrites this on a tag bump.
  version = "0.0.14";

  src = lib.cleanSource ./.;

  # buildGoModule fetches Go deps through the module proxy and hashes the
  # resulting vendor tree; `vendorHash` pins that hash so the sandboxed build
  # is reproducible. Bump after any `go get` / `go mod tidy` that changes
  # go.sum: `nix build` prints the expected hash on mismatch, or run
  # `just sync-flake`.
  # go-sum: 9b5282025b229dbff586348567b634e09cdb2edfa736acced49d90b0a9470c67
  vendorHash = "sha256-cz2Kqm983dF2VGP1gDhYXQoqt6QIwh+4QMQtaEcmfG4=";

  subPackages = ["."];

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X github.com/stubbedev/laravel-dev-mcp/version.Version=${version}"
  ];

  doCheck = true;

  meta = {
    description = "MCP server for local Laravel development (DB, logs, routes, config, models, Telescope)";
    homepage = "https://github.com/stubbedev/laravel-dev-mcp";
    license = lib.licenses.mit;
    mainProgram = "laravel-dev-mcp";
    platforms = lib.platforms.unix;
  };
}
