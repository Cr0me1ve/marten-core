@echo off
set GOOS=windows
set GOARCH=amd64
set CC=x86_64-w64-mingw32-gcc
set CGO_ENABLED=1
go run ./cli tunnel exit
del bin\marten-core.dll bin\MartenCli.exe
set CGO_LDFLAGS=
go build -trimpath -tags with_gvisor,with_quic,with_wireguard,with_ech,with_utls,with_clash_api,with_grpc -ldflags="-w -s" -buildmode=c-shared -o bin\marten-core.dll ./custom
copy bin\marten-core.dll .
set CGO_LDFLAGS="marten-core.dll"
go build -o bin\MartenCli.exe ./cli/bydll/
del marten-core.dll
