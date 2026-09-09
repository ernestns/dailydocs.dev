# DailyDocs

DailyDocs curates source documentation links; it does not author or host articles.
Read `docs/product-spec.md` for the product contract and `docs/generation-quality.md` for measured discovery evidence and URL/review identity rules.

- Missing-topic generation requires an explicit POST; ordinary URL visits must not enqueue or call providers.
- `just check` runs normal validation; the opt-in live quality test spends provider credits and must never be enabled for routine tests.
- Local `.env` and SQLite data are private; use `scripts/with-env.sh` to load configuration without copying credentials into tracked files.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
