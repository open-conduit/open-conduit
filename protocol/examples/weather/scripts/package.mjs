// Lays out the installable package in OUT (default dist/weather): the
// manifest, certification.json and one executable, weather-extension,
// bundled with esbuild so the package needs nothing but Node to run.
//
//   node scripts/package.mjs [OUT]
import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const out = resolve(process.argv[2] ?? join(root, "dist", "weather"));
const bin = join(out, "weather-extension");

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
await build({
  entryPoints: [join(root, "src", "main.ts")],
  bundle: true,
  platform: "node",
  target: "node22",
  // CommonJS, so the extensionless file runs whatever package.json is above it.
  format: "cjs",
  conditions: ["development"],
  banner: { js: "#!/usr/bin/env node" },
  outfile: bin,
  logLevel: "warning",
});
chmodSync(bin, 0o755);
writeFileSync(
  join(out, "open-conduit-extension.json"),
  execFileSync(bin, ["package-manifest"]),
);
writeFileSync(
  join(out, "certification.json"),
  execFileSync(bin, ["package-certification"]),
);
console.log(`packaged ${out}`);
