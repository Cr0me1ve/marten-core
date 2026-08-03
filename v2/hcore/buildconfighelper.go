package hcore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"github.com/Cr0me1ve/marten-core/v2/db"
	hcommon "github.com/Cr0me1ve/marten-core/v2/hcommon"
	hutils "github.com/Cr0me1ve/marten-core/v2/hutils"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func BuildConfigJson(in *StartRequest) (string, error) {
	Log(LogLevel_DEBUG, LogType_CORE, "Stating Service ")

	parsedContent, err := BuildConfig(in)
	if err != nil {
		return "", err
	}
	return config.ToJson(*parsedContent)
}

func BuildConfig(in *StartRequest) (*option.Options, error) {
	return buildConfigWithRuntimeConfig(in, loadCoreRuntimeConfigSnapshot())
}

func buildConfigWithRuntimeConfig(
	in *StartRequest,
	runtimeConfig coreRuntimeConfigSnapshot,
) (*option.Options, error) {
	Log(LogLevel_DEBUG, LogType_CORE, "Building Config...")
	content := in.ConfigContent

	if content == "" {
		fileContent, err := os.ReadFile(in.ConfigPath)
		if err != nil {
			return nil, err
		}
		content = string(fileContent)
	}
	// Log(LogLevel_DEBUG, LogType_CORE, "Parsing Config... ", in.ConfigPath, " content:", content, "-")
	Log(LogLevel_DEBUG, LogType_CORE, "Parsing Config... ")

	parsedContent, err := readOptions(content)
	Log(LogLevel_DEBUG, LogType_CORE, "Parsed")

	if err != nil {
		return nil, err
	}

	if !in.EnableRawConfig {
		if runtimeConfig.martenOptions == nil {
			return nil, fmt.Errorf("core settings unavailable")
		}
		// hcontent, err := json.MarshalIndent(static.MartenOptions, "", " ")
		// if err != nil {
		// 	return nil, err
		// }

		// Log(LogLevel_DEBUG, LogType_CORE, "Building config ", string(hcontent))
		// Log(LogLevel_DEBUG, LogType_CORE, "Building config ")
		built, err := config.BuildConfig(*runtimeConfig.martenOptions, parsedContent)
		if err != nil {
			return nil, err
		}
		logBuiltConfigStateWithRuntimeConfig(built, runtimeConfig)
		return built, nil
	}

	logBuiltConfigStateWithRuntimeConfig(&parsedContent, runtimeConfig)
	return &parsedContent, nil
}

func logBuiltConfigState(options *option.Options) {
	logBuiltConfigStateWithRuntimeConfig(options, loadCoreRuntimeConfigSnapshot())
}

func logBuiltConfigStateWithRuntimeConfig(
	options *option.Options,
	runtimeConfig coreRuntimeConfigSnapshot,
) {
	if options == nil {
		return
	}
	inboundTypes := make([]string, 0, len(options.Inbounds))
	hasTun := false
	for _, inbound := range options.Inbounds {
		inboundTypes = append(inboundTypes, inbound.Type)
		if inbound.Type == C.TypeTun {
			hasTun = true
		}
	}
	static.lastConfigHasTun.Store(hasTun)
	static.lastConfigInboundCount.Store(int64(len(options.Inbounds)))
	if !RuntimeDebugEnabled() {
		return
	}
	Log(LogLevel_DEBUG, LogType_CORE, fmt.Sprintf(
		"core config state enable_tun=%t platform_interface=%t inbound_count=%d inbound_types=%s",
		runtimeConfig.martenOptions != nil && runtimeConfig.martenOptions.EnableTun,
		runtimeConfig.platformInterface != nil,
		len(options.Inbounds),
		strings.Join(inboundTypes, ","),
	))
}

func (s *CoreService) Parse(ctx context.Context, in *ParseRequest) (*ParseResponse, error) {
	return Parse(in)
}

func Parse(in *ParseRequest) (*ParseResponse, error) {
	defer config.DeferPanicToError("parse", func(err error) {
		Log(LogLevel_FATAL, LogType_CONFIG, err.Error())
		StopAndAlert(MessageType_UNEXPECTED_ERROR, err.Error())
	})

	content := in.Content
	if content == "" {
		path := in.TempPath
		if path == "" {
			path = in.ConfigPath
		}
		contentBytes, err := os.ReadFile(path)
		content = string(contentBytes)
		// os.Chdir(filepath.Dir(in.ConfigPath))
		if err != nil {
			return nil, err
		}

	}

	runtimeConfig := loadCoreRuntimeConfigSnapshot()
	config, err := config.ParseConfigContent(content, true, runtimeConfig.martenOptions, false)
	if err != nil {
		return &ParseResponse{
			ResponseCode: hcommon.ResponseCode_FAILED,
			Message:      err.Error(),
		}, err
	}
	if in.ConfigPath != "" {
		err = os.WriteFile(in.ConfigPath, config, 0o644)
		if err != nil {
			return &ParseResponse{
				ResponseCode: hcommon.ResponseCode_FAILED,
				Message:      err.Error(),
			}, err
		}
	}
	return &ParseResponse{
		ResponseCode: hcommon.ResponseCode_OK,
		Content:      string(config),
		Message:      "",
	}, err
}

func (s *CoreService) ChangeMartenSettings(ctx context.Context, in *ChangeMartenSettingsRequest) (*CoreInfoResponse, error) {
	return ChangeMartenSettings(in, true)
}

func ChangeMartenSettings(in *ChangeMartenSettingsRequest, insert bool) (*CoreInfoResponse, error) {
	return changeMartenSettings(in, insert, insert)
}

func restoreMartenSettings(
	in *ChangeMartenSettingsRequest,
	ready bool,
) (*CoreInfoResponse, error) {
	return changeMartenSettings(in, false, ready)
}

func changeMartenSettings(
	in *ChangeMartenSettingsRequest,
	insert bool,
	ready bool,
) (*CoreInfoResponse, error) {
	static.settingsChangeLock.Lock()
	defer static.settingsChangeLock.Unlock()

	nextOptions := config.DefaultMartenOptions()
	if in.MartenSettingsJson == "" {
		publishCoreRuntimeSettings(nextOptions, false)
		return &CoreInfoResponse{}, nil
	}
	err := json.Unmarshal([]byte(in.MartenSettingsJson), nextOptions)
	if err != nil {
		return nil, err
	}
	normalizeLegacyMartenSettings(nextOptions)
	if insert {
		settingsJSON, err := normalizeLegacyMartenSettingsJSON(in.MartenSettingsJson)
		if err != nil {
			return nil, err
		}
		settings := db.GetTable[hcommon.AppSettings]()
		if err := settings.UpdateInsert(&hcommon.AppSettings{
			Id:    "MartenSettingsJson",
			Value: settingsJSON,
		}); err != nil {
			return nil, fmt.Errorf("persist core settings: %w", err)
		}
	}
	// Publish the fully decoded snapshot only after validation and persistence
	// succeed. A failed update must not leave transient defaults visible to a
	// concurrent background start.
	publishCoreRuntimeSettings(nextOptions, ready)
	return &CoreInfoResponse{}, nil
}

func normalizeLegacyMartenSettings(options *config.MartenOptions) {
	if options.MTU == 9000 {
		options.MTU = 1280
	}
	if isLegacyRemoteDNSDefault(options.RemoteDnsAddress) {
		options.RemoteDnsAddress = "8.8.8.8"
	}
}

func normalizeLegacyMartenSettingsJSON(settingsJSON string) (string, error) {
	var settings map[string]any
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return "", err
	}
	changed := false
	if mtu, ok := settings["mtu"].(float64); ok && mtu == 9000 {
		settings["mtu"] = 1280
		changed = true
	}
	if remoteDNS, ok := settings["remote-dns-address"].(string); ok && isLegacyRemoteDNSDefault(remoteDNS) {
		settings["remote-dns-address"] = "8.8.8.8"
		changed = true
	}
	if !changed {
		return settingsJSON, nil
	}
	normalized, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func isLegacyRemoteDNSDefault(address string) bool {
	return address == "9.9.9.9" ||
		address == "tcp://8.8.8.8" ||
		address == "tcp://9.9.9.9"
}

func (s *CoreService) GenerateConfig(ctx context.Context, in *GenerateConfigRequest) (*GenerateConfigResponse, error) {
	return GenerateConfig(in)
}

func GenerateConfig(in *GenerateConfigRequest) (*GenerateConfigResponse, error) {
	defer config.DeferPanicToError("generateConfig", func(err error) {
		Log(LogLevel_FATAL, LogType_CONFIG, err.Error())
		StopAndAlert(MessageType_UNEXPECTED_ERROR, err.Error())
	})
	runtimeConfig := loadCoreRuntimeConfigSnapshot()
	martenOptions := runtimeConfig.martenOptions
	if martenOptions == nil {
		martenOptions = config.DefaultMartenOptions()
	}
	config, err := generateConfigFromFile(in.Path, *martenOptions)
	if err != nil {
		return nil, err
	}
	return &GenerateConfigResponse{
		ConfigContent: config,
	}, nil
}

func generateConfigFromFile(path string, configOpt config.MartenOptions) (string, error) {
	os.Chdir(filepath.Dir(path))
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	options, err := readOptions(string(content))
	if err != nil {
		return "", err
	}
	config, err := config.BuildConfigJson(configOpt, options)
	if err != nil {
		return "", err
	}
	return config, nil
}

func removeTunnelIfNeeded(options *option.Options) (tuninb *option.TunInboundOptions) {
	if hutils.TunAllowed() {
		return nil
	}

	// Create a new slice to hold the remaining inbounds
	newInbounds := make([]option.Inbound, 0, len(options.Inbounds))

	for _, inb := range options.Inbounds {
		if inb.Type == C.TypeTun {
			tuninb = &inb.TunOptions
		} else {
			newInbounds = append(newInbounds, inb)
		}
	}

	options.Inbounds = newInbounds
	return tuninb
}
