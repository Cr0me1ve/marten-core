package sdk

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"runtime"
	"strings"

	"github.com/Cr0me1ve/marten-core/v2/config"
	hcore "github.com/Cr0me1ve/marten-core/v2/hcore"
	"github.com/sagernet/sing-box/option"
)

func RunInstance(martenSettings *config.MartenOptions, singconfig *option.Options) (*hcore.MartenService, error) {
	return hcore.RunInstance(martenSettings, singconfig)
}

func ParseConfig(martenSettings *config.MartenOptions, configStr string) (*option.Options, error) {
	if martenSettings == nil {
		martenSettings = config.DefaultMartenOptions()
	}
	if strings.HasPrefix(configStr, "http://") || strings.HasPrefix(configStr, "https://") {
		client := &http.Client{}
		configPath := strings.Split(configStr, "\n")[0]
		// Create a new request
		req, err := http.NewRequest("GET", configPath, nil)
		if err != nil {
			fmt.Println("Error creating request:", err)
			return nil, err
		}
		req.Header.Set("User-Agent", "Marten/1.0.0 ("+runtime.GOOS+") like ClashMeta v2ray sing-box")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Println("Error making GET request:", err)
			return nil, err
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read config body: %w", err)
		}
		configStr = string(body)
	}
	return config.ParseConfigContentToOptions(configStr, true, martenSettings, false)
}
