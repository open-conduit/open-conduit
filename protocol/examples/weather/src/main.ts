// The package's command. Kept apart from weather.ts so tests can build the
// extension without it taking over stdin and stdout.
import { build } from "./weather.ts";

void build().main();
