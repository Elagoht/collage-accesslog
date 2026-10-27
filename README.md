# elagoht/accesslog

A collage plugin that writes one structured log line per request — method, path,
status, bytes, duration, client address, user agent, referer and request id —
through `slog`, and gives every request an id its handlers can log with.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{accesslog.New(accesslog.Options{})},
})
```

Requires collage v0.61.0 or later.

## The line

```
time=2026-09-26T15:40:02.114+03:00 level=INFO msg=request method=GET path=/blog/hello status=200 bytes=5120 duration=1.84ms ip=203.0.113.9 user_agent="Mozilla/5.0 …" referer=https://example.org/ request_id=7f3c9a…
```

The fields are `slog` attributes, so the format is the logger's: the application's
own logger by default — whatever `Config.Logger` is — or `Options.Logger`. A
`slog.NewJSONHandler` writes the same line as JSON, where `duration` is in
nanoseconds.

Every request gets its line, whoever answers it — the page, an `app.Handle` mount,
another plugin, collage itself before routing (see
[What it sees](#what-it-sees)). A `5xx` is logged at `ERROR`, everything else at `INFO`. `path` is the path without
its query: a query carries tokens and e-mail addresses often enough that logging it
by default is a leak waiting to be found. `bytes` is the body as written, before
any compression a proxy in front adds.

`referer` is the `Referer` header's scheme, host and path —
`https://example.org/reset`, not `https://example.org/reset?token=…` — since a
page linking to the site can carry a token in its query, fragment or user info.
A referer that does not parse as a URL is logged empty, never raw, and an opaque
one such as `data:…` as its scheme alone. `fullReferer: true` logs the header
exactly as it came.

## Request ids

Every request gets an id. The one in its `X-Request-ID` header is kept when it looks
like an id — up to 128 letters, digits and `._:;=+/-`, which covers UUIDs, hex,
base64 and AWS's `Root=1-…` — and a new random one is made otherwise, so a client
cannot put a newline or a megabyte into the log.

The id is sent back on the response, and is in the request's context:

```go
func checkout(ctx context.Context, rc *collage.RenderContext) (Order, []string, error) {
	logger.InfoContext(ctx, "charging card", "request_id", accesslog.RequestID(ctx))
	// ...
}
```

`accesslog.RequestID(rc.Context())` works in a data handler, an action, and any
handler of the application's own, a bare `app.Handle` one included; outside a
request it is `""`. The request's own header is set to the id too, so a handler
proxying the request onward passes on the id that was logged — by the plugin's
middleware, so not for a request it does not reach (see
[What it sees](#what-it-sees)). A page that renders the id must not be cached: a cached page
would show every reader the first reader's id.

## What it sees

The plugin is two things: a `collage.RequestHook`, which sees every request before
any plugin's middleware and before routing, and a middleware, which wraps the
response writer. The middleware writes the line, with the bytes, and sends the id
back in `X-Request-ID`. A request the middleware never reaches is logged by the
hook once the response is written, with what the hook can know: the status, the
duration, the request's own fields and its id — **no `bytes`, and no
`X-Request-ID` on the response**. Those are:

- a request a plugin listed **before** accesslog answers itself: a ban
  `elagoht/fail2ban` refuses with `403`, a request `elagoht/health` sheds with `503
  overloaded`;
- a request an `app.Handle` mount serves, which runs no middleware unless it was
  mounted with `collage.WithMiddleware()` (collage v0.67.0);
- a request collage answers before routing: a dirty path's redirect to its clean
  spelling (`301`/`308`), logged under the path as it was asked, and a path with an
  encoded slash (`404`).

For the full line — `bytes`, and the id sent back — list accesslog **first** in
`Config.Plugins`, before `elagoht/health` and `elagoht/fail2ban`, and mount a
handler with `app.Handle(prefix, h, collage.WithMiddleware())`. fail2ban's README
asks to be listed first so its ban check runs before the other plugins'
middleware; accesslog is the exception, because its middleware only observes.
Listed first, it also logs every request of a banned client, each a `403`, which
sampling never drops: a scanner hammering the site after its ban is a line per
request, and those are the lines worth having in an attack.

## Skipping and sampling

`Skip` lists path prefixes that are never logged, `/_collage/` — collage's
development endpoints — and `/healthz` and `/readyz`, the probe paths of
`elagoht/health`, by default. A different `livePath` or `readyPath` there must be
added to `skip`. A prefix covers whole segments, with or without a trailing
slash: `/healthz` and `/healthz/` both cover `/healthz` and `/healthz/db`, not
`/healthz-report`. A skipped request still gets an id. An empty list logs
everything. For a request answered before routing, the prefix is matched against
the cleaned path, so `/_collage/../admin` is not skipped.

A busy site can log a fraction of its successful responses: `Sample: 0.1` logs one
`2xx` in ten, chosen at random. A redirect, a `4xx` and a `5xx` are always logged —
they are the lines someone goes looking for.

## The client's address

`ip` is `collage.ClientIP`: the address the connection came from or, when that is
one of the application's `Server.TrustedProxies`, the client's address from
`X-Forwarded-For`, walked from the right past every trusted proxy. Everything to
the left of the first untrusted address was written by the client, who can write
anything there. With no trusted proxies listed — the default — `X-Forwarded-For`
is ignored; `X-Real-IP` is never read. Behind a reverse proxy, list it in
`Config.Server.TrustedProxies` (addresses or CIDR ranges), and every plugin that
asks collage for the client's address agrees with the log.

## Streams and WebSockets

The response writer the plugin wraps passes `Flush` and `Hijack` through and has
`Unwrap`, so an event stream flushes and a WebSocket upgrades beneath it. A
hijacked connection is logged with status `101` when the request's handler returns;
its duration is how long the connection was open.

## Configuration

```json
{
  "elagoht/accesslog": {
    "skip": ["/_collage/", "/healthz", "/readyz", "/static/"],
    "sample": 0.25,
    "requestIDHeader": "X-Request-ID",
    "fullReferer": false
  }
}
```

`Logger` can only be set from Go. A sample outside 0 to 1, a skip prefix not
beginning with `/`, and a header name with anything but letters, digits and dashes stop the application from
starting.

## Limitations

- collage's development reload stream, and the development server's refusal of an
  unknown `Host`, are answered ahead of everything of the application's, the hook
  included, and are never logged, whatever `Skip` says.
- A request the middleware does not reach has no `bytes` in its line and no
  `X-Request-ID` on its response (see [What it sees](#what-it-sees)).
- A request whose handler panics is logged only when the middleware reached it:
  collage does not hand the hook a status for a request whose serving panicked.
- Sampling to none is not offered: `Sample` is zero by default and zero means all.
  A site that wants no successful requests logged wants `Skip`, or no access log.

## Changes

### Changed in v0.3.0

- **Behaviour change: every request is logged, wherever it was answered.** The
  plugin is a `collage.RequestHook` too (a new `OnRequest` method), and logs the
  requests its middleware never reached: a ban `elagoht/fail2ban` refuses when it
  is listed first, as its README asks, a request `elagoht/health` sheds, a bare
  `app.Handle` mount's response, and collage's redirect of a dirty path. Those
  lines have no `bytes` attribute. Expect more lines, a banned client's `403`s
  among them.
- `accesslog.RequestID` works in a bare `app.Handle` handler; its response still
  carries no `X-Request-ID` unless it was mounted with `collage.WithMiddleware()`.
- The README says which requests the middleware sees and in which order to list
  the plugin for the full line.

### v0.2.0

Changed in v0.2.0, for the v1 plugin conventions. A removed key now stops the
application at start with `unknown key`, where it used to be ignored.

| was | now |
|---|---|
| `"trustProxy"` / `Options.TrustProxy` | removed: list the proxies in `Config.Server.TrustedProxies` |
| `"trustedProxies"` / `Options.TrustedProxies` | `Config.Server.TrustedProxies` |
| `"requestIdHeader"` | `"requestIDHeader"` (the old spelling still decodes: keys match case-insensitively) |

- The client's address is `collage.ClientIP`. Two differences from before: with
  no trusted proxies listed nothing is trusted, where an empty `trustedProxies`
  trusted loopback and private addresses; and `X-Real-IP` is no longer read.
- When a trusted proxy forwards an `X-Forwarded-For` hop that is not an
  address, the line shows the connection's own address; before, it showed the
  nearest trusted hop past it.
- A `skip` prefix covers whole segments, with or without a trailing slash:
  `"/healthz"` no longer covers `/healthz-report`, and `"/static/"` covers
  `/static` itself.
- Requires collage v0.61.0.

### v0.1.13

- Rebuilt against collage v0.60.0; the plugin's code is unchanged.
- Requires collage v0.60.0.

### v0.1.12

- collage.json is regenerated for editors: `skip`'s description names
  elagoht/health's paths, as the code does. Nothing else changes.
- Requires collage v0.58.0.

### v0.1.11

- **Behaviour change: the referer is trimmed.** `referer` is logged as its scheme,
  host and path, without its query, fragment or user info, where a reset token or
  a session id can travel; a referer that does not parse is logged as `""` rather
  than raw. `fullReferer: true` (`Options.FullReferer`) logs the header exactly as
  before.
- Requires collage v0.55.0.

### v0.1.10

- Requires collage v0.53.0, the release `elagoht/health` needs. `/readyz` is skipped by default, beside `/healthz`: they are the paths of `elagoht/health`. The README says where to list accesslog relative to health. Nothing else changes.

### v0.1.9

- Requires collage v0.52.0. A static build's header capture (`collage.IsCapture`) is not logged and gets no request id: `collage build` no longer writes a line for every file it asks for, nor reports `X-Request-ID` as an unstable header.

### v0.1.8

- Retracts v0.1.6, tagged by mistake on the previous release's code. Use v0.1.7 or later. Nothing else changes.

### v0.1.7

- Requires collage v0.50.0. Plugin configuration is read with `collage.PluginConfig`, since `host.Config` is gone. Nothing else changes.

### v0.1.5

- Requires collage v0.49.0. Tests only: the test site gives its fragments
  typed data with `collage.Load` and `collage.DataHandler`, since
  `WithDataHandler` is gone. The plugin itself is unchanged.
- README: the request-id sample handler returns a real type, not `any`.

### v0.1.4

- README: from collage v0.38.0 the plugin's middleware is outside the application's
  own, so a request an application middleware answers is logged.

### v0.1.2

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.1.1

- README: a path collage v0.24.0 redirects to its clean spelling is redirected before middleware, so the redirect is not logged.
- Requires collage v0.24.0.
