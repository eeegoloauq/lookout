# Development

Go, no cgo. Build and test:

```sh
CGO_ENABLED=0 go build -o lookout ./cmd/lookout
go vet ./... && go test -race ./...
```

`go run ./cmd/lookout demo` serves the status page with made-up data. Nothing is probed and no
config is read.

[design.md](design.md) explains the less obvious decisions: no database, no keep-alives, two
statuses instead of three.
