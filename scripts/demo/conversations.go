package main

import "time"

const (
	min_ = time.Minute
	hour = time.Hour
	day  = 24 * time.Hour
)

// conversations is the demo corpus: one fictional repository (nimbus, a
// weather-forecast service) worked on by every supported agent.
var conversations = []convo{
	{"claude", "api", "Add rate limiting to the public forecast API", 3 * min_, []turn{
		u("Our public `/v1/forecast` endpoint is getting hammered by a few scrapers. Add per-API-key rate limiting, 600 requests/minute, with a burst of 50."),
		a("I'll add a token-bucket limiter in front of the forecast handlers.\n\n- **Where:** a new middleware in `api/middleware/ratelimit.go`, applied only to the `/v1/forecast*` routes.\n- **Keying:** the `X-API-Key` header, falling back to the client IP for unauthenticated calls.\n- **Storage:** Redis, so all four API replicas share one budget.\n\n```go\nfunc RateLimit(store Store, rate, burst int) func(http.Handler) http.Handler {\n\treturn func(next http.Handler) http.Handler {\n\t\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n\t\t\tok, retry := store.Take(r.Context(), clientKey(r), rate, burst)\n\t\t\tif !ok {\n\t\t\t\tw.Header().Set(\"Retry-After\", strconv.Itoa(int(retry.Seconds())))\n\t\t\t\thttp.Error(w, \"rate limit exceeded\", http.StatusTooManyRequests)\n\t\t\t\treturn\n\t\t\t}\n\t\t\tnext.ServeHTTP(w, r)\n\t\t})\n\t}\n}\n```"),
		u("Looks good. Make sure internal dashboards aren't limited."),
		a("Done. Keys with the `internal` scope skip the limiter entirely, and I added a test for that.\n\n- `go test ./api/...` passes.\n- Responses now include `X-RateLimit-Remaining`, so clients can back off before they hit a `429`."),
	}},
	{"codex", "api", "Fix flaky TestForecastCache under -race", 17 * min_, []turn{
		u("TestForecastCache fails about 1 in 20 runs with -race. Find out why and fix it."),
		a("The race is in `cache.Refresh`: it swaps `c.entries` while `Get` is still ranging over the old map. The test only catches it because it refreshes on a 1 ms ticker."),
		u("Can we avoid a mutex on the hot path?"),
		a("Yes. I switched `entries` to an `atomic.Pointer[map[string]Entry]`: readers load the pointer once, and `Refresh` builds a new map and stores it.\n\n- 500 runs with `-race -count=500`: 0 failures.\n- `BenchmarkGet` went from 41 ns/op to 12 ns/op."),
	}},
	{"omp", "", "Plan the v2 billing migration", 42 * min_, []turn{
		u("We need to move from per-request billing to monthly plans. Draft a migration plan that doesn't break existing customers."),
		a("## Plan\n\n1. **Dual-write** usage events to the old ledger and the new `subscriptions` table for one billing cycle.\n2. **Shadow invoices:** generate v2 invoices without charging, and diff them against v1.\n3. **Opt-in:** let customers switch plans from the dashboard; new signups default to v2.\n4. **Cutover:** after 60 days, migrate the remaining accounts and freeze the old ledger.\n\n> Risk: annual contracts priced per request. Those need a manual review, so I flagged the 14 affected accounts in `docs/billing-v2.md`."),
		u("What happens to prepaid credits?"),
		a("They carry over as a balance on the new plan and are drawn down before the card is charged. I added that rule to the plan and a test fixture for an account with credits."),
	}},
	{"pi", "web", "Dark mode toggle flickers on first load", 1 * hour, []turn{
		u("The dashboard flashes white for a frame before switching to dark mode. Fix it."),
		a("The theme is applied in a `useEffect`, which runs after the first paint. I moved the decision into a tiny inline script in `index.html` that sets `data-theme` before React boots:\n\n```html\n<script>\n  document.documentElement.dataset.theme =\n    localStorage.theme ?? (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')\n</script>\n```\n\nNo more flash on a cold load in Chrome, Firefox, or Safari."),
	}},
	{"claude", "infra", "Terraform plan wants to recreate the RDS instance", 2 * hour, []turn{
		u("`terraform plan` in staging says it will destroy and recreate `aws_db_instance.main`. Why? Nothing about the database changed."),
		a("The plan shows `engine_version: \"15.4\" -> \"15.5\"` forcing replacement. AWS auto-upgraded the minor version, and the module pins `15.4`.\n\nTwo fixes:\n\n- Set `engine_version = \"15\"` with `auto_minor_version_upgrade = true`, so Terraform stops fighting AWS.\n- Or pin `15.5` to match what's running.\n\nI'd take the first. Either way, **don't apply the current plan**: it would drop the staging database."),
		u("Go with the first option."),
		a("Changed `infra/modules/db/main.tf`. `terraform plan` now reports no changes for the instance."),
	}},
	{"opencode", "api", "Write OpenAPI docs for /v1/alerts", 3 * hour, []turn{
		u("Document the /v1/alerts endpoints in our OpenAPI spec."),
		a("I added `GET /v1/alerts`, `POST /v1/alerts`, and `DELETE /v1/alerts/{id}` to `api/openapi.yaml`, including:\n\n- request and response schemas generated from the Go structs\n- examples for a severe-thunderstorm alert\n- the `429` response from the new rate limiter\n\n`redocly lint` passes with no warnings."),
	}},
	{"gemini", "", "Summarize last week's incident postmortem", 5 * hour, []turn{
		u("Summarize last week's incident postmortem"),
		a("**Incident 2026-09-18: stale forecasts for 47 minutes**\n\n- **Cause:** the ingest worker's NOAA credentials expired, and the retry loop swallowed the `401`.\n- **Impact:** forecasts were up to 3 hours old for all EU customers.\n- **Fixes:** alert on ingest lag over 10 minutes, and treat `401` as fatal instead of retryable.\n- **Open item:** rotate credentials through the secrets manager, not by hand."),
	}},
	{"droid", "web", "Migrate the dashboard from Webpack to Vite", 8 * hour, []turn{
		u("Migrate the web dashboard build from Webpack 5 to Vite."),
		a("Migration done:\n\n- replaced `webpack.config.js` with `vite.config.ts`\n- moved `index.html` to the project root, as Vite expects\n- replaced `process.env.*` with `import.meta.env.VITE_*`\n\n| | Webpack | Vite |\n|---|---|---|\n| Cold start | 38 s | 1.9 s |\n| Production build | 71 s | 24 s |"),
		u("Hot reload broke for CSS modules."),
		a("The old config renamed classes with `[local]__[hash]`; Vite's default hashes differently, so the stale class names didn't match. I set `css.modules.generateScopedName` to the same pattern and HMR works again."),
	}},
	{"kimi", "api", "Profile slow /v1/history queries", 11 * hour, []turn{
		u("/v1/history takes 2-3 seconds for a 30-day range. Profile it."),
		a("The time goes into one query: a sequential scan over `observations` (180M rows) because the filter is `station_id = $1 AND observed_at::date BETWEEN ...`. The cast stops Postgres from using the `(station_id, observed_at)` index.\n\nRewriting it as a range on the raw column:\n\n```sql\nWHERE station_id = $1\n  AND observed_at >= $2\n  AND observed_at <  $3 + interval '1 day'\n```\n\ntakes p95 from 2.4 s to 38 ms."),
	}},
	{"grok", "", "Explain the tile cache eviction policy", 1 * day, []turn{
		u("How does the radar tile cache decide what to evict?"),
		a("It's a size-bounded LRU with two twists:\n\n1. Tiles for the **current hour** are pinned and never evicted.\n2. Zoom levels 0-4 get a 4x weight, because they're requested far more often and are cheap to keep.\n\nWhen the cache is over its 2 GiB budget, it evicts from the cold end until it's back under 90%, which avoids thrashing right at the limit."),
	}},
	{"copilot", "web", "Add Playwright smoke tests for sign-in", 1*day + 4*hour, []turn{
		u("Add Playwright smoke tests that cover sign-in and sign-out."),
		a("Added `web/e2e/auth.spec.ts` with three tests: sign-in with a valid key, a rejected key, and sign-out. They run against the preview deployment in CI, and the whole suite takes 11 seconds."),
	}},
	{"cursor", "api", "Refactor the alert fan-out worker", 1*day + 9*hour, nil},
	{"codex", "infra", "Rotate the staging TLS certificates", 2 * day, []turn{
		u("The staging TLS certificate expires in 5 days. Rotate it and make sure this doesn't happen again."),
		a("Rotated the certificate with cert-manager and moved staging onto the same `ClusterIssuer` as production, so it renews automatically 30 days before expiry. I also added an alert when any certificate is under 14 days from expiry."),
	}},
	{"claude", "web", "Accessibility audit of the forecast cards", 2*day + 6*hour, []turn{
		u("Run an accessibility audit on the forecast cards and fix what you find."),
		a("axe found 9 issues. The important ones:\n\n- The weather icons had no text alternative; screen readers announced \"image\".\n- The temperature colors fail WCAG AA contrast on the light theme (2.9:1).\n- The \"feels like\" tooltip only opens on hover, so keyboard users can't reach it.\n\nAll three are fixed. axe now reports 0 issues on the dashboard and the city page."),
	}},
	{"omp", "api", "Add OpenTelemetry tracing to the ingest pipeline", 3 * day, []turn{
		u("Instrument the ingest pipeline with OpenTelemetry so we can see where time goes."),
		a("Added spans for each stage: `fetch`, `decode`, `validate`, `upsert`. They're exported over OTLP to the collector in `infra/otel`.\n\nThe first traces show `decode` takes 70% of the time. The GRIB2 decoder allocates a new buffer for every message."),
		u("Can you fix the decoder too?"),
		a("Yes. The decoder now reuses a `sync.Pool` of buffers. Ingest of one forecast run dropped from 94 s to 31 s, and the traces confirm `decode` is no longer the bottleneck."),
	}},
	{"pi", "", "Bump Go to 1.25 and fix deprecations", 4 * day, []turn{
		u("Upgrade the repo to Go 1.25 and clean up any deprecation warnings."),
		a("Updated `go.mod`, the CI image, and the Dockerfile. `staticcheck` flagged 6 deprecated calls:\n\n- 4x `ioutil.ReadAll` -> `io.ReadAll`\n- 2x `rand.Seed` -> removed; the global source is seeded automatically\n\nAll tests pass on 1.25."),
	}},
	{"opencode", "web", "i18n: add German and Japanese locales", 5 * day, []turn{
		u("Add German and Japanese translations to the dashboard."),
		a("Added `de.json` and `ja.json` (212 strings each) and wired them into the locale switcher. Units now follow the locale too: `de` shows km/h and `ja` shows m/s."),
		u("The Japanese dates look wrong."),
		a("They used the `en` format. I switched date rendering to `Intl.DateTimeFormat` with the active locale, so Japanese now shows `9月26日(土)`."),
	}},
	{"codex", "", "subagent: review (Curie)", 5*day + 3*hour, []turn{
		a("Reviewed the diff for `feat/alert-digest`. Two problems:\n\n1. `digest.Send` ignores the error from `smtp.Dial`, so a mail outage looks like success.\n2. The test only covers one recipient; the batching path is untested.\n\nEverything else looks correct."),
	}},
	{"claude", "api", "Debug 502s from the load balancer during deploys", 6 * day, []turn{
		u("Every deploy causes about 30 seconds of 502s from the load balancer. Why?"),
		a("Pods are removed from the Service endpoints at the same moment they get `SIGTERM`, but the load balancer keeps sending traffic for up to 30 s while its target health check catches up. Our server exits immediately on `SIGTERM`, so those requests hit a closed port.\n\nFix: a `preStop` hook that sleeps 15 s, plus `http.Server.Shutdown` with a 20 s timeout so in-flight requests finish."),
		u("Apply it and verify."),
		a("Applied to the `api` deployment. During three test deploys under load there were 0 502s, compared with 1,200-1,800 before."),
	}},
	{"droid", "api", "Port the CSV exporter to streaming writes", 1 * 7 * day, []turn{
		u("Large CSV exports run the API out of memory. Make the exporter stream."),
		a("The exporter built the whole file in a `bytes.Buffer` before writing it. It now writes rows straight to the response through `csv.Writer` and flushes every 1,000 rows. A 2 GB export now uses 9 MB of memory instead of 3.1 GB."),
	}},
	{"gemini", "web", "Generate unit tests for useForecast hook", 8 * day, []turn{
		u("Generate unit tests for useForecast hook"),
		a("Added 8 tests in `useForecast.test.ts` covering loading, success, a `404` for an unknown city, retry after a network error, and unit conversion. Coverage for the hook went from 41% to 96%."),
	}},
	{"kimi", "infra", "Cut the CI pipeline from 14 to 6 minutes", 9 * day, []turn{
		u("CI takes 14 minutes. Get it under 7."),
		a("Changes:\n\n- **Cache Go modules and the build cache** between runs (-4 min)\n- **Run the web and API jobs in parallel** instead of one after the other (-3 min)\n- **Shard the integration tests** across 3 runners (-1.5 min)\n\nMedian pipeline time is now 5 min 50 s."),
	}},
	{"codex", "web", "Replace moment.js with date-fns", 10 * day, []turn{
		u("moment.js is 70 KB of our bundle. Replace it with date-fns."),
		a("Replaced all 37 uses. The main bundle is 64 KB smaller gzipped, and the date output matches the old snapshots exactly."),
	}},
	{"grok", "api", "Why does the radar endpoint allocate so much?", 12 * day, []turn{
		u("The pprof heap profile shows /v1/radar allocating 400 MB/s. What's going on?"),
		a("Each request decodes the full PNG tile into an `image.RGBA`, recolors it, and re-encodes it. For a 512x512 tile that's about 1 MB per request just for the pixel buffer.\n\nThe palette only has 16 colors, so switching to `image.Paletted` and swapping the palette instead of rewriting pixels would cut allocations by about 95%."),
	}},
	{"copilot", "", "Draft the CONTRIBUTING guide", 2 * 7 * day, []turn{
		u("Write a CONTRIBUTING.md for new contributors."),
		a("Drafted `CONTRIBUTING.md` with sections on local setup (`make dev`), the branch and commit conventions, how to run each test suite, and the review checklist. It links to the architecture overview in `docs/`."),
	}},
	{"omp", "web", "Fix Safari layout bug in the hourly chart", 2*7*day + 2*day, []turn{
		u("On Safari the hourly chart overflows its card and covers the precipitation row."),
		a("Safari doesn't honor `min-height: 0` on the flex child that holds the SVG, so the chart takes its intrinsic height. Setting an explicit `height: 100%` on the SVG and `overflow: hidden` on the wrapper fixes it in Safari 17 and 18 without changing Chrome or Firefox."),
	}},
	{"pi", "api", "Add idempotency keys to POST /v1/subscriptions", 3 * 7 * day, []turn{
		u("Mobile clients retry POST /v1/subscriptions on timeout and create duplicate subscriptions. Add idempotency keys."),
		a("Clients can now send an `Idempotency-Key` header. The first response for a key is stored for 24 hours, and a retry with the same key returns that response instead of creating a new subscription. A retry with the same key but a different body gets a `422`."),
	}},
	{"opencode", "infra", "Set up Grafana alerts for queue depth", 4 * 7 * day, []turn{
		u("Alert us when the ingest queue backs up."),
		a("Added a Grafana alert that fires when the queue depth is over 5,000 for 10 minutes, and pages on-call if it passes 20,000. Both link to the ingest dashboard."),
	}},
	{"claude", "", "Write the v1.4.0 release notes", 5 * 7 * day, []turn{
		u("Write release notes for v1.4.0 from the merged PRs."),
		a("## v1.4.0\n\n### New\n- Severe-weather alerts via webhooks (#412)\n- German and Japanese dashboard translations (#398)\n\n### Improved\n- `/v1/history` is up to 60x faster for long ranges (#405)\n- Forecast ingest is 3x faster (#401)\n\n### Fixed\n- No more 502s during deploys (#409)\n- Dark mode no longer flashes on load (#395)"),
	}},
	{"codex", "api", "Investigate memory leak in the websocket hub", 6 * 7 * day, []turn{
		u("The API's memory grows by about 200 MB a day. We suspect the websocket hub."),
		a("Confirmed. When a client disconnects without a close frame, its goroutine exits but the hub never removes the client from `h.clients`, so each dead connection keeps its 64 KB send buffer.\n\nI made the read loop unregister the client on any error and added a test that drops 1,000 connections. Memory is now flat over a 24-hour soak test."),
	}},
}
