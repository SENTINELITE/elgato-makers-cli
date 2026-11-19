#!/usr/bin/env node
"use strict";

const { spawn } = require("node:child_process");
const { constants } = require("node:os");
const { join } = require("node:path");

const platform = process.platform;
const arch = process.arch;
const supported = {
  darwin: ["arm64", "x64"],
  linux: ["arm64", "x64"],
  win32: ["x64"],
};

if (!supported[platform]?.includes(arch)) {
  console.error(`maker: unsupported platform ${platform}/${arch}. See https://github.com/SENTINELITE/elgato-makers-cli/releases.`);
  process.exit(1);
}

const executable = platform === "win32" ? "maker.exe" : "maker";
const child = spawn(join(__dirname, "..", "native", `${platform}-${arch}`, executable), process.argv.slice(2), {
  stdio: "inherit",
});

const signals = process.platform === "win32" ? [] : ["SIGINT", "SIGTERM", "SIGHUP"];
const forwardSignal = Object.fromEntries(signals.map(signal => [signal, () => child.kill(signal)]));
for (const signal of signals) {
  process.on(signal, forwardSignal[signal]);
}

child.on("error", err => {
  console.error(`maker: could not start the CLI (${err.code ?? err.message}). Reinstall the package and try again.`);
  process.exitCode = 1;
});
child.on("close", (code, signal) => {
  for (const name of signals) {
    process.off(name, forwardSignal[name]);
  }
  if (code !== null) {
    process.exitCode = code;
  } else if (signal && constants.signals[signal]) {
    process.exitCode = 128 + constants.signals[signal];
  }
});
