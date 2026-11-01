// The package's command: answers package-manifest and
// package-certification, and otherwise serves the host on stdin and stdout.
import { build } from "./weather.ts";

void build().main();
