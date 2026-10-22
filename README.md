# marten-core

Marten Core is the Go runtime used by [marten-app](https://github.com/Cr0me1ve/marten-app). It builds client configuration, exposes the runtime API used by the Flutter application, and packages the native integration around [marten-sing-box](https://github.com/Cr0me1ve/marten-sing-box).

## Layout

- `v2/` contains the runtime API, configuration, profile handling, and generated protocol bindings.
- `platform/` contains platform adapters and mobile bindings.
- `extension/` contains optional runtime extensions.
- `cmd/` contains command-line entry points.
- `docs/` contains generated API documentation.

## Tooling

Use the Go version declared in [go.mod](go.mod). Clone [marten-sing-box](https://github.com/Cr0me1ve/marten-sing-box) alongside this checkout; the main module uses its bundled Xray transport changes. This snapshot does not vendor the Marten-specific external modules used for TURNcoat and ICMP support. A full build therefore requires those module sources to be available to Go's module resolver.

## Verify

From the repository root, run the full Go test suite:

```sh
go test ./...
```

For the runtime lifecycle package with the race detector:

```sh
go test -race ./v2/hcore
```

Build the mobile runtime with the feature tags used by the app:

```sh
go build -tags=with_gvisor,with_quic,with_wireguard,with_ech,with_utls,with_clash_api,with_grpc ./platform/mobile
```

Platform packaging targets are provided by [Makefile](Makefile). They require the appropriate platform toolchain in addition to the Go dependencies.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security-sensitive reports belong in the private GitHub vulnerability reporting flow described in [SECURITY.md](SECURITY.md).

## License and attribution

Marten Core is distributed under the [Hiddify Extended GNU General Public License v3](LICENSE.md). Attribution and a short description of the Marten changes are in [NOTICE.md](NOTICE.md).
