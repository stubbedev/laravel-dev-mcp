// Resolves (and, if needed, downloads) the prebuilt Go binary that matches the
// current platform. Shared by the postinstall script and the CLI launcher so a
// failed install (e.g. offline) self-heals on first run.
import { createWriteStream, readFileSync } from 'node:fs';
import { chmod, mkdir, rename, rm, stat } from 'node:fs/promises';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..');
const binDir = join(root, 'bin');

const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf-8'));
const REPO = 'stubbedev/laravel-dev-mcp';

// Map Node's platform/arch onto the published release asset names. Only the
// platforms the release workflow builds for are listed; anything else falls
// back to the "go install" hint in target().
const PLATFORMS = {
  'linux:x64': { os: 'linux', arch: 'amd64' },
  'linux:arm64': { os: 'linux', arch: 'arm64' },
  'darwin:x64': { os: 'darwin', arch: 'amd64' },
  'darwin:arm64': { os: 'darwin', arch: 'arm64' },
};

function target() {
  const key = `${process.platform}:${process.arch}`;
  const t = PLATFORMS[key];
  if (!t) {
    throw new Error(
      `Unsupported platform ${key}. Build from source with: go install github.com/${REPO}@latest`,
    );
  }
  return t;
}

export function binaryPath() {
  return join(binDir, 'laravel-dev-mcp-native');
}

function assetUrl() {
  const { os, arch } = target();
  return `https://github.com/${REPO}/releases/download/v${pkg.version}/laravel-dev-mcp_${os}_${arch}`;
}

async function exists(path) {
  try {
    const s = await stat(path);
    return s.isFile() && s.size > 0;
  } catch {
    return false;
  }
}

// ensureBinary returns the path to the platform binary, downloading it from the
// matching GitHub release if it is not already present.
export async function ensureBinary() {
  const dest = binaryPath();
  if (await exists(dest)) return dest;

  await mkdir(binDir, { recursive: true });
  const url = assetUrl();
  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok || !res.body) {
    throw new Error(`Failed to download ${url}: HTTP ${res.status}`);
  }
  const tmp = `${dest}.download`;
  await pipeline(Readable.fromWeb(res.body), createWriteStream(tmp));
  await chmod(tmp, 0o755).catch(() => {});
  await rm(dest, { force: true });
  await rename(tmp, dest);
  return dest;
}
