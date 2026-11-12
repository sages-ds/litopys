# Contributing

Litopys is pre-release, and the contribution model (DCO or CLA) is not decided yet. Until it is, pull requests
cannot be accepted.

Issues are welcome: bug reports, questions about the model, reports from running a client against the server.
A good report says which client and version you used, what you did, what you expected and what happened.

## Security

Do not open a public issue for a vulnerability. Use the repository's private vulnerability reporting
(Security → Report a vulnerability).

## Working on the code

- Go code is formatted with `gofmt` and must pass `go vet` and `staticcheck`.
- The domain in `domain/` is the source of the API: change `domain/verbs.yaml` and regenerate the surface with
  `node domain/tools/gen.mjs`; never edit `core/api/gen/` by hand.
- Tests that need a database run against a disposable PostgreSQL 18; unit tests run with plain `go test`.
