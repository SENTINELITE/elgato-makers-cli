import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const number = "(?:0|[1-9][0-9]*)";
const prerelease = `(?:${number}|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)`;
const pattern = new RegExp(`^${number}\\.${number}\\.${number}(?:-${prerelease}(?:\\.${prerelease})*)?$`);

export function buildVersion(input) {
  const version = input?.replace(/^v/, "") ?? readFileSync(new URL("../internal/cli/cli.go", import.meta.url), "utf8")
    .match(/^var Version = "([^"]+)"$/m)?.[1];
  if (!version || version.length > 100 || !pattern.test(version)) {
    throw new Error("Expected a version like 0.9.0 or 1.0.0-rc.1 (optional v prefix, no build metadata).");
  }
  return version;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  console.log(buildVersion(process.argv[2]));
}
