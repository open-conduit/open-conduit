// An extension with one operation an agent can call, weather.forecast.
// Forecasts come from the scope's config rather than a weather service, so
// the package runs and certifies with no network or credentials.

import { extension, type Infer, OperationError, t, text } from "@open-conduit/sdk";

const Forecast = t.object({
  summary: t.string(),
  high_c: t.number(),
});

// The config of a scope's `weather` entry. A config that doesn't match is
// refused before the daemon applies it, and configure receives it typed.
const WeatherConfig = t.object({
  forecasts: t.record(Forecast, { description: "Forecasts by city, in lower case." }),
});

type Forecasts = Infer<typeof WeatherConfig>["forecasts"];

export function build() {
  // Rebuilt on every configuration generation, so a removed entry stops answering.
  let byScope = new Map<string, Forecasts>();

  return extension("weather", {
    version: "0.1.0",
    summary: "Example TypeScript extension: weather forecasts from the scope's config.",
  })
    .entryType("weather", {
      config: WeatherConfig,
      configure: (entries) => {
        byScope = new Map(entries.map((e) => [e.scopeId, e.config.forecasts]));
      },
    })
    .operation("weather.forecast", {
      input: t.object({
        city: t.string({ description: "The city to forecast, by name, in lower case." }),
        days: t.optional(t.integer({ description: "How many days ahead (default 1)." })),
      }),
      // Agents read this in their toolbelt, so it says when to call, not how it works.
      guidance: "Call `weather.forecast` with a city before planning anything outdoors.",
      handler: async ({ city, days = 1 }, { host, authority }) => {
        // A call runs under its Work's scope, so each scope sees only its own forecasts.
        const forecasts = byScope.get(authority.scope_id);
        // The agent gets the code and message as they are, so both say what to do next.
        if (forecasts === undefined) throw new OperationError("not_configured", "this scope has no weather entry; ask the operator to add one");
        const forecast = forecasts[city];
        if (forecast === undefined) throw new OperationError("unknown_city", `no forecast for ${city}; known cities: ${Object.keys(forecasts).join(", ")}`);

        // KV is namespaced to the extension and kept by the host, so the count survives restarts.
        await host.update("weather.lookups", `${authority.scope_id}/${city}`, (old) => String(Number(old === undefined ? "0" : text(old)) + 1));
        return { city, days, ...forecast };
      },
    })
    // doctor runs checks; the detail line is what the operator sees.
    .check("weather", () => `${byScope.size} scope(s) configured`)
    // `extension certify` runs these cases against the packaged binary with this config.
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
