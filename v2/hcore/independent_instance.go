package hcore

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Cr0me1ve/marten-core/v2/config"
	"golang.org/x/net/proxy"

	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
)

func getRandomAvailblePort() uint16 {
	// TODO: implement it
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	return uint16(listener.Addr().(*net.TCPAddr).Port)
}

func RunInstanceString(martenSettings *config.MartenOptions, proxiesInput string) (*MartenService, error) {
	if martenSettings == nil {
		martenSettings = config.DefaultMartenOptions()
	}
	singconfigs, err := config.ParseConfigContentToOptions(proxiesInput, true, martenSettings, false)
	if err != nil {
		return nil, err
	}
	return RunInstance(martenSettings, singconfigs)
}

func RunInstance(martenSettings *config.MartenOptions, singconfig *option.Options) (*MartenService, error) {
	if martenSettings == nil {
		martenSettings = config.DefaultMartenOptions()
	}
	martenSettings.EnableClashApi = false
	martenSettings.InboundOptions.MixedPort = getRandomAvailblePort()
	martenSettings.InboundOptions.EnableTun = false
	martenSettings.InboundOptions.EnableTunService = false
	martenSettings.InboundOptions.SetSystemProxy = false
	martenSettings.InboundOptions.TProxyPort = 0
	martenSettings.InboundOptions.LocalDnsPort = 0
	martenSettings.Region = "other"
	martenSettings.BlockAds = false
	martenSettings.LogFile = "/dev/null"

	finalConfigs, err := config.BuildConfig(*martenSettings, *singconfig)
	if err != nil {
		return nil, err
	}

	instance, err := NewService(*finalConfigs)
	if err != nil {
		return nil, err
	}
	err = instance.Start()
	if err != nil {
		return nil, err
	}
	<-time.After(250 * time.Millisecond)
	hservice := &MartenService{libbox: instance, ListenPort: martenSettings.InboundOptions.MixedPort}
	hservice.PingConnectivity()
	return hservice, nil
}

type MartenService struct {
	libbox     *libbox.BoxService
	ListenPort uint16
}

// dialer, err := s.libbox.GetInstance().Router().Dialer(context.Background())

func (s *MartenService) Close() error {
	return s.libbox.Close()
}

func (s *MartenService) GetContent(url string) (string, error) {
	return s.ContentFromURL("GET", url, 10*time.Second)
}

func (s *MartenService) ContentFromURL(method string, url string, timeout time.Duration) (string, error) {
	if method == "" {
		return "", fmt.Errorf("empty method")
	}
	if url == "" {
		return "", fmt.Errorf("empty url")
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return "", err
	}

	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", s.ListenPort), nil, proxy.Direct)
	if err != nil {
		return "", err
	}

	transport := &http.Transport{
		Dial: dialer.Dial,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("request failed with status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if body == nil {
		return "", fmt.Errorf("empty body")
	}

	return string(body), nil
}

func (s *MartenService) PingConnectivity() (time.Duration, error) {
	return s.Ping("http://example.com")
}

// func (s *MartenService) RawConnection(ctx context.Context, url string) (net.Conn, error) {
// 	return
// }

func (s *MartenService) PingAverage(url string, count int) (time.Duration, error) {
	if count <= 0 {
		return -1, fmt.Errorf("count must be greater than 0")
	}

	var sum int
	real_count := 0
	for i := 0; i < count; i++ {
		delay, err := s.Ping(url)
		if err == nil {
			real_count++
			sum += int(delay.Milliseconds())
		} else if real_count == 0 && i > count/2 {
			return -1, fmt.Errorf("ping average failed")
		}

	}
	return time.Duration(sum / real_count * int(time.Millisecond)), nil
}

func (s *MartenService) Ping(url string) (time.Duration, error) {
	startTime := time.Now()
	_, err := s.ContentFromURL("HEAD", url, 4*time.Second)
	if err != nil {
		return -1, err
	}
	duration := time.Since(startTime)
	return duration, nil
}
