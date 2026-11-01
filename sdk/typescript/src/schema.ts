// Typed operation inputs. `t` builds the JSON Schema an operation's input
// declares in the manifest, and the TypeScript type its handler receives
// comes from the same value, so the two cannot disagree. Decoding is strict,
// as the Go SDK's is: a missing required field, a field the schema does not
// have, or a value of the wrong type refuses the call with bad_request
// before the handler runs.

declare const phantom: unique symbol;
const optionalMark = Symbol("optional");

/** A JSON Schema that decodes to T. Serialises as plain JSON Schema. */
export type Type<T> = Readonly<Record<string, unknown>> & { readonly [phantom]?: T };

/** The TypeScript type a schema decodes to. */
export type Infer<S> = S extends Type<infer T> ? T : never;

type Optional<T> = Type<T> & { readonly [optionalMark]: true };

type Shape = Record<string, Type<unknown>>;
type RequiredKeys<S extends Shape> = { [K in keyof S]: S[K] extends { readonly [optionalMark]: true } ? never : K }[keyof S];
type Flatten<T> = { [K in keyof T]: T[K] } & {};
type ObjectOf<S extends Shape> = Flatten<
  { [K in RequiredKeys<S>]: Infer<S[K]> } & { [K in Exclude<keyof S, RequiredKeys<S>>]?: Infer<S[K]> }
>;

interface Doc {
  /** Becomes the field's description, which the agent reads. */
  description?: string;
}

function withDoc(schema: Record<string, unknown>, o?: Doc): Record<string, unknown> {
  return o?.description === undefined ? schema : { ...schema, description: o.description };
}

export const t = {
  string(o?: Doc & { format?: string }): Type<string> {
    const s: Record<string, unknown> = { type: "string" };
    if (o?.format !== undefined) s.format = o.format;
    return withDoc(s, o);
  },
  /** One of the given strings. */
  enum<const V extends readonly [string, ...string[]]>(values: V, o?: Doc): Type<V[number]> {
    return withDoc({ type: "string", enum: [...values] }, o);
  },
  integer(o?: Doc): Type<number> {
    return withDoc({ type: "integer" }, o);
  },
  number(o?: Doc): Type<number> {
    return withDoc({ type: "number" }, o);
  },
  boolean(o?: Doc): Type<boolean> {
    return withDoc({ type: "boolean" }, o);
  },
  array<T>(items: Type<T>, o?: Doc): Type<T[]> {
    return withDoc({ type: "array", items }, o);
  },
  /** An object of exactly these fields; each is required unless t.optional. */
  object<S extends Shape>(shape: S, o?: Doc): Type<ObjectOf<S>> {
    const required = Object.keys(shape).filter((k) => !(optionalMark in (shape[k] as object)));
    const s: Record<string, unknown> = { type: "object", properties: shape, additionalProperties: false };
    if (required.length > 0) s.required = required;
    return withDoc(s, o);
  },
  /** A field that may be left out. */
  optional<T>(inner: Type<T>): Optional<T> {
    const out = { ...inner };
    Object.defineProperty(out, optionalMark, { value: true, enumerable: false });
    return out as Optional<T>;
  },
  /** Any JSON value, passed through as it is. */
  any(o?: Doc): Type<unknown> {
    return withDoc({}, o);
  },
};

/** Why value does not match schema, or undefined when it does. */
export function check(schema: Readonly<Record<string, unknown>>, value: unknown, path = ""): string | undefined {
  const field = path === "" ? "the input" : `field "${path}"`;
  const got = () => `${field} has the wrong type (got a JSON ${jsonKind(value)})`;
  switch (schema.type) {
    case undefined:
      return undefined;
    case "string":
      if (typeof value !== "string") return got();
      if (Array.isArray(schema.enum) && !schema.enum.includes(value)) return `${field} must be one of ${schema.enum.map((v) => JSON.stringify(v)).join(", ")}`;
      return undefined;
    case "integer":
      return typeof value === "number" && Number.isInteger(value) ? undefined : got();
    case "number":
      return typeof value === "number" ? undefined : got();
    case "boolean":
      return typeof value === "boolean" ? undefined : got();
    case "array": {
      if (!Array.isArray(value)) return got();
      const items = (schema.items ?? {}) as Record<string, unknown>;
      for (let i = 0; i < value.length; i++) {
        const why = check(items, value[i], `${path}[${i}]`);
        if (why !== undefined) return why;
      }
      return undefined;
    }
    case "object": {
      if (value === null || typeof value !== "object" || Array.isArray(value)) return path === "" ? "the input must be a JSON object" : got();
      const props = (schema.properties ?? {}) as Record<string, Record<string, unknown>>;
      const obj = value as Record<string, unknown>;
      for (const k of (schema.required ?? []) as string[]) {
        if (!(k in obj)) return `missing field "${join(path, k)}"`;
      }
      for (const [k, v] of Object.entries(obj)) {
        const p = props[k];
        if (p === undefined) {
          if (schema.additionalProperties === false) return `unknown field "${join(path, k)}"`;
          continue;
        }
        const why = check(p, v, join(path, k));
        if (why !== undefined) return why;
      }
      return undefined;
    }
    default:
      return undefined;
  }
}

function join(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}

function jsonKind(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v === "object" ? "object" : typeof v;
}
