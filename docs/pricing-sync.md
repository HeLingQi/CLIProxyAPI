# Spec: Dynamic Model Pricing Sync

## Objective

Add a one-click **Sync** action to the Management Center pricing page. The server fetches BaseLLM's NewAPI pricing expressions from:

`https://basellm.github.io/llm-metadata/api/newapi/ratio_config-v1-base.json`

It matches source model names case-insensitively against models currently available in CLIProxyAPI's runtime registry, then atomically adds or updates one pricing entry for every matched provider/model pair. Existing catalog entries that do not match an active model are preserved.

## Pricing Semantics

Synced entries persist the source expression instead of flattening it to one fixed price. The evaluator supports the complete expression language currently used by the source:

- arithmetic: `+`, `*`, parentheses
- comparisons: `<`, `<=`, `>`, `>=`, `==`
- boolean operators: `&&`, `||`
- ternary expressions: `condition ? yes : no`
- functions: `tier`, `fixed`, `param`, `weekday`, `hour`
- variables: `p`, `c`, `cr`, `cc`, `cc1h`, `cc12h`, `ai`, `ao`, `len`, `image_count`

Runtime values are defined as:

| Expression value | CLIProxyAPI usage field |
|---|---|
| `p` | uncached input tokens / 1,000,000 |
| `c` | output tokens / 1,000,000 |
| `cr` | cache-read tokens / 1,000,000 |
| `cc` | cache-write tokens not classified as 1h/12h / 1,000,000 |
| `cc1h` | 1-hour cache-write tokens / 1,000,000 |
| `cc12h` | 12-hour cache-write tokens / 1,000,000 |
| `ai` / `ao` | audio input/output tokens / 1,000,000 |
| `len` | total input tokens before the response |
| `image_count` | reported generated image count |
| `param("enable_thinking")` | true when reasoning was requested or reasoning tokens were reported |
| `weekday("UTC")` / `hour("UTC")` | request timestamp in UTC |

`tier(name, value)` returns `value`; `fixed(value)` is a fixed USD amount. The final expression value is USD and is converted to integer micro-USD using the existing rounding and overflow rules.

## API Contract

Add:

`POST /v8/management/observability/pricing/models/sync`

Request:

```json
{ "revision": 3 }
```

Response:

```json
{
  "revision": 4,
  "models": [],
  "sync": {
    "source": "https://basellm.github.io/llm-metadata/api/newapi/ratio_config-v1-base.json",
    "configured_models": 12,
    "matched": 10,
    "updated": 10,
    "unmatched": [
      { "provider": "codex", "model": "custom-model" }
    ]
  }
}
```

Rules:

- The request uses the catalog revision for optimistic concurrency.
- Source download, JSON validation, expression parsing, model matching, and catalog mutation must all succeed before the new catalog revision is published.
- A source expression that cannot be parsed aborts the sync; no partial catalog update is allowed.
- A revision conflict returns the existing `pricing_revision_conflict` response.
- The source URL is compiled into the server; clients cannot supply arbitrary URLs.
- The response never exposes credentials or raw request data.

## Persistence Compatibility

- Introduce a new catalog encoding version that stores optional expression and source metadata.
- Continue decoding existing version-1 fixed-price catalogs.
- Fixed-price entries and manual CRUD behavior remain compatible.
- Editing a synced row manually replaces dynamic expression pricing with the entered fixed prices.

## Management Center

- Add a secondary **Sync** button next to **Add model price**.
- The action calls the sync endpoint once and replaces the local catalog with the returned revision.
- Show a success notification containing matched and unmatched counts.
- Synced rows display the dynamic expression and source marker; fixed-price rows retain the four price columns.
- Sync is disabled while disconnected, loading, saving, or syncing.
- Add all new strings to every supported locale.

## Commands

Backend:

```bash
gofmt -w <changed-go-files>
go test ./internal/pricing ./internal/registry ./internal/api/handlers/management
go build -o test-output ./cmd/server && rm test-output
```

Frontend:

```bash
bun run test
bun run lint
bun run build
```

## Testing Strategy

- Parser unit tests cover every operator, function, variable, tier selection, token scaling, time conditions, thinking mode, audio/image billing, invalid syntax, and overflow.
- Catalog tests cover version-1 compatibility, version-2 round trips, fixed quotes, dynamic quotes, atomic multi-model sync, and revision conflicts.
- Registry tests cover active provider/model grouping and duplicate suppression.
- Management handler tests cover source validation, matched/unmatched reporting, and no partial writes.
- Frontend tests cover the sync API contract and dynamic row presentation model.
- Production verification uses real Chrome against all three pricing/usage routes with zero console and network errors.

## Boundaries

### Always

- Parse expressions with a bounded native parser; never use JavaScript evaluation or shell execution.
- Cap source response size and expression/model lengths.
- Preserve existing manual entries not targeted by the current sync.
- Apply the synchronized catalog in one atomic revision.

### Never

- Do not accept a caller-provided source URL.
- Do not execute arbitrary functions or identifiers from source expressions.
- Do not silently flatten tiered expressions.
- Do not retroactively rewrite historical usage records.

## Success Criteria

1. Clicking **Sync** configures every exact model-name match currently available in the runtime registry.
2. Tiered, time-based, thinking-based, cache-TTL, audio-token, and image-count expressions calculate the expected micro-USD cost.
3. Unknown models are reported but do not delete or corrupt existing entries.
4. Sync is atomic and revision-safe.
5. Existing fixed-price catalogs remain readable and editable.
6. Backend tests/build and frontend tests/lint/build pass.
7. The deployed production flow succeeds in a real browser without console or failed network requests.

## Open Questions

None. The selected behavior is full source-expression support.
