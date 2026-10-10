---
title: Action schemas
parent: Reference
nav_order: 5
---

# Action schemas

An action is described the way the major language-model platforms describe a
tool, so a person browsing the catalogue and an agent choosing a tool read the
same contract. This page states that contract in full: the title, and the part
of JSON Schema an action's input and output may use.

## The contract

Every action carries:

| Field | What it is |
|---|---|
| address | `owner@kernel/name`, the name software uses to run it |
| title | a short name people read in a list, such as "Weather forecast" |
| description | what the action does, when to use it, and what it returns |
| input schema | the arguments it takes, as JSON Schema |
| output schema | the result it returns, as JSON Schema |
| price | the most a call can cost, everything it does included |

The title, description, schemas and price are the terms a caller agrees to. Changing
any of them changes the quote, so a caller who pinned the old terms is refused
rather than charged under new ones.

These fields map directly onto the tool definitions agents already use:

| Juice | Anthropic | OpenAI | MCP |
|---|---|---|---|
| address | `name` | `name` | `name` |
| title | — | — | `title` |
| description | `description` | `description` | `description` |
| input schema | `input_schema` | `parameters` | `inputSchema` |
| output schema | — | — | `outputSchema` |

An agent can therefore hand an action's input schema to any of these platforms
unchanged. OpenAI's strict mode additionally lists every property as required
and spells an optional one as nullable; its client libraries make that change
themselves.

## The title

A title is required. It is one line of at most 80 characters, and surrounding
spaces are removed. Set it with `--title` when you create an action and change
it with `action update --title`. Changing the title resets the action's
statistics but leaves it enabled.

Actions created before titles existed were given the last part of their name,
such as `send-draft` for `mail/send-draft`. Give them a proper title with
`action update`.

## What a schema may contain

The top level of both schemas is always an object: an action takes named
arguments and returns named results. Every field inside has a type, or is `{}`
with at most a title, description, default or examples, which accepts any JSON
value.

| Type | Keywords it accepts |
|---|---|
| any type | `type`, `title`, `description`, `default`, `examples`, `deprecated` |
| `string` | `enum` (a list of strings), `format`, `minLength`, `maxLength`, `pattern` |
| `integer`, `number` | `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf` |
| `boolean` | — |
| `array` | `items` (required), `minItems`, `maxItems` |
| `object` | `properties`, `required`, `additionalProperties` (only `false`) |

- **A choice** is a string with an `enum`: `{"type": "string", "enum": ["c", "f"]}`.
  It is the only kind of choice a schema offers.
- **A field that may be empty** has the type `[T, "null"]`:
  `{"type": ["integer", "null"]}` accepts a number or `null`. With an `enum`,
  `null` is accepted only if the enum lists it, as in `["c", "f", null]`.
- **An integer** must lie within ±9007199254740991 (2⁵³ − 1), the range JSON
  carries exactly (RFC 7493). A larger one is refused, since it may already
  have been rounded.
- **An object with declared properties is closed**: a key it does not declare is
  refused, and the stored schema says so with `"additionalProperties": false`.
  An object with no declared properties accepts any keys and any values.
  Entries whose names the caller chooses are a list of objects, each carrying
  its name: `[{"name": "env", "value": "prod"}]`, not `{"env": "prod"}`.
- **Leaving an optional field out is not the same as sending it empty**: `{}`,
  `{"note": ""}` and `{"note": null}` are three different arguments.
- **`format`** describes a string, such as `date-time` or `email`, and is not
  checked.
- **`multipleOf`** is checked exactly as written in decimal: `0.3` is a multiple
  of `0.1`, and `0.30000000001` is not.
- **`pattern`** is a regular expression in the RE2 syntax, which covers the
  patterns JSON Schema documents use in practice.
- **Every property needs a `description`** before the action can be enabled.
  A property `title` is optional; where there is none, the property's key is its
  label.

Arguments are checked against the input schema before any money is reserved,
and results against the output schema before the provider is paid.

## What is rewritten when you save

Several spellings mean the same thing as the forms above. Juice accepts them
and stores the form above, so every action reads alike:

| You write | Juice stores |
|---|---|
| `"nullable": true` (OpenAPI 3.0) | `"type": [T, "null"]` |
| `"const": "x"` on a string | `"enum": ["x"]` |
| a value listed twice in an `enum` | the value listed once |
| `"$ref": "#/$defs/X"` | the definition, written in place |
| `"example": x` | `"examples": [x]` |
| an object with properties | the same, with `"additionalProperties": false` |
| `{}` as the whole schema | `{"type": "object"}` |

Each of these is only another way of writing the same thing: the stored schema
accepts exactly the values the original did. Beside a `$ref`, a `title`,
`description`, `default` or `examples` replaces the referenced one, and a
limit the referenced schema lacks, such as `maxLength`, is added to it.

Keys that describe a document rather than a value, such as `$schema`,
`readOnly` and keys starting with `x-`, are dropped.

## What is refused

A schema using any of the following is refused when you create or update the
action, with an error naming where it is:

- `allOf`, `anyOf` and `oneOf`: a field has one type, a choice among strings
  is an `enum`, and a field that may be empty is `"type": [T, "null"]`. This
  includes `anyOf: [T, {"type": "null"}]`, which Pydantic and Zod produce for an
  optional field;
- a limit beside a `$ref` that differs from the referenced schema's, such as
  `"minLength": 2` beside a reference requiring 5;
- an `enum` of numbers, a `const` that is not a string, or a `const` whose
  value is not in the field's `enum`;
- a field with a limit, such as `minLength`, but no `type`;
- `uniqueItems`, and `additionalProperties` given as a schema;
- `not`, `if`/`then`/`else`, `dependentRequired`, `dependentSchemas`,
  `patternProperties`, `propertyNames`, `prefixItems`, `contains` and the
  `unevaluated` keywords;
- a `$ref` to another document, or a definition that refers to itself;
- `"additionalProperties": true` beside declared properties;
- a top level that is not an object;
- a schema nested more than 8 levels deep, or larger than 64 KiB once its
  references are written in place.

For example:

```console
$ juice action create pick --title "Pick one" --source https://api.example.com/pick \
    --input-schema '{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}'
error: input.properties.v: oneOf is not supported: a field has one type, a choice among strings is an enum, and a field that may be empty is "type": [T, "null"]
```

When Juice starts, it rewrites every stored schema into the stored form above.
An action whose schema it refuses is disabled, and a warning names it; fix its
schema with `action update` and enable it again.

## An example

```json
{
  "type": "object",
  "properties": {
    "city":  {"type": "string", "description": "City name", "examples": ["Zurich"]},
    "days":  {"type": "integer", "minimum": 1, "maximum": 7, "default": 3,
              "description": "Days ahead"},
    "units": {"type": ["string", "null"], "enum": ["c", "f", null],
              "description": "Temperature units; null means the city's own"}
  },
  "required": ["city", "units"],
  "additionalProperties": false
}
```

Importing an OpenAPI document applies the same rules to every operation and
reports each change and each refusal; see
[Wrapping a web API](../providing/web-apis.html).
