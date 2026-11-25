# db-proxy-ro

A read-only proxy for **PostgreSQL** and **MySQL / MariaDB**.

Clients connect to the proxy as if it were the database itself — with no
password, or with any user name and password — and name only the database.
The proxy looks the database up in its parameter files, connects to the real
server with the real credentials and relays the traffic. Reads pass; anything
that would change data is refused.

Typical use: give people, scripts or AI agents a way to query production
without handing out credentials and without the risk of an accidental write.

Русская версия: [README.ru.md](README.ru.md)

```
psql / mysql / app ──► db-proxy-ro ──► real database
   (no password)        │  refuses writes     (real credentials
                        │  logs every query    from params/*.toml)
```

## Quick start

```sh
git clone https://github.com/ivanov39/db-proxy-ro.git && cd db-proxy-ro

mkdir -p params/pgsql params/mysql
cp examples/params/pgsql/example.toml params/pgsql/app.toml   # then edit
cp examples/params/mysql/example.toml params/mysql/shop.toml  # then edit

docker compose up -d --build
```

Connect, naming the database and nothing else:

```sh
psql  -h 127.0.0.1 -p 25432 app_db
mysql -h 127.0.0.1 -P 23306 --protocol=tcp app_db
```

```
app_db=> delete from users;
ERROR:  db-proxy-ro: DELETE statements are not allowed: the proxy is read-only
```

## Database parameters

One TOML file per database, in `params/pgsql/` or `params/mysql/`:

```toml
host     = "pg.example.internal"
port     = 5432
user     = "app_user"
password = "secret"
name     = "app_db"            # database name on the server

# alias   = "app_db_prod"      # optional: the name clients use (default: name)
# sslmode = "prefer"           # optional: disable | prefer | require | verify-full
```

* Files are read **once, at start-up**. Restart the container after changing them:
  `docker compose restart`.
* The proxy refuses to start if a file is malformed or if two files of the same
  DBMS define the same client-facing name. The reason is in `logs/error.log`.
* No database is contacted at start-up. A backend connection is opened when a
  client connects, one backend connection per client connection.
* Host names are resolved from inside the container. With the provided
  `docker-compose.yml` (host networking) that is the same as on the host,
  VPN routes and internal DNS included.

`params/` is in `.gitignore`; do not commit credentials.

## Settings

Environment variables; with Docker Compose put them into `.env`
(see [.env.example](.env.example)).

| Variable | Default (compose) | Meaning |
|---|---|---|
| `DBPROXY_PGSQL_LISTEN` | `127.0.0.1:25432` | Address for PostgreSQL clients |
| `DBPROXY_MYSQL_LISTEN` | `127.0.0.1:23306` | Address for MySQL clients |
| `DBPROXY_LOG_LEVEL` | `info` | Service log level: `debug`, `info`, `warn`, `error` |
| `DBPROXY_CONNECT_TIMEOUT` | `10s` | Time limit for connecting to a backend |
| `DBPROXY_MYSQL_SERVER_VERSION` | `8.0.36-db-proxy-ro` | Version shown to MySQL clients in the greeting |
| `DBPROXY_LOG_SQL_MAX_LEN` | `8192` | Longer SQL texts are truncated in the query log; `0` = no limit |
| `DBPROXY_SHUTDOWN_TIMEOUT` | `5s` | Grace period for open sessions on shutdown |
| `DBPROXY_PARAMS_DIR` | `/params` | Directory with the parameter files |
| `DBPROXY_LOG_DIR` | `/logs` | Where the container redirects its logs; empty = `docker logs` |
| `DBPROXY_UID`, `DBPROXY_GID` | `1000` | User the container runs as (compose only) |

> **The proxy does not authenticate clients.** Whoever can reach its ports can
> read the configured databases. Keep it on `127.0.0.1` or on a network you trust.

## Logs

The process writes the query log to stdout and the service log to stderr, both
as JSON lines. The container entrypoint redirects them to files in the mounted
`./logs` directory:

* `logs/query.log` — every request: database, client, SQL, duration, rows, and
  the backend error if any; refused requests carry `"msg":"rejected"` and a `reason`.
* `logs/error.log` — start-up, shutdown, failed backend connections, broken sessions.

```json
{"time":"…","level":"INFO","msg":"query","dbms":"pgsql","db":"app_db","conn":1,"client":"127.0.0.1:51054","user":"alice","kind":"query","sql":"select count(*) from t","duration_ms":0.707,"rows":1}
{"time":"…","level":"WARN","msg":"rejected","dbms":"pgsql","db":"app_db","conn":1,"client":"127.0.0.1:51054","user":"alice","kind":"query","sql":"drop table t","reason":"DROP statements are not allowed: the proxy is read-only"}
```

The files are opened in append mode, so `logrotate` with `copytruncate` works.
SQL texts are logged as sent, including any literals in them.

## How read-only is enforced

Two independent layers; a write has to get past both.

1. **SQL guard in the proxy.** Every SQL text (plain queries and prepared
   statements) is tokenized and checked against an allow-list: `SELECT`, `WITH`,
   `SHOW`, `EXPLAIN`, transaction control, session `SET`, and a few more. Inside an
   allowed statement, data-changing constructs are refused too: data-modifying
   CTEs, `SELECT … INTO`, `FOR UPDATE`, `COPY … FROM`, `EXPLAIN ANALYZE` of a
   write, functions such as `nextval()` or `set_config()`. Anything the guard
   does not understand is refused.
2. **Read-only session on the backend.** Each backend session is opened in
   read-only transaction mode (`default_transaction_read_only=on` for
   PostgreSQL, `SET SESSION TRANSACTION READ ONLY` for MySQL), so the database
   itself refuses writes the guard cannot see, such as a write inside a stored
   function. The guard refuses every statement that could switch this mode off,
   and on PostgreSQL 14+ the proxy also drops a session if the server reports
   that the mode has changed.

The guard is deliberately strict where SQL is ambiguous. A query is accepted
only if it is harmless under every server setting that changes how it is
parsed (`standard_conforming_strings`, `NO_BACKSLASH_ESCAPES`, `ANSI_QUOTES`).

For the strongest guarantee, also give the proxy a database account that has
only `SELECT` privileges. The proxy is then a second line of defence, not the
only one.

## Limitations

* **No TLS and no authentication towards clients.** Clients must connect with
  TLS disabled or optional (`sslmode=prefer`, `--ssl-mode=PREFERRED` — the
  defaults). The connection from the proxy to the database can use TLS (`sslmode`).
* **Strictness causes some false refusals.** Examples: an unquoted column named
  `update` or `delete` (quote it), `SELECT … INTO @var`, MySQL executable
  comments `/*! … */`, `CALL`, SQL-level `PREPARE`/`EXECUTE` in MySQL, `DO`.
  Tools that need these (for example `mysqldump`, which locks tables) will not
  work through the proxy; `pg_dump` is not supported either.
* **PostgreSQL**: needs a direct connection to the server, or a pooler in
  session mode — a transaction-mode pooler would not keep the read-only
  setting, and the proxy refuses to work if it cannot confirm the setting.
  Replication connections and the function-call protocol are refused. Of the
  client's start-up parameters only a safe subset is forwarded
  (`application_name`, `client_encoding`, `search_path`, …); `options` is dropped.
* **PostgreSQL older than 14** does not report changes of the read-only setting,
  so the "session left read-only mode" watchdog is unavailable there. This
  matters only if the database already contains a function that turns the
  setting off.
* **MySQL / MariaDB**: tested with MySQL 5.7 and 8.0 and MariaDB 11, with
  `mysql_native_password` and `caching_sha2_password` accounts
  (`sha256_password` is implemented but not covered by tests; other plugins,
  such as MariaDB `ed25519`, are not supported). `COM_CHANGE_USER`, replication commands and `LOAD DATA LOCAL` are
  refused. Protocol compression is not offered. Temporary tables remain
  writable in a MySQL read-only transaction, but only statements on the
  allow-list reach the server.
* Parameter files are re-read only on restart.

## Architecture

Go, Clean Architecture; dependencies point inwards.

```
cmd/db-proxy-ro            entry point
internal/
  domain/                  entities and rules, no dependencies
    sqlguard/              the read-only policy: lexer + allow-list
  usecase/                 application logic and ports (TargetSource, AuditLog)
  infrastructure/          adapters: env config, TOML files, logs
  transport/               adapters: wire protocols
    netserver/             TCP accept loop, graceful shutdown
    pgsql/                 PostgreSQL protocol relay
    mysql/                 MySQL protocol relay
  app/                     composition root
```

After the handshake both protocols are relayed as they are, message by
message; only client messages that carry SQL are parsed. Result sets are
streamed and never buffered as a whole.

## Development

```sh
make test               # unit tests
make test-integration   # real PostgreSQL 13/16, MySQL 5.7/8.0, MariaDB 11 in Docker
make lint
make build              # ./db-proxy-ro; settings via the DBPROXY_* variables
```

## License

[MIT](LICENSE)
