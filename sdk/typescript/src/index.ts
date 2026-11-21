// @open-conduit/sdk: build an Open Conduit extension in TypeScript.
//
//   import { extension, t, OperationError } from "@open-conduit/sdk";
//
//   const ext = extension("weather", { version: "0.1.0" })
//     .operation("weather.forecast", {
//       input: t.object({ city: t.string({ description: "The city, by name." }) }),
//       guidance: "Call `weather.forecast` before planning anything outdoors.",
//       handler: async ({ city }) => ({ city, summary: "sunny" }),
//     });
//   await ext.main();
//
// The guide is site/src/content/docs/architecture/extension-packaging.md.

export { extension, Extension, Server, ProtocolVersions } from "./extension.ts";
export type {
  CallContext,
  CheckContext,
  CheckFn,
  CommitOut,
  Context,
  EmitOut,
  EmitSpec,
  EntryOf,
  EntryTypeSpec,
  IngressTypeSpec,
  MigrateFn,
  OperationSpec,
  Options,
  PushDelivery,
  RouteCall,
  RouteSpec,
  SampleCall,
  SourceSpec,
  TaskFn,
  WaitOut,
  WaitSpec,
  WebhookContext,
  WebhookRequest,
  WebhookResponse,
  WorkKindSpec,
} from "./extension.ts";
export { Host, MaxWorksPage, text } from "./host.ts";
export type {
  Accept,
  Batch,
  Caller,
  DeliveryInput,
  Entry,
  Put,
  Value,
  WorksQuery,
} from "./host.ts";
export { t, check, diagnose, matches, isRecord } from "./schema.ts";
export type { Infer, Type } from "./schema.ts";
export {
  Code,
  OperationError,
  ProtocolError,
  ConfigError,
  pointer,
  CancelledError,
  TransportError,
  isProtocolCode,
} from "./errors.ts";
export type { ConfigDiagnostic } from "./errors.ts";
export {
  Conn,
  MaxMessageBytes,
  MaxChunkBytes,
  MaxInflight,
  MaxControlInflight,
  laneOf,
} from "./jsonrpc.ts";
export * from "./wire.ts";
