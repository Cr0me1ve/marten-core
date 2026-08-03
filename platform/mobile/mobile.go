package mobile

import (
	"encoding/json"

	hcore "github.com/Cr0me1ve/marten-core/v2/hcore"

	_ "github.com/sagernet/gomobile"

	"github.com/sagernet/sing-box/experimental/libbox"
)

// These values are populated by the release build. Defaults make local and
// incorrectly configured builds explicit instead of silently claiming a VCS
// revision discovered from a parent worktree.
var (
	CoreRevision     = "unknown"
	SingBoxRevision  = "unknown"
	TurncoatRevision = "unknown"
	SourceDirty      = "unknown"
)

type buildProvenance struct {
	CoreRevision     string `json:"core_revision"`
	SingBoxRevision  string `json:"sing_box_revision"`
	TurncoatRevision string `json:"turncoat_revision"`
	SourceDirty      string `json:"source_dirty"`
}

// BuildProvenance is exported through gomobile so the Android app and release
// checks can inspect the revisions that were actually compiled into the AAR.
func BuildProvenance() string {
	payload, err := json.Marshal(buildProvenance{
		CoreRevision:     CoreRevision,
		SingBoxRevision:  SingBoxRevision,
		TurncoatRevision: TurncoatRevision,
		SourceDirty:      SourceDirty,
	})
	if err != nil {
		return `{"core_revision":"unknown","sing_box_revision":"unknown","turncoat_revision":"unknown","source_dirty":"unknown"}`
	}
	return string(payload)
}

func Setup(baseDir string, workingDir string, tempDir string, mode int, listen string, secret string, debug bool, platformInterface libbox.PlatformInterface) error {
	return hcore.Setup(&hcore.SetupRequest{
		BasePath:          baseDir,
		WorkingDir:        workingDir,
		TempDir:           tempDir,
		FlutterStatusPort: 0,
		Listen:            listen,
		Debug:             debug,
		Mode:              hcore.SetupMode(mode),
		Secret:            secret,
	}, platformInterface)

	// return hcore.Start(17078)
}

// SetDebug refreshes diagnostic mode even when the long-lived gRPC setup is
// already initialized. The next Start will construct sing-box with DEBUG logs
// enabled before any outbound (including ICMP) is created.
func SetDebug(debug bool) {
	hcore.SetRuntimeDebug(debug)
}

func RuntimeState() string {
	return hcore.RuntimeState()
}

func BuildConfig(configPath string) (string, error) {
	return hcore.BuildConfigJson(&hcore.StartRequest{
		ConfigPath: configPath,
	})
}

// func Start(configPath string, configContent string, platformInterface libbox.PlatformInterface) (*hcore.CoreInfoResponse, error) {
// 	state, err := hcore.StartWithPlatformInterface(&hcore.StartRequest{
// 		ConfigContent: configContent,
// 		ConfigPath:    configPath,
// 	}, platformInterface)
// 	return state, err
// }

func Start(configPath string, configContent string) error {
	_, err := hcore.StartService(&hcore.StartRequest{
		ConfigPath:    configPath,
		ConfigContent: configContent,
	})
	return err
}

func Stop() error {
	_, err := hcore.Stop()
	return err
}

// ResetNetwork refreshes network-bound outbounds while preserving the running
// core service and TUN interface.
func ResetNetwork() error {
	return hcore.ResetNetwork()
}

func GetServerPublicKey() []byte {
	return hcore.GetGrpcServerPublicKey()
}

func AddGrpcClientPublicKey(clientPublicKey []byte) error {
	return hcore.AddGrpcClientPublicKey(clientPublicKey)
}

func Close(mode int) error {
	return hcore.Close(hcore.SetupMode(mode))
}

func Test() string {
	return "Hello from mobile"
}
