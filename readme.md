# Logger — Traefik access log plugin

[![Build Status](https://github.com/gabay/logger/workflows/Main/badge.svg)](https://github.com/gabay/logger/actions)

A [Traefik](https://traefik.io) middleware plugin (run by the [Yaegi](https://github.com/traefik/yaegi) interpreter) that writes access logs to a file, with a configurable line format and optional scheduled rotation with gzip compression.

- **Configurable format**: a Go template with `{` / `}` delimiters, e.g. `{.Ip} {.Status}`. The default is the Common Log Format (with referer and user agent).
- **Never blocks requests**: entries go through a buffered channel to one background writer per file. The writer formats, buffers and writes them. If it can't keep up, entries are dropped and counted rather than delaying responses.
- **Scheduled rotation** with a cron expression, keeping `keep` plain-text backups followed by `keepCompressed` gzip backups.
- **Self-healing retention**: on startup and whenever the retention changes, existing backups are converted (compressed or decompressed) and pruned to match the configuration.

## Configuration

### Static configuration

```yaml
experimental:
  plugins:
    logger:
      moduleName: github.com/gabay/logger
      version: v0.1.1
```

### Dynamic configuration

```yaml
http:
  routers:
    my-router:
      rule: host(`demo.localhost`)
      service: service-foo
      entryPoints: [web]
      middlewares: [access-log]

  middlewares:
    access-log:
      plugin:
        logger:
          file: /var/log/traefik/access.log
          # Optional, defaults to the Common Log Format below.
          format: '{.Ip} - {.User} [{.Time}] "{.Method} {.Path}" {.Status} {.ResponseSize} "{.Referer}" "{.UserAgent}"'
          # Optional: proxies whose X-Forwarded-For / X-Real-Ip are trusted for {.ClientIp}.
          trustedForwarders: [10.0.0.0/8, 192.0.2.1]
          # Optional: entries buffered per file before new ones are dropped.
          queueSize: 8192
          # Optional: streaming requests skip status recording so they are flushed live.
          detectStreaming: true      # default: Server-Sent Events and gRPC
          streamingPaths: [/events/] # default: none
          # Optional: without a rotate section the file is never rotated.
          rotate:
            schedule: "0 0 * * *" # default: daily at midnight
            keep: 2               # default: 1  -> access.log.1, access.log.2
            keepCompressed: 2     # default: 0  -> access.log.3.gz, access.log.4.gz
```

| Option                  | Required | Default      | Description |
|-------------------------|----------|--------------|-------------|
| `file`                  | yes      |              | Log file path. Relative paths are resolved against Traefik's working directory. Missing directories are created. |
| `format`                | no       | `Default()`  | Line format, see [Format](#format). |
| `trustedForwarders`     | no       | none         | IP addresses and CIDR ranges of proxies in front of Traefik, used by `{.ClientIp}`. See [Client IP](#client-ip). |
| `queueSize`             | no       | `8192`       | Entries buffered per file between requests and the writer, 1 to 1048576. When it is full, entries are dropped (and counted) instead of delaying responses. Set once per file: a different value from a reload is ignored (and reported) until Traefik restarts. |
| `detectStreaming`       | no       | `true`       | Detects streaming requests: Server-Sent Events (`Accept: text/event-stream`) and gRPC (`Content-Type: application/grpc…`). See [Streaming](#streaming). |
| `streamingPaths`        | no       | none         | Path prefixes (starting with `/`) of other streaming endpoints, e.g. `/events/`. |
| `rotate`                | no       | disabled     | Enables rotation when present. Fields left unset use their defaults. |
| `rotate.schedule`       | no       | `0 0 * * *`  | Cron expression: 5 fields, 7 fields (with seconds and year) or a descriptor (`@daily`, `@hourly`, `@weekly`, …). Uses the time zone of the Traefik process. |
| `rotate.keep`           | no       | `1`          | Number of plain-text backups: `<file>.1` … `<file>.<keep>`. |
| `rotate.keepCompressed` | no       | `0`          | Number of gzip backups kept after the plain ones: `<file>.<keep+1>.gz` … |

### Format

The format is a Go [`text/template`](https://pkg.go.dev/text/template) that uses `{` and `}` as delimiters instead of `{{` and `}}`. It is executed once per request with the fields below. The format is validated when the configuration loads: a syntax error or an unknown field (such as `{.Nope}`) makes Traefik reject the middleware.

#### Supported fields

| Field              | Value | Example |
|--------------------|-------|---------|
| `{.Ip}`            | The connection's remote address without the port. This is the direct peer, so behind another proxy it's that proxy's IP. | `203.0.113.7` |
| `{.ClientIp}`      | Originating client IP, read from `X-Forwarded-For` / `X-Real-Ip` when the peer is a trusted forwarder; otherwise the same as `{.Ip}`. See [Client IP](#client-ip). | `198.51.100.1` |
| `{.User}`          | User name from HTTP basic authentication (`Authorization: Basic …`) | `frank` |
| `{.Time}`          | Request start time, in Common Log Format, in the Traefik process's time zone | `10/Oct/2000:13:55:36 -0700` |
| `{.Method}`        | Request method | `GET` |
| `{.Path}`          | Request URI as received, including the query string | `/items?page=2` |
| `{.Protocol}`      | Request protocol | `HTTP/1.1` |
| `{.Host}`          | Request host (`Host` header) | `example.com` |
| `{.Status}`        | Response status code; `-` for [streaming](#streaming) requests | `200` |
| `{.ResponseSize}`  | `Content-Length` response header (body size in bytes); `-` when absent, e.g. for chunked or streamed responses | `2326` |
| `{.Referer}`       | `Referer` request header | `https://example.com/` |
| `{.UserAgent}`     | `User-Agent` request header | `curl/8.5.0` |
| `{.Duration}`      | Time spent in the next handlers (upstream included), in milliseconds | `12` |

Field names are case-sensitive. Every field is a string:

- **Empty values** are written as `-`.
- **Escaping:** quotes, backslashes and control characters are escaped JSON-style (`\"`, `\\`, `\n`, `\u0001`), so a request value can't break out of a quoted field or inject fake log lines.

#### Template features

Any template action works, for example:

```text
{.Method} {.Path} {.Status}{if ne .Status "200"} !{end}
{printf "%-7s" .Method} {.Path}
```

Since `{` starts an action, write a literal brace as a string constant: `{"{"}` and `{"}"}`. For example, JSON lines:

```text
{"{"}"ip":"{.Ip}","status":{.Status},"path":"{.Path}"{"}"}
```

## Client IP

`{.Ip}` is always the direct peer. `{.ClientIp}` resolves the originating client behind proxies:

1. If `trustedForwarders` is empty, or the peer isn't in it, the peer is the client: headers sent by untrusted peers are ignored, so they can't be spoofed.
2. Otherwise, `X-Forwarded-For` is read from right to left, skipping trusted forwarders. The first untrusted address is the client. Addresses further left were written by the client itself and are ignored. If every address is trusted, the leftmost one is used.
3. Without `X-Forwarded-For`, `X-Real-Ip` is used. If that is missing too, the peer is used.

> [!IMPORTANT]
> Before middlewares run, Traefik's entrypoint drops the `X-Forwarded-*` and `X-Real-Ip` headers of peers not listed in [`entryPoints.<name>.forwardedHeaders.trustedIPs`](https://doc.traefik.io/traefik/routing/entrypoints/#forwarded-headers), then sets a missing `X-Real-Ip` to the peer address. Your proxies must be listed there **and** in `trustedForwarders`:
>
> ```yaml
> entryPoints:
>   web:
>     address: ":80"
>     forwardedHeaders:
>       trustedIPs: ["10.0.0.0/8"]
> ```

## Streaming

Yaegi has a limitation: when a plugin wraps the response writer (this plugin wraps it to record the status code), the writer Traefik gets back has lost `http.Flusher`. A streamed response wrapped that way only reaches the client when Traefik's buffer fills or the response completes. In a test with an event every second, the first event of a 3-second Server-Sent Events stream arrived after 3 s.

So streaming requests are passed through **unwrapped**, and their response is flushed as it is written (the first event arrived after 1 ms in the same test):

- with `detectStreaming` (the default): Server-Sent Events (`Accept: text/event-stream`) and gRPC (`Content-Type: application/grpc…`)
- requests whose path starts with one of `streamingPaths`, for other streaming endpoints (e.g. chunked downloads or long polling)

The status of these requests is unknown and logged as `-`; every other field works. When the format doesn't use `{.Status}`, no request is wrapped. WebSockets don't need any of this: `http.Hijacker` survives the wrapping.

## Rotation and retention

When the schedule fires, the writer goroutine flushes the file, shifts the backups (`.1` → `.2`, …), renames the file to `<file>.1` and reopens a fresh file. Because this happens on the same goroutine that writes the log, there is no race between writing and rotating. Compressing and pruning the backups then runs in the background, so logging resumes right away. Empty files are not rotated.

With `keep: 2, keepCompressed: 2`, after a few rotations you get:

```text
access.log         current
access.log.1       previous period
access.log.2
access.log.3.gz
access.log.4.gz    oldest; anything older is deleted
```

You can change `keep` and `keepCompressed` between runs. On startup, or when a configuration reload changes them, the existing backups are fixed up to match:

- indexes `1..keep` are decompressed if needed
- indexes `keep+1..keep+keepCompressed` are compressed if needed
- higher indexes are deleted
- temporary files left by an interrupted conversion are removed

Each conversion writes to a temporary file that is atomically renamed before the source is deleted, so a crash can never lose a backup. Without a `rotate` section, existing backups are left untouched.

## Design notes

- **Request path**: the middleware captures only the values the format uses (strings are shared, not copied), wraps the response writer to record the status code (only if the format uses it, and not for [streaming](#streaming) requests), and does a non-blocking send to the writer's channel (`queueSize`, 8192 entries by default). `Write` is not wrapped: the size comes from `Content-Length`, so body writes add no interpreted calls.
- **Writer**: a single goroutine per file builds the template data (only the fields used), executes the template into a reused buffer, and writes the result through a 64 KiB buffer. Escaping uses a `strings.Replacer`, and execution uses `text/template`, both from the standard library, which runs compiled inside Traefik while the plugin's own code is interpreted. A line whose template fails at runtime is skipped and reported, never written partially. It flushes whenever the queue is empty, so idle periods reach the disk immediately and bursts are batched. Dropped entries and write errors are reported on Traefik's error log.
- **Compression** runs in-process with the standard library's `compress/gzip`, which runs compiled (not interpreted) code: about 380 MB/s at the default level. Faster pure-Go implementations such as `klauspost/compress` can't be used, because they import `unsafe`, which Yaegi rejects.
- **Sharing and reloads**: Traefik calls `New` for every router and on every configuration reload, and it never closes old instances. Writers are therefore kept in a global registry, one per absolute file path and protected by a global lock. Every middleware instance that logs to the same file shares that file's writer, so there are no duplicate file handles, goroutines or concurrent rotations. The most recently applied rotation settings win. The queue size is fixed when the writer is created.
- **Idle files are closed**: a writer that received no entries for 5 to 10 minutes closes its file, and the next entry reopens it (recreating the directory if needed). This releases the file handles of paths that a reload stopped using. External tools can also move an idle log away, and logging continues in a new file. A scheduled rotation still happens while the file is closed.

### Performance

Plugin code is interpreted by Yaegi, so native benchmarks are misleading. Measured with `make yaegi_bench` (48-core Xeon, 2.6 GHz):

| | Under Yaegi | Native |
|---|---|---|
| Added to each request (`ServeHTTP`, including streaming detection) | ~4.2 µs | ~0.1 µs |
| Writer: format and buffer one default-format line | ~25 µs (~40k lines/s per file) | ~3.5 µs |
| Writer: `{.ClientIp}` resolution with trusted forwarders (`BenchmarkFormatClientIp`, compared with `BenchmarkFormat`) | ~+16 µs per line | |

Entries beyond the writer's throughput are dropped (and reported) rather than slowing requests down.

### Known limitations

- **No status for streaming requests.** Requests detected as streaming are logged with `{.Status}` as `-` (see [Streaming](#streaming)). Streaming endpoints that aren't detected (neither Server-Sent Events nor gRPC, and not under `streamingPaths`) are buffered.
- **Queued entries can be lost on shutdown.** Traefik gives plugins no shutdown hook. Entries still queued at shutdown may be lost, although the writer flushes as soon as its queue is empty.
- **Old writers keep a goroutine.** If a reload changes `file`, the writer for the old path closes its file once idle, but its goroutine and timers remain until Traefik restarts.

## Development

```sh
make lint        # golangci-lint
make test        # unit tests with the race detector and coverage
make bench       # benchmarks
make yaegi_test  # run the tests in the Yaegi interpreter, like Traefik does
make yaegi_bench # run the benchmarks in Yaegi: these are the numbers that matter in Traefik
make vendor      # vendor dependencies (required for Yaegi plugins; commit vendor/)
```

### Benchmark regressions

The [Benchmarks](.github/workflows/bench.yml) workflow runs on every pull request. It runs `make yaegi_bench` 6 times on the base branch and 6 times on the pull request, alternating between them, and compares the results with [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat). It fails when a change is statistically significant and exceeds +20% in `sec/op` or +10% in `allocs/op` (see [benchcheck.py](.github/scripts/benchcheck.py)). Shared CI runners are noisy, so a borderline failure is worth re-running before investigating. To compare locally:

```sh
git worktree add /tmp/logger-base main
make -s -C /tmp/logger-base yaegi_bench BENCH_FLAGS=-count=6 2>/dev/null > /tmp/base.txt
make -s yaegi_bench BENCH_FLAGS=-count=6 2>/dev/null > /tmp/head.txt
benchstat -format csv /tmp/base.txt /tmp/head.txt | .github/scripts/benchcheck.py
```

### Local mode

To try the plugin without publishing it, copy the repository to `./plugins-local/src/github.com/gabay/logger` (relative to Traefik's working directory, with `vendor/` included) and declare it in the static configuration:

```yaml
experimental:
  localPlugins:
    logger:
      moduleName: github.com/gabay/logger
```
