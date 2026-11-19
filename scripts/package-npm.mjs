import { chmodSync, copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { buildVersion } from "./build-version.mjs";

const [versionArg, source, output] = process.argv.slice(2);
if (!versionArg || !source || !output) {
  throw new Error("Usage: node scripts/package-npm.mjs <version> <binary-directory> <new-package-directory>");
}
const version = buildVersion(versionArg);

const platforms = [
  ["darwin", "arm64", "darwin-arm64"],
  ["darwin", "amd64", "darwin-x64"],
  ["linux", "arm64", "linux-arm64"],
  ["linux", "amd64", "linux-x64"],
  ["windows", "amd64", "win32-x64"],
];

const root = new URL("../", import.meta.url);
const manifest = JSON.parse(readFileSync(new URL("npm/package.json", root), "utf8"));
delete manifest.private;
manifest.version = version;

// Start in a fresh directory so npm cannot pack stale or unrelated artifacts.
mkdirSync(output);
mkdirSync(join(output, "bin"));
copyFileSync(new URL("npm/bin/maker.cjs", root), join(output, "bin", "maker.cjs"));
chmodSync(join(output, "bin", "maker.cjs"), 0o755);
copyFileSync(new URL("LICENSE", root), join(output, "LICENSE"));
copyFileSync(new URL("npm/README.md", root), join(output, "README.md"));

for (const [goos, goarch, target] of platforms) {
  const ext = goos === "windows" ? ".exe" : "";
  const directory = join(output, "native", target);
  mkdirSync(directory, { recursive: true });
  const destination = join(directory, `maker${ext}`);
  copyFileSync(join(source, `maker_${version}_${goos}_${goarch}${ext}`), destination);
  chmodSync(destination, 0o755);
}

writeFileSync(join(output, "package.json"), JSON.stringify(manifest, null, 2) + "\n");
