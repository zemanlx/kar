# kar

Go 1.27 Kubernetes controller (controller-runtime, client-go dynamic informers). Policies are evaluated with
`github.com/zemanlx/kat`. README.md "How it works" and "What kar can audit" define the behaviour; read them before
changing `internal/controller`.

## Commands

```bash
export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 -p path)
go generate ./...                                              # DeepCopy and CRD from api/v1alpha1/types.go
go test -race -count=1 ./...                                   # ~2.5 min with -race (~80s without)
go test -count=1 -v -run TestName ./internal/controller/      # one test
golangci-lint run ./... && golangci-lint run --build-tags e2e ./... && golangci-lint run --build-tags load ./...
golangci-lint fmt
hack/e2e.sh -test.run=TestName                                 # kind e2e, needs docker and helm
```

- Without `KUBEBUILDER_ASSETS` the envtest tests skip and `go test` still prints `ok`. Run with `-v` and check for
  `SKIP` before calling a change verified.
- `e2e` and `load` files are behind build tags. Plain `go test` and `golangci-lint run` never compile them.
- Lint failing with `typecheck` and "export data version" errors means the binary predates the Go toolchain:
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
- Load tests (`-tags load`) run for up to an hour and stay local; see README "Testing".

## Code

- Bug fixes start with a failing test that reproduces the bug.
- Comments state constraints the code cannot show. No narration, no "changed X to Y".
- A deliberate shortcut gets a `ponytail:` comment with its ceiling and upgrade path:

  ```go
  // ponytail: evaluation runs inside the transform, which client-go asks to be fast; the ceiling is
  // about 1 ms per object on the reflector goroutine. Upgrade path: evaluate in a worker pool.
  ```

- Errors are sentinel vars wrapped with `%w` (err113 is on): `fmt.Errorf("binding %q %w", name, errNotKar)`.
- Generated code: change `api/v1alpha1/types.go`, then run `go generate ./...`. Never edit
  `api/v1alpha1/zz_generated.deepcopy.go` or `charts/kar/crds/kar.zeman.dev_policyexemptions.yaml` by hand.
- `e2e/helpers_test.go` has its own `derivedName`; keep it in step with `internal/controller/binding.go`.

## Boundaries

- Never raise a `TestRegression*` or load-test budget to make a test pass. A breach is a regression to fix or report.
- Do not hold `auditor.mu` while evaluating objects or during API writes; copy what you need under the lock.
- kar writes the PolicyExemption status and handles the API server's too-large error (`StateTooLarge`). Do not add
  a client-side size limit.
- Update README in the same change when behaviour, limits or measured numbers change.
