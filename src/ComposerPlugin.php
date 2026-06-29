<?php

namespace Stubbedev\LaravelDevMcp;

use Composer\Composer;
use Composer\EventDispatcher\EventSubscriberInterface;
use Composer\IO\IOInterface;
use Composer\Plugin\PluginInterface;
use Composer\Script\Event;
use Composer\Script\ScriptEvents;

// On install/update of this package, fetch the right-arch native binary and wire
// it into the project's .mcp.json so the MCP client runs the binary directly.
// PHP is the installer, not the runtime — nothing wraps the binary's execution.
final class ComposerPlugin implements PluginInterface, EventSubscriberInterface
{
    private Composer $composer;
    private IOInterface $io;

    public function activate(Composer $composer, IOInterface $io): void
    {
        $this->composer = $composer;
        $this->io = $io;
    }

    public function deactivate(Composer $composer, IOInterface $io): void {}

    public function uninstall(Composer $composer, IOInterface $io): void {}

    public static function getSubscribedEvents(): array
    {
        return [
            ScriptEvents::POST_INSTALL_CMD => 'onComposerEvent',
            ScriptEvents::POST_UPDATE_CMD => 'onComposerEvent',
        ];
    }

    public function onComposerEvent(Event $event): void
    {
        if (getenv('LARAVEL_MCP_SKIP_DOWNLOAD') === '1') {
            return;
        }
        try {
            $bin = Binary::ensure();
        } catch (\Throwable $e) {
            $this->io->writeError("<warning>[laravel-dev-mcp] {$e->getMessage()}</warning>");
            $this->io->writeError("<warning>[laravel-dev-mcp] Re-run 'composer install' once online to fetch the binary.</warning>");
            return;
        }
        $this->writeMcpConfig($bin);
    }

    private function writeMcpConfig(string $bin): void
    {
        $vendorDir = (string) $this->composer->getConfig()->get('vendor-dir');
        $projectRoot = dirname($vendorDir);
        $configPath = $projectRoot . '/.mcp.json';

        // Relative path: the committed .mcp.json then resolves on every machine
        // that runs `composer install` (the binary lives in the vendor dir).
        $command = $this->relativePath($projectRoot, $bin);

        $data = [];
        if (is_file($configPath)) {
            $decoded = json_decode((string) file_get_contents($configPath), true);
            if (is_array($decoded)) {
                $data = $decoded;
            }
        }
        if (($data['mcpServers']['laravel-dev']['command'] ?? null) === $command) {
            return; // already configured — avoid rewriting the file
        }
        $data['mcpServers']['laravel-dev'] = ['command' => $command];

        file_put_contents(
            $configPath,
            json_encode($data, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES) . "\n"
        );
        $this->io->write("<info>[laravel-dev-mcp] Registered 'laravel-dev' in .mcp.json ({$command})</info>");
    }

    private function relativePath(string $base, string $path): string
    {
        $base = rtrim($base, '/') . '/';
        if (str_starts_with($path, $base)) {
            return substr($path, strlen($base));
        }
        return $path; // outside the project (e.g. a global install) — keep absolute
    }
}
