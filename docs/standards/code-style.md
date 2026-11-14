# Code style

Go's own conventions, taken from *Learning Go* (Jon Bodner) and the
[Google Go Style Guide](https://google.github.io/styleguide/go/). The goal is code a reader
understands on the first pass. When two rules pull apart, clarity wins over cleverness and over
brevity.

Each rule says who enforces it: **lint** (`make lint` fails, so CI fails) or **review** (a person
or an AI reviewer has to catch it). `make fmt` handles formatting (gofumpt, imports grouped as
standard library / others / this module); don't format by hand.

## Naming

- Mixed caps, never underscores: `userID`, `maxRetries`. Initialisms keep one case: `ID`, `URL`,
  `HTTP` (`userID`, not `userId`). — lint (revive `var-naming`)
- A name's length matches its reach: `i` in a three-line loop, `pendingInvoices` at package level.
  — review
- Don't repeat the package in a name: `task.Service`, not `task.TaskService`. — review
- Package names are short lowercase nouns that say what's inside. Never `util`, `common`,
  `helpers`, `models`: they say nothing and collect unrelated code. — review
- Receivers are one or two letters from the type, the same in every method: `func (s *Service)`,
  `func (tr taskRoutes)`. — the same in every method: lint (revive `receiver-naming`); short and
  from the type: review
- Don't rename an import unless two imports clash or the package name is unclear
  (`stdlog "log"` next to zerolog's `log` is fine; alone it isn't). — review
- No `Get` prefix on getters (`Title()`, not `GetTitle()`); don't write getters or setters for a
  struct field at all unless an interface needs them. — review
- Constants are mixed caps like everything else (`maxBodyBytes`), never `MAX_BODY_BYTES`. — lint
  (revive `var-naming`)

## Declarations

- Inside functions use `:=`; use `var x T` for a zero value you fill in later. — review
- No mutable package-level variables: pass state in through a struct. A package-level `var` needs a
  `//nolint:gochecknoglobals` with the reason (e.g. `-X` build flags can only set one). — lint
  (gochecknoglobals)
- No `init()`: setup runs in `main` (or `TestMain`), where order and errors are visible. — lint
  (gochecknoinits)
- Group related constants in one `const (...)` block. Use `iota` only when the values don't matter
  (they're never stored or sent); set values explicitly when they do. — review
- Name the fields in struct literals: `Task{ID: id, Title: t}`. Always for another package's type;
  for our own, leave them out only in table-test rows whose struct is declared right above. — review
- Prefer `make([]T, 0, n)` when the size is known, so the slice doesn't regrow. — review

## Functions and control flow

- The happy path runs down the left edge: handle the error or edge case, return early, and keep
  the normal code unindented. No `else` after a `return`. — lint (revive `indent-error-flow`,
  `superfluous-else`)
- No named results, except to change the returned error in a `defer`. No bare `return` in a
  function that returns values. — lint (nonamedreturns, nakedret)
- `context.Context` is the first parameter, named `ctx`, and is never stored in a struct. — lint
  (revive `context-as-argument`, containedctx)
- Never shadow a built-in (`len`, `new`, `error`) or, by accident, an outer variable with `:=`.
  — lint (revive `redefines-builtin-id`); outer variables: review
- Type assertions use the two-value form: `v, ok := x.(T)`. — lint (forcetypeassert)
- A `switch` over our own enum lists every value, so adding a value fails lint until every switch
  handles it. Don't add `default` to such a switch: the linter counts it as covering everything.
  — lint (exhaustive)
- Use `panic` only for a bug (a state the code should make impossible), never for bad input or a
  failed call. — review

## Types and interfaces

- Accept interfaces, return concrete types. Define an interface where it's used (the consumer),
  with only the methods that consumer calls, and only once a second implementation or a test needs
  it. — lint for returns (ireturn); the rest: review
- If the standard library has the interface (`io.Reader`, `fmt.Stringer`), use it. — review
- Use a named type to make a value's meaning and limits part of its type (`type Kind int`). — review
- Avoid `any` in our own APIs; it moves type errors from the compiler to run time. — review
- Pointers only to share or change a value, or for large structs; not "for speed" by default.
  — review

## Errors

The rules for which error to return live in [errors.md](errors.md). Style only:

- Error strings are lowercase with no final punctuation: they get wrapped into longer messages.
  A string may start with a name that's capitalized anyway (`DATABASE_URL is required`,
  `HTTP ...`). — lint (revive `error-strings`)
- Put `%w` at the end (`"read config: %w"`), so the chain reads outermost first; don't repeat what
  the wrapped error already says (pgx's errors name the host they tried). — review
- Wrap with `%w` to keep the cause, and compare with `errors.Is` / `errors.As` (or
  `errors.AsType`), never `==` or a type assertion on a wrapped error. — lint (errorlint)
- An error type's name ends in `Error`; a sentinel's starts with `Err`. — lint (errname)

## Concurrency

- Start a goroutine only when you know how it ends: a `context`, a closed channel or a
  `sync.WaitGroup`. A goroutine that never ends leaks its memory. — review
- The sender closes a channel, and only when a receiver waits for the close. — review
- Keep concurrency inside a function: callers get a plain blocking call. — review
- Measure before adding concurrency; it often makes code slower to read and no faster to run.
  — review

## Comments

- Every exported name has a doc comment, a full sentence that starts with the name. — lint
  (revive `exported`). Unexported names that have a doc comment follow the same form. — review
- Comments say *why*: a reason, a trap, what breaks if the line changes. Don't restate what the
  code says, and don't tell its history (that's the commit message's job). — review
- A `//nolint` names the linter and gives the reason: `//nolint:gosec // G204: no shell, our own
  arguments`. — lint (nolintlint)

## Libraries

- Use JSON only through `internal/json` (`Marshal`, `Unmarshal`, `DecodeStrict`), never go-json
  or `encoding/json` directly, so the library can be replaced in that one package. Tests may use
  `encoding/json` to check that replies are plain JSON. — lint (depguard)
- Import pgx only in `internal/postgres` and the code that opens or builds databases
  (`internal/startup`, `internal/testdb`); services recognise database errors through
  `internal/postgres`. Tests may use pgx to seed and read rows. — lint (depguard)
- Queue, handle and schedule jobs through `internal/queue`; `cmd/` picks the implementation. asynq
  and go-redis stay in `internal/queue/valkey`, `internal/startup` and `internal/testqueue`, and
  `internal/queue/valkey` in `cmd/`. — lint (depguard)

## HTTP, database and tests

These have their own pages: [http-api.md](http-api.md), [database.md](database.md),
[testing.md](testing.md), [logging.md](logging.md). The linter also checks that HTTP bodies are
closed and requests carry a context (bodyclose, noctx), that row loops check `rows.Err()`
(rowserrcheck), that zerolog events are sent (zerologlint), and the security checks in gosec.
