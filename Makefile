.ONESHELL:
PRODUCT_NAME=marten-core
BASENAME=$(PRODUCT_NAME)
BINDIR=bin
LIBNAME=$(PRODUCT_NAME)
CLINAME=MartenCli

BRANCH=$(shell git branch --show-current)
VERSION=$(shell git describe --tags || echo "unknown version")
TAGS=with_gvisor,with_quic,with_wireguard,with_ech,with_utls,with_clash_api,with_grpc
IOS_ADD_TAGS=with_dhcp,with_low_memory,with_conntrack
GOBUILDLIB=CGO_ENABLED=1 go build -trimpath -tags $(TAGS) -ldflags="-w -s" -buildmode=c-shared
GOBUILDSRV=CGO_ENABLED=1 go build -ldflags "-s -w" -trimpath -tags $(TAGS)
ANDROID_BOOTTIME_GOROOT ?= $(HOME)/.cache/marten/go-android-boottime/$(shell go env GOVERSION)
CORE_REVISION ?= $(shell git rev-parse HEAD)
SING_BOX_DIR ?= ../marten-sing-box
TURNCOAT_DIR ?= ../TURNcoat
SING_BOX_REVISION ?= $(shell git -C "$(SING_BOX_DIR)" rev-parse HEAD 2>/dev/null || echo 450cefb48900)
TURNCOAT_REVISION ?= $(shell git -C "$(TURNCOAT_DIR)" rev-parse HEAD 2>/dev/null || echo 7716c0d0a86d)
SOURCE_DIRTY ?= $(if $(shell git status --porcelain --untracked-files=normal),true,false)
MOBILE_PACKAGE=github.com/Cr0me1ve/marten-core/platform/mobile
# Keep DWARF and the symbol table in the prebuilt AAR. Android strips the copy
# packaged in the APK, while Crashlytics consumes this unstripped input to
# symbolicate native Go failures from libmarten-core.so.
MOBILE_LDFLAGS=-X $(MOBILE_PACKAGE).CoreRevision=$(CORE_REVISION) -X $(MOBILE_PACKAGE).SingBoxRevision=$(SING_BOX_REVISION) -X $(MOBILE_PACKAGE).TurncoatRevision=$(TURNCOAT_REVISION) -X $(MOBILE_PACKAGE).SourceDirty=$(SOURCE_DIRTY)
GOMOBILE_BIN=$(shell go env GOPATH)/bin/gomobile
GOBIND_BIN=$(shell go env GOPATH)/bin/gobind

.PHONY: protos
protos:
	go install github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc@latest
	# protoc --go_out=./ --go-grpc_out=./ --proto_path=martenrpc martenrpc/*.proto
	# for f in $(shell find v2 -name "*.proto"); do \
	# 	protoc --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go_out=./ --go-grpc_out=./  $$f; \
	# done
	# for f in $(shell find extension -name "*.proto"); do \
	# 	protoc --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go_out=./ --go-grpc_out=./  $$f; \
	# done
	protoc --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go_out=./ --go-grpc_out=./  $(shell find v2 -name "*.proto") $(shell find extension -name "*.proto")
	protoc --doc_out=./docs  --doc_opt=markdown,martenrpc.md $(shell find v2 -name "*.proto") $(shell find extension -name "*.proto")
	# protoc --js_out=import_style=commonjs,binary:./extension/html/rpc/ --grpc-web_out=import_style=commonjs,mode=grpcwebtext:./extension/html/rpc/ $(shell find v2 -name "*.proto") $(shell find extension -name "*.proto")
	# npx browserify extension/html/rpc/extension.js >extension/html/rpc.js


lib_install: prepare
	test -x "$(GOMOBILE_BIN)" || go install -v github.com/sagernet/gomobile/cmd/gomobile@v0.1.12
	test -x "$(GOBIND_BIN)" || go install -v github.com/sagernet/gomobile/cmd/gobind@v0.1.12
	test -d node_modules || npm install

headers:
	go build -buildmode=c-archive -o $(BINDIR)/ ./platform/desktop2

ANDROID_TARGET ?= android
# Preserve the feature set validated by the Android release acceptance.
ANDROID_TAGS ?= with_gvisor,with_quic,with_wireguard,with_utls,with_clash_api,with_conntrack
android: lib_install
	./scripts/prepare_android_boottime_go.sh "$(ANDROID_BOOTTIME_GOROOT)"
	MARTEN_ANDROID_CORE_DIR="$(CURDIR)" \
	MARTEN_ANDROID_SING_BOX_DIR="$(abspath $(SING_BOX_DIR))" \
	MARTEN_ANDROID_TURNCOAT_DIR="$(abspath $(TURNCOAT_DIR))" \
	MARTEN_ANDROID_GOROOT="$(ANDROID_BOOTTIME_GOROOT)" \
	MARTEN_ANDROID_TARGET="$(ANDROID_TARGET)" \
	MARTEN_ANDROID_TAGS="$(ANDROID_TAGS)" \
	MARTEN_ANDROID_LDFLAGS="$(MOBILE_LDFLAGS)" \
	MARTEN_ANDROID_OUTPUT="$(CURDIR)/$(BINDIR)/$(LIBNAME).aar" \
	./scripts/build_android_aar.sh

ios-full: lib_install
	gomobile bind -v  -target ios,iossimulator,tvos,tvossimulator,macos -libname=$(PRODUCT_NAME) -tags=$(TAGS),$(IOS_ADD_TAGS) -trimpath -ldflags="-w -s" -o $(BINDIR)/$(PRODUCT_NAME).xcframework github.com/sagernet/sing-box/experimental/libbox ./platform/mobile 
	mv $(BINDIR)/$(PRODUCT_NAME).xcframework $(BINDIR)/$(LIBNAME).xcframework 
	cp MartenCore.podspec $(BINDIR)/$(LIBNAME).xcframework/

ios: lib_install
	gomobile bind -v  -target ios -libname=$(PRODUCT_NAME) -tags=$(TAGS),$(IOS_ADD_TAGS) -trimpath -ldflags="-w -s" -o $(BINDIR)/MartenCore.xcframework github.com/sagernet/sing-box/experimental/libbox ./platform/mobile
	cp Info.plist $(BINDIR)/MartenCore.xcframework/


webui:
	curl -L -o webui.zip  https://github.com/MetaCubeX/Yacd-meta/archive/gh-pages.zip
	unzip -d ./ -q webui.zip
	rm webui.zip
	rm -rf bin/webui
	mv Yacd-meta-gh-pages bin/webui

.PHONY: build
windows-amd64: prepare
	env GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc $(GOBUILDLIB) -o $(BINDIR)/$(LIBNAME).dll ./platform/desktop
	go install -mod=readonly github.com/akavel/rsrc@latest ||echo "rsrc error in installation"
	go run ./cli tunnel exit
	cp $(BINDIR)/$(LIBNAME).dll ./$(LIBNAME).dll 
	$$(go env GOPATH)/bin/rsrc -ico ./assets/marten-cli.ico -o ./cmd/bydll/cli.syso ||echo "rsrc error in syso"
	env GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc CGO_LDFLAGS="$(LIBNAME).dll" $(GOBUILDSRV) -o $(BINDIR)/$(CLINAME).exe ./cmd/bydll
	rm ./$(LIBNAME).dll
	make webui
	

linux-amd64: prepare
	mkdir -p $(BINDIR)/lib
	env GOOS=linux GOARCH=amd64 $(GOBUILDLIB) -o $(BINDIR)/lib/$(LIBNAME).so ./platform/desktop
	mkdir lib
	cp $(BINDIR)/lib/$(LIBNAME).so ./lib/$(LIBNAME).so
	env GOOS=linux GOARCH=amd64  CGO_LDFLAGS="./lib/$(LIBNAME).so" $(GOBUILDSRV) -o $(BINDIR)/$(CLINAME) ./cmd/bydll
	rm -rf ./lib
	chmod +x $(BINDIR)/$(CLINAME)
	make webui
linux-arm64:
	mkdir -p $(BINDIR)/lib
	env GOOS=linux GOARCH=arm64 $(GOBUILDLIB) -o $(BINDIR)/lib/$(LIBNAME).so ./platform/desktop
	mkdir lib
	cp $(BINDIR)/lib/$(LIBNAME).so ./lib/$(LIBNAME).so
	env GOOS=linux GOARCH=arm64  CGO_LDFLAGS="./lib/$(LIBNAME).so" $(GOBUILDSRV) -o $(BINDIR)/$(CLINAME) ./cmd/bydll
	rm -rf ./lib
	chmod +x $(BINDIR)/$(CLINAME)
	make webui


linux-custom: prepare
	mkdir -p $(BINDIR)/
	#env GOARCH=mips $(GOBUILDSRV) -o $(BINDIR)/$(CLINAME) ./cmd/
	go build -ldflags "-s -w" -trimpath -tags $(TAGS) -o $(BINDIR)/$(CLINAME) ./cmd/main
	chmod +x $(BINDIR)/$(CLINAME)
	make webui

macos-amd64:
	env GOOS=darwin GOARCH=amd64 CGO_CFLAGS="-mmacosx-version-min=10.11 -O2" CGO_LDFLAGS="-mmacosx-version-min=10.11 -O2 -lpthread" CGO_ENABLED=1 go build -trimpath -tags $(TAGS),$(IOS_ADD_TAGS) -buildmode=c-shared -o $(BINDIR)/$(LIBNAME)-amd64.dylib ./platform/desktop
macos-arm64:
	env GOOS=darwin GOARCH=arm64 CGO_CFLAGS="-mmacosx-version-min=10.11 -O2" CGO_LDFLAGS="-mmacosx-version-min=10.11 -O2 -lpthread" CGO_ENABLED=1 go build -trimpath -tags $(TAGS),$(IOS_ADD_TAGS) -buildmode=c-shared -o $(BINDIR)/$(LIBNAME)-arm64.dylib ./platform/desktop
	
macos: prepare macos-amd64 macos-arm64 
	
	lipo -create $(BINDIR)/$(LIBNAME)-amd64.dylib $(BINDIR)/$(LIBNAME)-arm64.dylib -output $(BINDIR)/$(LIBNAME).dylib
	cp $(BINDIR)/$(LIBNAME).dylib ./$(LIBNAME).dylib 
	mv $(BINDIR)/$(LIBNAME)-arm64.h $(BINDIR)/desktop.h 
	# env GOOS=darwin GOARCH=amd64 CGO_CFLAGS="-mmacosx-version-min=10.15" CGO_LDFLAGS="-mmacosx-version-min=10.15" CGO_LDFLAGS="bin/$(LIBNAME).dylib"  CGO_ENABLED=1 $(GOBUILDSRV)  -o $(BINDIR)/$(CLINAME) ./cmd/bydll
	# rm ./$(LIBNAME).dylib
	# chmod +x $(BINDIR)/$(CLINAME)

prepare: 
	go mod tidy

clean:
	rm $(BINDIR)/*




release: # Create a new tag for release.	
	@bash -c '.github/change_version.sh'
	
