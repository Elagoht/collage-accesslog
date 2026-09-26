# elagoht/accesslog

A collage plugin that writes one structured log line per request — method, path,
status, bytes, duration, client address, user agent, referer and request id —
through `slog`, and gives every request an id its handlers can log with.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{accesslog.New(accesslog.Options{})},
})
```

Requires collage v0.23.0 or later.

## The line

```
time=2026-09-26T15:40:02.114+03:00 level=INFO msg=request method=GET path=/blog/hello status=200 bytes=5120 duration=1.84ms ip=203.0.113.9 user_agent="Mozilla/5.0 …" referer=https://example.org/ request_id=7f3c9a…
```

The fields are `slog` attributes, so the format is the logger's: the application's
own logger by default — whatever `Config.Logger` is — or `Options.Logger`. A
`slog.NewJSONHandler` writes the same line as JSON, where `duration` is in
nanoseconds.

A `5xx` is logged at `ERROR`, everything else at `INFO`. `path` is the path without
its query: a query carries tokens and e-mail addresses often enough that logging it
by default is a leak waiting to be found. `bytes` is the body as written, before
any compression a proxy in front adds.

## Request ids

Every request gets an id. The one in its `X-Request-ID` header is kept when it looks
like an id — up to 128 letters, digits and `._:;=+/-`, which covers UUIDs, hex,
base64 and AWS's `Root=1-…` — and a new random one is made otherwise, so a client
cannot put a newline or a megabyte into the log.

The id is sent back on the response, and is in the request's context:

```go
func checkout(ctx context.Context, rc *collage.RenderContext) (any, []string, error) {
	logger.InfoContext(ctx, "charging card", "request_id", accesslog.RequestID(ctx))
	// ...
}
```

`accesslog.RequestID(rc.Context())` works in a data handler, an action, and any
handler of the application's own; outside a request it is `""`. The request's own
header is set to the id too, so a handler proxying the request onward passes on the
id that was logged. A page that renders the id must not be cached: a cached page
would show every reader the first reader's id.

## Skipping and sampling

`Skip` lists path prefixes that are never logged, `/_collage/` — collage's
development endpoints — and `/healthz` by default. A skipped request still gets an
id. An empty list logs everything.

A busy site can log a fraction of its successful responses: `Sample: 0.1` logs one
`2xx` in ten, chosen at random. A redirect, a `4xx` and a `5xx` are always logged —
they are the lines someone goes looking for.

## The client's address

`ip` is the address the connection came from. Behind a reverse proxy that is the
proxy, so `TrustProxy` reads `X-Forwarded-For` — or `X-Real-IP` — instead, when the
connection came from a trusted proxy. `X-Forwarded-For` is walked from the right,
past every trusted proxy, and the first address that is not one is the client's:
everything to its left was written by the client, who can write anything there.

`TrustedProxies` names the proxies, as addresses or CIDR ranges. Empty trusts
loopback and private addresses, which is what a proxy on the same host or network
has; a CDN's ranges have to be named.

## Streams and WebSockets

The response writer the plugin wraps passes `Flush` and `Hijack` through and has
`Unwrap`, so an event stream flushes and a WebSocket upgrades beneath it. A
hijacked connection is logged with status `101` when the request's handler returns;
its duration is how long the connection was open.

## Configuration

```json
{
  "elagoht/accesslog": {
    "skip": ["/_collage/", "/healthz", "/static/"],
    "sample": 0.25,
    "trustProxy": true,
    "trustedProxies": ["10.0.0.0/8"],
    "requestIdHeader": "X-Request-ID"
  }
}
```

`Logger` can only be set from Go. A sample outside 0 to 1, a skip prefix not
beginning with `/`, a trusted proxy that is neither an address nor a range, and a
header name with anything but letters, digits and dashes stop the application from
starting.

## Limitations

- The plugin's middleware runs after the application's own, so a request an
  application middleware answers itself — a `401` from an auth check — never
  reaches it and is not logged, and the time the application's middleware spent is
  not in `duration`.
- collage's development reload stream is served ahead of all middleware and is
  never logged, whatever `Skip` says.
- Sampling to none is not offered: `Sample` is zero by default and zero means all.
  A site that wants no successful requests logged wants `Skip`, or no access log.
