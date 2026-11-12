# Contributing to kar

kar is maintained by one person on a best-effort basis. Issues and pull requests are welcome.

## Reporting bugs and asking for features

Open a [GitHub issue](https://github.com/zemanlx/kar/issues). For a bug, include the kar version, the Kubernetes
version, the policy and binding involved, what you expected and what happened. The PolicyExemption status and the
controller logs usually show why.

Report security vulnerabilities privately as [SECURITY.md](SECURITY.md) describes, not in an issue.

## Proposing a change

1. For anything bigger than a small fix, open an issue first so we can agree on the approach.
2. Branch from `main` and open a pull request against `main`.
3. Write commit messages and the pull request title as [Conventional Commits](https://www.conventionalcommits.org/):
   `feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`, with `!` for a breaking change.
4. Sign your commits. The `main` branch only accepts signed commits.
5. CI must pass: unit and envtest tests and the kind end-to-end suite on every supported Kubernetes version,
   golangci-lint, and a check that generated code is up to date.

By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).

## Development

You need the Go version in `go.mod`. The end-to-end suite also needs Docker and Helm.

```bash
export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 -p path)
go test -race -count=1 -v ./...   # unit and envtest suites, about 2.5 minutes
hack/e2e.sh                       # kind end-to-end suite
golangci-lint run ./... && golangci-lint run --build-tags e2e ./... && golangci-lint run --build-tags load ./...
golangci-lint fmt
go generate ./...                 # after changing api/v1alpha1/types.go
```

Without `KUBEBUILDER_ASSETS` the envtest tests skip and `go test` still prints `ok`, so check the `-v` output for
`SKIP`. Install the golangci-lint version that `.github/workflows/ci.yml` pins. README [Testing](README.md#testing)
covers the end-to-end options and the local load tests.

## Tests

- New behaviour comes with tests that cover it.
- A bug fix starts with a test that fails without the fix.
- The `TestRegression*` tests hold budgets for kar's per-object costs. Do not raise a budget to make a test pass;
  a breach is a regression to fix, or to discuss in the pull request.

## Code style

- `golangci-lint run` and `golangci-lint fmt` with the repository's [.golangci.yaml](.golangci.yaml) must be clean.
- Errors are sentinel variables wrapped with `%w`, for example `fmt.Errorf("binding %q %w", name, errNotKar)`.
- Comments explain constraints the code cannot show, not what the next line does.
- Generated files (`api/v1alpha1/zz_generated.deepcopy.go` and the CRD in `charts/kar/crds/`) come from
  `go generate ./...`; never edit them by hand.
- When behaviour, limits or measured numbers change, update README in the same pull request.
- Workflow changes keep actions pinned by commit SHA and permissions minimal; see the "CI and supply chain" section
  of [AGENTS.md](AGENTS.md).
