# Contributing

Thanks for your interest in improving the NetBox data source for Grafana.

## Reporting bugs and requesting features

Use the [issue templates](https://github.com/netboxlabs/netboxlabs-grafana-datasource/issues/new/choose)
— see [SUPPORT.md](./SUPPORT.md) for where to get help. For anything beyond a small fix,
open an issue first so the approach can be agreed before you invest time in a pull request.

## Development setup

The **Development** section of the [README](./README.md#development) covers the frontend
(npm), backend (Go/mage), and the docker-compose dev Grafana. The README's
[Architecture](./README.md#architecture) section explains how the plugin is put together.

## Pull requests

- Keep PRs small and focused on one change.
- Use a [Conventional Commits](https://www.conventionalcommits.org/) style title, e.g.
  `fix(query): handle paginated brief responses` — PR titles become the commit history.
- Make sure the checks pass locally before pushing:

  ```bash
  npm run typecheck && npm run lint && npm run test:ci   # frontend
  go test ./...                                          # backend
  ```

- Add or update tests for behavior you change. E2E tests (`npm run e2e`) run in CI.
- Update the README/docs when you change user-facing behavior.

## License

By contributing, you agree that your contributions are licensed under the
[Apache-2.0 license](./LICENSE) that covers the project.
