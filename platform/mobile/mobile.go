package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"

	coreconfig "github.com/Cr0me1ve/marten-core/v2/config"
	hcore "github.com/Cr0me1ve/marten-core/v2/hcore"

	_ "github.com/sagernet/gomobile"

	"github.com/sagernet/sing-box/experimental/libbox"
)

// EndpointResolver is implemented by Android with a resolver bound to the
// current physical Network. Resolve returns newline-separated literal IPs and
// must not expose or log the private endpoint hostname.
type EndpointResolver interface {
	Resolve(request *EndpointResolveContext, domain string) string
}

// EndpointResolveContext carries cancellation across the gomobile boundary.
// The Android resolver must also bound non-interruptible legacy OS lookups.
type EndpointResolveContext struct {
	ctx  context.Context
	mu   sync.Mutex
	stop func() bool
}

func (r *EndpointResolveContext) OnCancel(callback libbox.Func) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
	}
	r.stop = context.AfterFunc(r.ctx, func() { _ = callback.Invoke() })
}

func (r *EndpointResolveContext) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
}

// SetEndpointResolver installs Android's physical-network resolver for config
// bootstrap. A nil resolver restores the default behavior for other clients.
func SetEndpointResolver(resolver EndpointResolver) {
	if resolver == nil {
		coreconfig.SetPlatformEndpointResolver(nil)
		return
	}
	coreconfig.SetPlatformEndpointResolver(func(ctx context.Context, domain string) ([]netip.Addr, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request := &EndpointResolveContext{ctx: ctx}
		defer request.close()
		encoded := resolver.Resolve(request, domain)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if encoded == "" {
			return nil, errors.New("physical endpoint resolution unavailable")
		}
		addresses := make([]netip.Addr, 0)
		for _, token := range strings.Fields(encoded) {
			address, err := netip.ParseAddr(strings.Trim(token, "[]"))
			if err != nil {
				continue
			}
			addresses = append(addresses, address)
		}
		if len(addresses) == 0 {
			return nil, errors.New("physical endpoint resolution returned no usable address")
		}
		return addresses, nil
	})
}

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

// CanonicalizeSubscription parses a subscription with the exact option schema
// bundled into this mobile library without starting a core, mutating runtime
// settings, or writing persistent state. It is safe for foreground and
// headless refresh engines to use as the single admission boundary before an
// encrypted profile transaction is committed.
func CanonicalizeSubscription(content string, settingsJSON string) (string, error) {
	options := coreconfig.DefaultMartenOptions()
	if settingsJSON != "" {
		if err := json.Unmarshal([]byte(settingsJSON), options); err != nil {
			return "", err
		}
	}
	// Remote subscription admission is always compatibility mode. Explicit
	// full-config execution is a separate user workflow and must not cause one
	// future or malformed subscription entry to reject compatible siblings.
	options.EnableFullConfig = false
	canonical, err := coreconfig.ParseConfigContent(content, false, options, false)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
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

// CancelStart releases a Mobile.start call that is waiting for fixed local
// listeners. The subsequent Mobile.close remains responsible for exact native
// teardown.
func CancelStart() bool {
	return hcore.CancelStart()
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

// ResetNetworkForRouteRecovery performs an in-place transport recovery after
// the Android VPN data-plane proof has failed. The core service and TUN remain
// alive; TURNcoat receives a forced physical-carrier retirement instead of a
// relay-only probe-and-keep decision.
func ResetNetworkForRouteRecovery() error {
	return hcore.ResetNetworkForRouteRecovery()
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
