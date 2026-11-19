# Elgato Makers CLI

`maker` is an independent CLI for Elgato Marketplace makers. It reads your
catalog and analytics and prepares Stream Deck Marketplace releases.

```sh
npx @sentinelite/elgato-makers-cli --version
npm install -g @sentinelite/elgato-makers-cli
maker --help
```

The npm package includes the native executables for macOS and Linux on
Apple Silicon/ARM64 and x64, plus Windows x64. It requires Node.js 20 or later
and uses no installation scripts. All executable files are bundled so running
`maker` does not need to download a binary. If you use Windows on ARM, use x64
Node and Windows x64 emulation or build the CLI from source.

For setup and authentication, see the [project README](https://github.com/SENTINELITE/elgato-makers-cli#readme).
Standalone executables and checksums are available on [GitHub Releases](https://github.com/SENTINELITE/elgato-makers-cli/releases).
