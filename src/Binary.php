<?php

namespace Stubbedev\LaravelDevMcp;

// Resolves and downloads the prebuilt Go binary matching this platform, from the
// matching GitHub release. PHP is used ONLY to fetch the right-arch binary at
// install time — the MCP client then execs the native binary directly, no PHP
// in the runtime path.
final class Binary
{
    public const REPO = 'stubbedev/laravel-dev-mcp';

    // PHP_OS_FAMILY:arch -> release asset os/arch. Only the built targets are
    // listed; anything else throws with the go-install hint.
    private const PLATFORMS = [
        'Linux:amd64'  => ['linux',  'amd64'],
        'Linux:arm64'  => ['linux',  'arm64'],
        'Darwin:amd64' => ['darwin', 'amd64'],
        'Darwin:arm64' => ['darwin', 'arm64'],
    ];

    // Cached inside the package dir (gitignored, like the npm wrapper's copy).
    public static function path(): string
    {
        return dirname(__DIR__) . '/bin/laravel-dev-mcp';
    }

    private static function target(): array
    {
        $m = strtolower(php_uname('m'));
        $arch = match (true) {
            in_array($m, ['x86_64', 'amd64'], true) => 'amd64',
            in_array($m, ['arm64', 'aarch64'], true) => 'arm64',
            default => $m,
        };
        $key = PHP_OS_FAMILY . ":$arch";
        if (!isset(self::PLATFORMS[$key])) {
            throw new \RuntimeException(
                "Unsupported platform $key. Build from source with: go install github.com/" . self::REPO . "@latest"
            );
        }
        return self::PLATFORMS[$key];
    }

    // Installed package version, so the download is reproducible (a pinned tag
    // gets that tag's binary). Null => fetch the "latest" release.
    private static function version(): ?string
    {
        if (class_exists(\Composer\InstalledVersions::class)) {
            try {
                $v = \Composer\InstalledVersions::getPrettyVersion(self::REPO);
                if (is_string($v) && preg_match('/\d+\.\d+\.\d+/', $v, $m)) {
                    return $m[0];
                }
            } catch (\OutOfBoundsException) {
                // not registered (source checkout) — fall through to "latest"
            }
        }
        return null;
    }

    private static function assetUrl(): string
    {
        [$os, $arch] = self::target();
        $v = self::version();
        $tag = $v !== null ? "download/v$v" : 'latest/download';
        return 'https://github.com/' . self::REPO . "/releases/$tag/laravel-dev-mcp_{$os}_{$arch}";
    }

    // Returns the path to the platform binary, downloading it if not present.
    public static function ensure(): string
    {
        $dest = self::path();
        if (is_file($dest) && filesize($dest) > 0) {
            return $dest;
        }
        @mkdir(dirname($dest), 0755, true);
        $tmp = "$dest.download";
        self::download(self::assetUrl(), $tmp);
        @chmod($tmp, 0755);
        @unlink($dest);
        if (!rename($tmp, $dest)) {
            throw new \RuntimeException("Failed to move binary into place at $dest");
        }
        return $dest;
    }

    private static function download(string $url, string $dest): void
    {
        if (function_exists('curl_init')) {
            $fh = fopen($dest, 'w');
            if ($fh === false) {
                throw new \RuntimeException("Cannot open $dest for writing");
            }
            $ch = curl_init($url);
            curl_setopt_array($ch, [
                CURLOPT_FILE => $fh,
                CURLOPT_FOLLOWLOCATION => true,
                CURLOPT_FAILONERROR => true,
                CURLOPT_USERAGENT => 'laravel-dev-mcp-composer',
            ]);
            $ok = curl_exec($ch);
            $err = curl_error($ch);
            $code = curl_getinfo($ch, CURLINFO_HTTP_CODE);
            curl_close($ch);
            fclose($fh);
            if ($ok === false) {
                @unlink($dest);
                throw new \RuntimeException("Failed to download $url: $err (HTTP $code)");
            }
            return;
        }
        // Fallback: streams (follow_location is on by default for http://).
        $ctx = stream_context_create(['http' => [
            'user_agent' => 'laravel-dev-mcp-composer',
            'follow_location' => 1,
        ]]);
        $src = @fopen($url, 'r', false, $ctx);
        if ($src === false) {
            throw new \RuntimeException("Failed to open $url");
        }
        $out = fopen($dest, 'w');
        stream_copy_to_stream($src, $out);
        fclose($src);
        fclose($out);
        if (filesize($dest) === 0) {
            @unlink($dest);
            throw new \RuntimeException("Empty download from $url");
        }
    }
}
