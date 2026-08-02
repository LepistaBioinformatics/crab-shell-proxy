# Which memory to write to

You have more than one place to remember things, and they are not
interchangeable. Choosing wrong means the user cannot find what you saved.

## 1. Knowledge graph (MCP server `memory`) — the default for facts

Use it whenever a durable fact appears about an **entity** — a person, project,
system, company, a preference of the user — or a **link** between two of them.
This is the memory the user can browse, search and audit; it is also the only one
that records which conversation a fact came from.

- `mcp_memory_create_entities` — the entity does not exist yet
- `mcp_memory_add_observations` — the entity exists; add the fact to it
- `mcp_memory_create_relations` — link two entities, in active voice
  ("maintains", "works on", "depends on", "includes")
- `mcp_memory_search_nodes` / `mcp_memory_semantic_search` — **consult before
  answering** anything about the user or a project
- `mcp_memory_read_graph` / `mcp_memory_open_nodes` — inspect

**Search before you create.** `create_entities` ignores a name that already
exists, so re-asserting a fact under a different spelling ("Ana" vs "ana")
produces two entities that no single query reunites. If the entity is there, add
an observation instead.

**Create the relation too.** An entity with no relations is an isolated point:
nothing leads to it from anywhere else. When a fact involves two entities,
record the link as well as the fact.

**Keep `entityType` consistent** and reuse the types already in the graph — the
user filters the list by it, so inventing a new type per fact makes that filter
useless. Read the graph before coining one.

## 2. Themes: model them as entities, not as a type

When several facts belong to one subject, create an entity of type `tema`/`theme`
and relate its members to it:

```
create_entities   {"name": "Onboarding", "entityType": "theme", ...}
create_relations  {"from": "Onboarding", "to": "Access checklist",
                   "relationType": "includes"}
```

Not via `entityType`: that field says what a thing **is** and holds one value, so
an entity cannot sit in two themes through it. Relations are many-to-many — an
entity belongs to as many themes as make sense, and a theme can contain a theme.

## 3. `MEMORY.md` — your own operational notes only

Do not put facts about the user, about people or about projects here. If you
find yourself using `append_file` or `write_file` on `MEMORY.md` to remember a
fact, it belonged in the graph.

## 4. `MEMORY_CUSTOM.md` — the user's standing instructions

Written by the user, for you. Read it; never overwrite it.

## Never claim a save you did not make

Do not say you stored something in the knowledge graph unless you called an
`mcp_memory_*` tool in that same turn. If the call failed, say it failed and
why — do not report success.

When the user asks where something was saved, **check** (with
`mcp_memory_search_nodes` or `mcp_memory_open_nodes`) before answering.

---

*This file is maintained by the platform and mounted read-only. Edits to it do
not persist. The user's own `MEMORY_CUSTOM.md` takes precedence over anything
here.*
