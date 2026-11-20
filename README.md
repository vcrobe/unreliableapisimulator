# Unreliable API simulator

A small Go HTTP server that simulates third-party successes, HTTP errors, and slow responses. No external dependencies. Requires Go 1.22 or newer.

## Run

```sh
go run .
```

The default address is `127.0.0.1:8080`. All `*.json` files directly inside `providers/` are loaded at startup. Override either setting:

```sh
go run . -addr :8080 -config ./providers
```

Point your application's third-party base URL to `http://127.0.0.1:8080`. Keep its usual HTTP client, retries, timeouts, request paths, and request bodies.

## Run with Docker Compose

```sh
docker compose up --build
```

The API is available on your PC at `http://127.0.0.1:8080`. Compose mounts the local `providers/` directory into the container read-only, so edit the JSON files on your PC without rebuilding the image. The server loads configuration at startup; apply changes by restarting it:

```sh
docker compose restart simulator
```

To use another directory, create it first and set `PROVIDERS_DIR` to its absolute path when starting Compose:

```sh
PROVIDERS_DIR=/absolute/path/to/providers docker compose up --build
```

Use the same `PROVIDERS_DIR` with subsequent Compose commands. The directory must contain at least one provider JSON file. Other containers on the Compose network can reach the API at `http://simulator:8080`.

## Try the included payment provider

```sh
# 70% immediate success, 20% HTTP 500, 10% success delayed by 10 seconds.
# With this two-second client deadline, delayed successes cause a timeout.
curl -i --max-time 2 -X POST http://127.0.0.1:8080/payments

# Always successful. The body is static, regardless of the requested ID.
curl -i http://127.0.0.1:8080/payments/pay_123

# Call repeatedly: 500, 500, 200, then 200 for every subsequent call.
curl -i -X POST http://127.0.0.1:8080/refunds
```

## Configure a provider

Each file has a provider `name` and an `endpoints` array. See [providers/payments.json](providers/payments.json) for a complete example.

| Endpoint field | Meaning |
| --- | --- |
| `method` | Required HTTP method, such as `GET` or `POST`. |
| `path` | Required Go HTTP route pattern, such as `/payments` or `/payments/{id}`. |
| `policy` | `random` (default) or `sequence`. One response provides fixed behavior. |
| `repeat` | For sequences only: start again after the last response. Defaults to `false`, which keeps returning the last response. |
| `responses` | Required nonempty array of possible responses. |

| Response field | Meaning |
| --- | --- |
| `name` | Optional label used in request logs. |
| `status` | Required final HTTP status, from 200 to 599. |
| `body` | Optional static JSON value. Omit for an empty body. Cannot accompany 204 or 304. |
| `headers` | Optional object of header names and string values, for example `{"Retry-After":"2"}`. |
| `delay` | Optional nonnegative integer wait in milliseconds before sending headers, such as `500` or `10000`. Defaults to no delay. |
| `weight` | Relative likelihood under `random`. Omitted or zero means 1; negative values are rejected. Ignored for sequence selection. |

Weights are relative: 7, 2, and 1 give the same probabilities as 70, 20, and 10. Random outcomes are independent, so ten requests are not guaranteed to contain exactly seven successes, two errors, and one slow response.

Routes follow Go's HTTP routing rules: `{id}` matches one path segment, a trailing slash matches a subtree, and a `GET` route also accepts `HEAD`. Query parameters and request bodies do not affect response selection. Responses do not interpolate path parameters or persist created resources.

All providers share one URL namespace. Conflicting route patterns, unknown configuration fields, and invalid response settings prevent startup. If providers need the same method/path, use separate simulator instances or explicitly prefix their paths and configure your application's base URLs accordingly.

## State and timeouts

Sequences advance when a request reaches the endpoint, including requests whose callers later time out. State is shared by all callers of that endpoint and resets when the simulator restarts. Concurrent requests select sequence entries atomically, but their responses can finish in a different order because of delays. Use a separate simulator process for each isolated test run.

A delayed `200` exercises the caller's own timeout only if its deadline is shorter than the configured delay. An immediate `504` is an HTTP error response and exercises a different path. Delayed handlers stop waiting when the caller cancels or disconnects.

Request logs show the method, path, response label, status, and elapsed time, or cancellation. Configuration changes require a restart. There is no management API, timeline policy, proxy mode, or per-run state in this version.

## Verify

```sh
go test -race ./...
go vet ./...
go build .
```
