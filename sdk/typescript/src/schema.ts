// Schemas that are also types. `t` builds a JSON Schema, and Infer gives the
// TypeScript type of the values it accepts, so a declaration and its type
// cannot disagree. The SDK decodes everything that crosses the wire through
// one: an operation's input (strictly, as the Go SDK does), an entry's
// config, and the host's own messages (openly, so a newer host's extra
// fields are ignored rather than refused).

declare const phantom: unique symbol;
// Symbol keys never reach JSON, so the marks stay out of the manifest.
const optionalMark = Symbol("optional");

/** A JSON Schema that accepts values of type T. */
export type Type<T> = Readonly<Record<string, unknown>> & {
  readonly [phantom]?: T;
};

/** The type of the values a schema accepts. */
export type Infer<S> = S extends Type<infer T> ? T : never;

type Optional<T> = Type<T> & { readonly [optionalMark]: true };

type Shape = Record<string, Type<unknown>>;
type RequiredKeys<S extends Shape> = {
  [K in keyof S]: S[K] extends { readonly [optionalMark]: true } ? never : K;
}[keyof S];
type Flatten<T> = { [K in keyof T]: T[K] } & {};
type ObjectOf<S extends Shape> = Flatten<
  { [K in RequiredKeys<S>]: Infer<S[K]> } & {
    [K in Exclude<keyof S, RequiredKeys<S>>]?: Infer<S[K]>;
  }
>;

interface Doc {
  /** Becomes the schema's description, which an agent reads in its toolbelt. */
  description?: string;
}

interface ObjectOptions extends Doc {
  /** Accept fields the shape doesn't name, and ignore them. */
  open?: boolean;
}

function withDoc(
  schema: Record<string, unknown>,
  o: Doc | undefined,
): Record<string, unknown> {
  return o?.description === undefined
    ? schema
    : { ...schema, description: o.description };
}

function isOptional(s: Type<unknown>): boolean {
  return optionalMark in s;
}

export const t = {
  string(o?: Doc & { format?: string }): Type<string> {
    return withDoc(
      o?.format === undefined
        ? { type: "string" }
        : { type: "string", format: o.format },
      o,
    );
  },
  /** One of the given strings. */
  enum<const V extends readonly [string, ...string[]]>(
    values: V,
    o?: Doc,
  ): Type<V[number]> {
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
  /** Exactly this value: tells apart the members of a union. */
  literal<const V extends string | number | boolean>(
    value: V,
    o?: Doc,
  ): Type<V> {
    return withDoc({ const: value }, o);
  },
  array<T>(items: Type<T>, o?: Doc): Type<T[]> {
    return withDoc({ type: "array", items }, o);
  },
  /** An object of these fields; each is required unless t.optional. */
  object<S extends Shape>(shape: S, o?: ObjectOptions): Type<ObjectOf<S>> {
    const required = Object.keys(shape).filter((k) => {
      const field = shape[k];
      return field !== undefined && !isOptional(field);
    });
    const s: Record<string, unknown> = { type: "object", properties: shape };
    if (!o?.open) s.additionalProperties = false;
    if (required.length > 0) s.required = required;
    return withDoc(s, o);
  },
  /** An object with any keys, each holding a value of one schema. */
  record<T>(values: Type<T>, o?: Doc): Type<Record<string, T>> {
    return withDoc({ type: "object", additionalProperties: values }, o);
  },
  /** A field that may be left out. */
  optional<T>(inner: Type<T>): Optional<T> {
    return { ...inner, [optionalMark]: true };
  },
  /** inner, or null (Go sends a nil slice or map as null). */
  nullable<T>(inner: Type<T>): Type<T | null> {
    return { anyOf: [inner, { type: "null" }] };
  },
  /** Any JSON value, passed through as it is. */
  any(o?: Doc): Type<unknown> {
    return withDoc({}, o);
  },
};

/** Reports whether v is a JSON object (not null, not an array). */
export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Reports whether value matches schema, narrowing it to the schema's type. */
export function matches<S extends Type<unknown>>(
  schema: S,
  value: unknown,
): value is Infer<S> {
  return check(schema, value) === undefined;
}

/** Why value does not match schema, or undefined when it does. */
export function check(
  schema: Readonly<Record<string, unknown>>,
  value: unknown,
  path = "",
): string | undefined {
  const field = path === "" ? "the input" : `field "${path}"`;
  const wrongType = `${field} has the wrong type (got a JSON ${jsonKind(value)})`;
  if (Array.isArray(schema.anyOf)) {
    const options = schema.anyOf.filter(isRecord);
    return options.some((s) => check(s, value, path) === undefined)
      ? undefined
      : wrongType;
  }
  if ("const" in schema && value !== schema.const)
    return `${field} must be ${JSON.stringify(schema.const)}`;
  switch (schema.type) {
    case "null":
      return value === null ? undefined : wrongType;
    case "string":
      if (typeof value !== "string") return wrongType;
      if (Array.isArray(schema.enum) && !schema.enum.includes(value))
        return `${field} must be one of ${schema.enum.map((v) => JSON.stringify(v)).join(", ")}`;
      return undefined;
    case "integer":
      return typeof value === "number" && Number.isInteger(value)
        ? undefined
        : wrongType;
    case "number":
      return typeof value === "number" ? undefined : wrongType;
    case "boolean":
      return typeof value === "boolean" ? undefined : wrongType;
    case "array": {
      if (!Array.isArray(value)) return wrongType;
      const items = isRecord(schema.items) ? schema.items : {};
      for (const [i, item] of value.entries()) {
        const why = check(items, item, `${path}[${i}]`);
        if (why !== undefined) return why;
      }
      return undefined;
    }
    case "object":
      return checkObject(schema, value, path, wrongType);
    default:
      return undefined;
  }
}

function checkObject(
  schema: Readonly<Record<string, unknown>>,
  value: unknown,
  path: string,
  wrongType: string,
): string | undefined {
  if (!isRecord(value))
    return path === "" ? "the input must be a JSON object" : wrongType;
  const props = isRecord(schema.properties) ? schema.properties : {};
  const required = Array.isArray(schema.required) ? schema.required : [];
  for (const k of required) {
    if (typeof k === "string" && !(k in value))
      return `missing field "${join(path, k)}"`;
  }
  for (const [k, v] of Object.entries(value)) {
    const p = props[k];
    if (isRecord(p)) {
      const why = check(p, v, join(path, k));
      if (why !== undefined) return why;
    } else if (schema.additionalProperties === false) {
      return `unknown field "${join(path, k)}"`;
    } else if (isRecord(schema.additionalProperties)) {
      const why = check(schema.additionalProperties, v, join(path, k));
      if (why !== undefined) return why;
    }
  }
  return undefined;
}

function join(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}

function jsonKind(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v === "object" ? "object" : typeof v;
}
