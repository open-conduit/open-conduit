// The weather extension: one operation an agent can call, weather.forecast,
// answered from the forecasts in its scope's `weather` entry. It is the
// TypeScript twin of the Go example in the extension packaging guide, and
// what `make certify` certifies for the TypeScript SDK.

import { extension, OperationError, t } from "@open-conduit/sdk";

export interface Forecast {
  summary: string;
  high_c: number;
}

/** Forecasts by scope, from the last configure. */
type Table = Map<string, Record<string, Forecast>>;

export function build() {
  let table: Table = new Map();
  return extension("weather", {
    version: "0.1.0",
    summary: "Example TypeScript extension: weather forecasts from the scope's config.",
  })
    .entryType("weather")
    .onValidate((p) => {
      if (p.type !== "weather") return;
      const c = p.config as { forecasts?: unknown } | undefined;
      if (c === undefined || c === null || typeof c.forecasts !== "object" || c.forecasts === null) throw new Error("a weather entry needs forecasts");
    })
    .onConfigure((p) => {
      const next: Table = new Map();
      for (const e of p.entries ?? []) {
        if (e.type !== "weather") continue;
        next.set(e.scope_id, ((e.config ?? {}) as { forecasts?: Record<string, Forecast> }).forecasts ?? {});
      }
      table = next;
    })
    .operation("weather.forecast", {
      input: t.object({
        city: t.string({ description: "The city to forecast, by name, in lower case." }),
        days: t.optional(t.integer({ description: "How many days ahead (default 1)." })),
      }),
      guidance: "Call `weather.forecast` with a city before planning anything outdoors.",
      handler: async ({ city, days }, { host, authority }) => {
        const forecasts = table.get(authority.scope_id);
        if (forecasts === undefined) throw new OperationError("not_configured", "this scope has no weather entry");
        const f = forecasts[city];
        if (f === undefined) throw new OperationError("unknown_city", `no forecast for ${city}`);
        // Count lookups per city in the extension's own KV namespace.
        await host.update("weather.lookups", `${authority.scope_id}/${city}`, (old) => String(Number(old === undefined ? 0 : new TextDecoder().decode(old)) + 1));
        return { city, days: days ?? 1, summary: f.summary, high_c: f.high_c };
      },
    })
    .check("weather", () => `${table.size} scope(s) configured`)
    .certification({
      fixture: {
        entries: [{ scope_id: "weather-scope", type: "weather", name: "weather", config: { forecasts: { london: { summary: "rain", high_c: 14 } } } }],
        operations: [
          {
            operation: "weather.forecast",
            input: { schema: "weather.forecast.request.v1", data: { city: "london" } },
            output: { schema: "weather.forecast.v1", data: { city: "london", days: 1, summary: "rain", high_c: 14 } },
          },
          { operation: "weather.forecast", input: { schema: "weather.forecast.request.v1", data: { city: "atlantis" } }, error: "unknown_city" },
          { operation: "weather.forecast", input: { schema: "weather.forecast.request.v1", data: {} }, error: "bad_request" },
        ],
      },
    });
}
