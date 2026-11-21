#!/usr/bin/env node
// Builds the npm tarball of @open-conduit/sdk at VERSION in OUT and prints
// its path. Nothing in the checkout changes.
//
//   node scripts/pack-sdk.mjs VERSION OUT
//
// sdk/typescript/package.json stays private at 0.0.0, so a stray `npm
// publish` there is refused; the published manifest is derived here. It
// serves only the compiled dist: its exports have no `development`
// condition, because Node won't strip types from a file under node_modules.
import { execFileSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const [version, outArg] = process.argv.slice(2);
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version ?? "") || !outArg) {
  console.error("usage: pack-sdk.mjs VERSION OUT");
  process.exit(2);
}
const out = resolve(outArg);
const sdk = join(dirname(fileURLToPath(import.meta.url)), "..", "sdk", "typescript");
// npm's own output goes to stderr, so stdout is only the tarball's path.
const quiet = { stdio: ["ignore", "pipe", "inherit"] };

if (!existsSync(join(sdk, "node_modules")))
  execFileSync("npm", ["ci", "--no-audit", "--no-fund"], { ...quiet, cwd: sdk });

const stage = mkdtempSync(join(tmpdir(), "open-conduit-sdk-"));
try {
  execFileSync(
    join(sdk, "node_modules", ".bin", "tsc"),
    ["-p", "tsconfig.json", "--outDir", join(stage, "dist")],
    { ...quiet, cwd: sdk },
  );
  // tsc rewrites relative .ts imports to .js in the JavaScript but not in the
  // declarations, which must name files the package ships.
  for (const f of readdirSync(join(stage, "dist"))) {
    if (!f.endsWith(".d.ts")) continue;
    const path = join(stage, "dist", f);
    writeFileSync(
      path,
      readFileSync(path, "utf8").replace(/(["']\.\.?\/[^"']+)\.ts(["'])/g, "$1.js$2"),
    );
  }
  for (const f of ["README.md", "LICENSE"]) copyFileSync(join(sdk, f), join(stage, f));

  const { private: _private, scripts: _scripts, devDependencies: _dev, ...pkg } =
    JSON.parse(readFileSync(join(sdk, "package.json"), "utf8"));
  const entry = (name) => ({
    types: `./dist/${name}.d.ts`,
    default: `./dist/${name}.js`,
  });
  writeFileSync(
    join(stage, "package.json"),
    `${JSON.stringify(
      {
        ...pkg,
        version,
        exports: { ".": entry("index"), "./testing": entry("testing") },
        files: ["dist"],
        publishConfig: { access: "public" },
      },
      null,
      2,
    )}\n`,
  );

  mkdirSync(out, { recursive: true });
  const name = execFileSync(
    "npm",
    ["pack", stage, "--pack-destination", out, "--silent"],
    { ...quiet, encoding: "utf8" },
  ).trim();
  console.log(join(out, name));
} finally {
  rmSync(stage, { recursive: true, force: true });
}
