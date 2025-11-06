### Adding new monitors

- when adding a new metric exporter to monitor.go, try to keep the structure consistent with existing monitors, this means:
  - define a new package that contains an implementation of the `metrics.Exporter` interface.
  - conditionally append the new exporter to the `exporters` slice in `monitor.go` based on command-line flags.
  - ensure tests are present for the new metrics added.

### README File

- when new metrics are added, ensure to update the README file to include descriptions of the new metrics.
- when new cli flags are added, ensure to update the README file to include descriptions of the new flags.

### Lint code
- run `make lint-fix` to ensure code is formatted and linted.
