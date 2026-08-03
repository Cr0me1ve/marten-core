package hcore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Cr0me1ve/marten-core/v2/config"
)

func TestRuntimeConfigSnapshotCopiesSettingsAndPlatformUnderOneLock(t *testing.T) {
	static.runtimeConfigLock.Lock()
	originalOptions := static.MartenOptions
	originalPlatform := static.globalPlatformInterface
	originalReady := static.settingsReady.Load()
	static.runtimeConfigLock.Unlock()
	t.Cleanup(func() {
		static.runtimeConfigLock.Lock()
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatform
		static.settingsReady.Store(originalReady)
		static.runtimeConfigLock.Unlock()
	})

	options := config.DefaultMartenOptions()
	options.EnableTun = true
	platform := &MobilePlatformInterface{}
	publishCoreRuntimeSettings(options, true)
	updateCorePlatformInterface(platform)

	snapshot := loadCoreRuntimeConfigSnapshot()
	if snapshot.martenOptions == nil || !snapshot.martenOptions.EnableTun {
		t.Fatalf("snapshot did not retain the published enable_tun setting: %#v", snapshot.martenOptions)
	}
	if !snapshot.settingsReady || snapshot.platformInterface != platform {
		t.Fatalf("snapshot lost ready/platform state: ready=%t platform=%T", snapshot.settingsReady, snapshot.platformInterface)
	}
	options.EnableTun = false
	if !snapshot.martenOptions.EnableTun {
		t.Fatal("snapshot aliases mutable published settings")
	}
}

func TestRuntimeConfigPublicationAndSnapshotAreRaceSafe(t *testing.T) {
	static.runtimeConfigLock.Lock()
	originalOptions := static.MartenOptions
	originalPlatform := static.globalPlatformInterface
	originalReady := static.settingsReady.Load()
	static.runtimeConfigLock.Unlock()
	t.Cleanup(func() {
		static.runtimeConfigLock.Lock()
		static.MartenOptions = originalOptions
		static.globalPlatformInterface = originalPlatform
		static.settingsReady.Store(originalReady)
		static.runtimeConfigLock.Unlock()
	})

	const workers = 8
	const iterations = 200
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				options := config.DefaultMartenOptions()
				options.EnableTun = (worker+iteration)%2 == 0
				publishCoreRuntimeSettings(options, true)
				updateCorePlatformInterface(&MobilePlatformInterface{})
				snapshot := loadCoreRuntimeConfigSnapshot()
				if snapshot.martenOptions == nil || !snapshot.settingsReady || snapshot.platformInterface == nil {
					t.Errorf("incomplete runtime snapshot: %#v", snapshot)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
}

func TestChangeMartenSettingsPersistsBeforePublishingRuntimeSnapshot(t *testing.T) {
	data, err := os.ReadFile("buildconfighelper.go")
	if err != nil {
		t.Fatalf("read buildconfighelper.go: %v", err)
	}
	if !strings.Contains(string(data), "persist core settings") {
		t.Fatal("persistence errors must be returned to the caller")
	}

	parsed, err := parser.ParseFile(token.NewFileSet(), "buildconfighelper.go", data, 0)
	if err != nil {
		t.Fatalf("parse buildconfighelper.go: %v", err)
	}
	var change *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "changeMartenSettings" && function.Recv == nil {
			change = function
			break
		}
	}
	if change == nil {
		t.Fatal("changeMartenSettings not found")
	}

	var persist, publish token.Pos
	ast.Inspect(change.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calledIdentName(call.Fun) {
		case "UpdateInsert":
			persist = call.Pos()
		case "publishCoreRuntimeSettings":
			publish = call.Pos()
		}
		return true
	})
	if persist == token.NoPos || publish == token.NoPos {
		t.Fatalf("expected persistence and publication calls, got persist=%v publish=%v", persist, publish)
	}
	if persist >= publish {
		t.Fatalf("settings snapshot must be persisted before publication: persist=%v publish=%v", persist, publish)
	}
}

func TestPersistedRestorePublishesOptionsAndReadinessAsOneSnapshot(t *testing.T) {
	data, err := os.ReadFile("buildconfighelper.go")
	if err != nil {
		t.Fatalf("read buildconfighelper.go: %v", err)
	}
	if strings.Contains(string(data), "setCoreRuntimeSettingsReady") {
		t.Fatal("settings readiness must be published with options, not by a separate restore step")
	}

	parsed, err := parser.ParseFile(token.NewFileSet(), "buildconfighelper.go", data, 0)
	if err != nil {
		t.Fatalf("parse buildconfighelper.go: %v", err)
	}
	var restore *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "restoreMartenSettings" && function.Recv == nil {
			restore = function
			break
		}
	}
	if restore == nil {
		t.Fatal("restoreMartenSettings not found")
	}

	var publishCall *ast.CallExpr
	ast.Inspect(restore.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && calledIdentName(call.Fun) == "changeMartenSettings" {
			publishCall = call
			return false
		}
		return true
	})
	if publishCall == nil || len(publishCall.Args) != 3 || !isBooleanIdent(publishCall.Args[1], "false") || !isBooleanIdent(publishCall.Args[2], "ready") {
		t.Fatalf("persisted restore must pass its readiness value into the single settings publication: %#v", publishCall)
	}
}
