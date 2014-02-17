package main

import (
	"encoding/json"
	"io/ioutil"
	"time"

	"code.google.com/p/goauth2/oauth"
)

type ScanServerConfig struct {
	ClientId               string
	ClientSecret           string
	OAuthToken             oauth.Token
	RemoteParentFolderId   string
	LocalScanDir           string
	LastProccessedScanTime time.Time
	TmpDir                 string
	DuplexPrefix           string
}

func ParseConfig(config_file string) ScanServerConfig {
	text_bytes, err := ioutil.ReadFile(config_file)
	if err != nil {
		panic(err)
	}

	var config ScanServerConfig
	// We assume that an empty file is a new config.
	if len(text_bytes) != 0 {
		err = json.Unmarshal(text_bytes, &config)
		if err != nil {
			panic(err)
		}
	}

	return config
}

func WriteConfig(config_file string, config ScanServerConfig) {
	text_bytes, err := json.Marshal(config)
	if err != nil {
		panic(err)
	}
	ioutil.WriteFile(config_file, text_bytes, 0644)
}
