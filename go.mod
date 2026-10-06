// A collage plugin that writes one structured log line per request — method,
// path, status, size, duration, client address, request id — through slog, and
// hands every request an id its handlers can log with.
module github.com/Elagoht/collage-accesslog

go 1.26

require github.com/Elagoht/collage v0.49.0
