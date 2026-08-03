package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/reddts/ray2sing/ray2sing"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

var ray2singShareSchemes = map[string]struct{}{
	"vmess": {}, "trojan": {}, "svmess": {}, "strojan": {},
	"ss": {}, "tuic": {}, "hysteria": {}, "hysteria2": {}, "hy2": {},
	"ssh": {}, "wg": {}, "wireguard": {}, "ssconf": {}, "ssconf+http": {},
	"warp": {}, "direct": {}, "socks": {}, "phttp": {}, "phttps": {},
	"http": {}, "https": {}, "xvmess": {}, "xvless": {}, "xtrojan": {},
	"xdirect": {},
}

func shareLinks2SingNoAutoVLESS(input string) (string, bool, error) {
	input = decodeShareBase64IfNeeded(input)
	lines := strings.Split(strings.ReplaceAll(input, "\r", "\n"), "\n")

	var outbounds []option.Outbound
	counter := 0
	handled := false
	sawMartenVLESS := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 5 || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/") {
			continue
		}
		chains := strings.Split(line, "&&detour=")
		detourTag := ""
		for _, rawChain := range chains {
			chain := strings.TrimSpace(decodeShareBase64IfNeeded(rawChain))
			scheme := shareScheme(chain)
			if scheme == "" {
				if sawMartenVLESS {
					return "", true, fmt.Errorf("[VLESSParser] unsupported chained entry: %s", rawChain)
				}
				return "", false, nil
			}

			var outbound *option.Outbound
			var err error
			switch scheme {
			case "vless", "svless":
				outbound, err = parseVLESSShareLink(chain)
				sawMartenVLESS = true
			default:
				if _, ok := ray2singShareSchemes[scheme]; !ok {
					if sawMartenVLESS {
						return "", true, fmt.Errorf("[VLESSParser] unsupported URI scheme %q", scheme)
					}
					return "", false, nil
				}
				outbound, err = parseNonVLESSShareLink(chain)
			}
			if err != nil {
				return "", true, fmt.Errorf("[VLESSParser] %s: %w", chain, err)
			}
			if outbound == nil {
				return "", true, fmt.Errorf("[VLESSParser] %s: parser returned no outbound", chain)
			}
			if outbound.Tag == "" {
				outbound.Tag = outbound.Type
			}
			outbound.Tag += " § " + strconv.Itoa(counter)
			if dialer := outboundDialerOptions(outbound); dialer != nil {
				dialer.Detour = detourTag
			}
			detourTag = outbound.Tag
			outbounds = append(outbounds, *outbound)
			counter++
			handled = true
		}
	}

	if !handled || !sawMartenVLESS {
		return "", false, nil
	}
	if len(outbounds) == 0 {
		return "", true, fmt.Errorf("[VLESSParser] no outbounds found")
	}
	fullConfig := option.Options{Outbounds: outbounds}
	encoded, err := json.MarshalIndent(fullConfig, "", "  ")
	if err != nil {
		return "", true, err
	}
	return string(encoded), true, nil
}

func parseNonVLESSShareLink(link string) (*option.Outbound, error) {
	converted, err := ray2sing.Ray2Singbox(link, false)
	if err != nil {
		return nil, err
	}
	var options option.Options
	if err := json.Unmarshal([]byte(converted), &options); err != nil {
		return nil, err
	}
	if len(options.Outbounds) != 1 {
		return nil, fmt.Errorf("expected one outbound, got %d", len(options.Outbounds))
	}
	return &options.Outbounds[0], nil
}

func parseVLESSShareLink(link string) (*option.Outbound, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	port := uint16(443)
	if parsed.Port() != "" {
		port64, err := strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %w", err)
		}
		port = uint16(port64)
	}
	params := normalizedQuery(parsed)
	tlsOptions, err := vlessShareTLSOptions(params)
	if err != nil {
		return nil, err
	}
	transportOptions, err := vlessShareTransportOptions(params)
	if err != nil {
		return nil, err
	}
	packetEncoding := params["packetencoding"]

	outbound := &option.Outbound{
		Type: C.TypeVLESS,
		Tag:  parsed.Fragment,
		VLESSOptions: option.VLESSOutboundOptions{
			ServerOptions: option.ServerOptions{
				Server:     parsed.Hostname(),
				ServerPort: port,
			},
			UUID:                        parsed.User.Username(),
			Flow:                        params["flow"],
			OutboundTLSOptionsContainer: tlsOptions,
			Transport:                   transportOptions,
		},
	}
	if packetEncoding != "" {
		outbound.VLESSOptions.PacketEncoding = &packetEncoding
	}
	return outbound, nil
}

func vlessShareTLSOptions(params map[string]string) (option.OutboundTLSOptionsContainer, error) {
	security := params["security"]
	if security == "" {
		security = params["tls"]
	}
	if security != "tls" && security != "reality" {
		return option.OutboundTLSOptionsContainer{}, nil
	}
	tlsOptions := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: params["sni"],
		Insecure:   isTruthy(params["insecure"]) || isTruthy(params["allowinsecure"]),
	}
	if alpn := params["alpn"]; alpn != "" {
		tlsOptions.ALPN = splitNonEmpty(alpn, ",")
	}
	if fp := params["fp"]; fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{
			Enabled:     true,
			Fingerprint: fp,
		}
	}
	if security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{
			Enabled:   true,
			PublicKey: params["pbk"],
			ShortID:   params["sid"],
			SpiderX:   params["spx"],
		}
	}
	return option.OutboundTLSOptionsContainer{TLS: tlsOptions}, nil
}

func vlessShareTransportOptions(params map[string]string) (*option.V2RayTransportOptions, error) {
	network := params["net"]
	if network == "" {
		network = params["type"]
	}
	if network == "" || network == "raw" || network == "tcp" {
		return nil, nil
	}
	path := params["path"]
	if path == "" {
		path = params["servicename"]
	}
	host := params["host"]
	transport := &option.V2RayTransportOptions{}

	switch network {
	case "http":
		transport.Type = C.V2RayTransportTypeHTTP
		transport.HTTPOptions.Path = path
		if host != "" {
			transport.HTTPOptions.Host = splitNonEmpty(host, ",")
		}
	case "h2":
		transport.Type = C.V2RayTransportTypeHTTP2
		transport.HTTPOptions.Path = path
		if host != "" {
			transport.HTTPOptions.Host = splitNonEmpty(host, ",")
		}
	case "httpupgrade":
		transport.Type = C.V2RayTransportTypeHTTPUpgrade
		transport.HTTPUpgradeOptions.Host = host
		transport.HTTPUpgradeOptions.Path = path
	case "ws":
		transport.Type = C.V2RayTransportTypeWebsocket
		transport.WebsocketOptions.Path = websocketPathAndEarlyData(path, &transport.WebsocketOptions)
		if host != "" {
			transport.WebsocketOptions.Headers = option.HTTPHeader{"Host": option.Listable[string]{host}}
		}
	case "grpc":
		transport.Type = C.V2RayTransportTypeGRPC
		transport.GRPCOptions.ServiceName = path
		transport.GRPCOptions.Authority = params["authority"]
		mode := strings.ToLower(params["mode"])
		transport.GRPCOptions.MultiMode = mode == "multi" || mode == "multi-mode" || mode == "multi_mode"
	case "quic":
		transport.Type = C.V2RayTransportTypeQUIC
		transport.QUICOptions.Security = params["quicsecurity"]
		transport.QUICOptions.Key = params["key"]
		transport.QUICOptions.Header.Type = firstNonEmpty(params["headertype"], params["header"])
	case "xhttp", "splithttp":
		if network == "xhttp" {
			transport.Type = C.V2RayTransportTypeXHTTP
		} else {
			transport.Type = C.V2RayTransportTypeSplitHTTP
		}
		transport.XHTTPOptions.Host = host
		transport.XHTTPOptions.Path = path
		transport.XHTTPOptions.Mode = params["mode"]
		if extra := params["extra"]; extra != "" {
			var extraConfig map[string]any
			if err := json.Unmarshal([]byte(extra), &extraConfig); err != nil {
				return nil, fmt.Errorf("invalid xhttp extra: %w", err)
			}
			transport.XHTTPOptions.Extra = extraConfig
		}
	default:
		return nil, fmt.Errorf("unknown VLESS transport type: %s", network)
	}
	return transport, nil
}

func websocketPathAndEarlyData(path string, options *option.V2RayWebsocketOptions) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	pathURL, err := url.Parse(path)
	if err != nil {
		return path
	}
	query := pathURL.Query()
	if maxEarlyData := query.Get("ed"); maxEarlyData != "" {
		if parsed, err := strconv.ParseUint(maxEarlyData, 10, 32); err == nil {
			options.MaxEarlyData = uint32(parsed)
			options.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			query.Del("ed")
			pathURL.RawQuery = query.Encode()
		}
	}
	return pathURL.String()
}

func outboundDialerOptions(outbound *option.Outbound) *option.DialerOptions {
	switch outbound.Type {
	case C.TypeDirect:
		return &outbound.DirectOptions.DialerOptions
	case C.TypeShadowsocks:
		return &outbound.ShadowsocksOptions.DialerOptions
	case C.TypeVMess:
		return &outbound.VMessOptions.DialerOptions
	case C.TypeTrojan:
		return &outbound.TrojanOptions.DialerOptions
	case C.TypeWireGuard, C.TypeAmneziaWG:
		return &outbound.WireGuardOptions.DialerOptions
	case C.TypeHysteria:
		return &outbound.HysteriaOptions.DialerOptions
	case C.TypeVLESS:
		return &outbound.VLESSOptions.DialerOptions
	case C.TypeTUIC:
		return &outbound.TUICOptions.DialerOptions
	case C.TypeHysteria2:
		return &outbound.Hysteria2Options.DialerOptions
	case C.TypeSSH:
		return &outbound.SSHOptions.DialerOptions
	case C.TypeXray:
		return &outbound.XrayOptions.DialerOptions
	default:
		return nil
	}
}

func normalizedQuery(parsed *url.URL) map[string]string {
	params := make(map[string]string)
	for key, values := range parsed.Query() {
		normalized := strings.ReplaceAll(strings.ToLower(key), "_", "")
		params[normalized] = strings.Join(values, ",")
	}
	return params
}

func shareScheme(link string) string {
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme)
}

func decodeShareBase64IfNeeded(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		candidate := value
		if encoding == base64.StdEncoding || encoding == base64.URLEncoding {
			if padding := len(candidate) % 4; padding != 0 {
				candidate += strings.Repeat("=", 4-padding)
			}
		}
		decoded, err := encoding.DecodeString(candidate)
		if err == nil && strings.Contains(string(decoded), "://") {
			return string(decoded)
		}
	}
	return value
}

func splitNonEmpty(value string, sep string) []string {
	parts := strings.Split(value, sep)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
