# rosactl

CLI tool (`rosactl`) for managing AWS infrastructure (VPC, IAM, OIDC) for ROSA hosted clusters via CloudFormation stacks. Optionally deployable as a Lambda function for event-driven workflows.

**Tech stack**: Go 1.26, Cobra, AWS SDK v2, CloudFormation, rosa-hyperfleet-api clientset, Ginkgo/Gomega

## Key Directories

| Path                                 | Purpose                                                                                                            |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------ |
| `cmd/rosactl/`                       | Binary entry point                                                                                                 |
| `internal/commands/`                 | Cobra CLI subcommands (bootstrap, cluster, clusteriam, clusteroidc, clustervpc, handler, login, nodepool, version) |
| `internal/services/`                 | Business logic shared by CLI commands and Lambda handler                                                           |
| `internal/aws/cloudformation/`       | CloudFormation client and stack operations                                                                         |
| `internal/cloudformation/templates/` | Embedded CloudFormation templates (go:embed)                                                                       |
| `test/localstack/`                   | Integration tests against LocalStack                                                                               |
| `docs/`                              | Architecture, guides, and specs                                                                                    |

## Commands

```bash
make build          # Build ./bin/rosactl
make test           # Unit tests (go test -race)
make test-localstack # Integration tests (requires LocalStack Pro + LOCALSTACK_AUTH_TOKEN)
make verify         # fmt-check + vet + lint
make fmt            # Auto-format Go code
```

## Important Context

- **Stack naming**: `rosa-{cluster-name}-vpc` and `rosa-{cluster-name}-iam`
- **Templates embedded**: CloudFormation templates are compiled into the binary via `go:embed` — edit files under `internal/cloudformation/templates/`
- **Service layer**: `internal/services/` is the shared layer used by both CLI and Lambda handler — add new business logic here, not in commands
- **Lambda is optional**: All cluster management works without Lambda; it's only needed for event-driven workflows
- **Conventional commits**: Use `feat:`, `fix:`, `docs:`, `chore:` prefixes — drives semantic versioning via `make release`
- **Architecture**: `docs/architecture/ARCHITECTURE.md`
