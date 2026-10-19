// Package capcut is a Go port of the capcut_tts_api Python SDK
// (github.com/K07VN/capcut-tts-api): request signing, TTS/STT task APIs,
// VOD media upload and the voice catalog.
package capcut

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	BaseURL    = "https://editor-api-sg.capcutapi.com"
	VODRegion  = "sdwdmwlll"
	VODService = "vod"

	DefaultVoice      = "BV074_streaming"
	DefaultResourceID = "7102355709945188865"
	userAgent         = "Cronet/TTNetVersion:1d7cc3b1 2025-07-16 QuicVersion:52c2b40d 2025-04-03"
)

const TTSSignPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAmTd34Lw4b7IuldSXh/zY
CMla+ITdGG5TeWz6ad+OySd4r+IrY45AoqrYUxhQ2dl+7z+i7r/5vEa8rr39BYfB
8AGMQLmZA8HmgpWBsqrn/V6daUALkKnkLb70Fn32CJigIuGXAYqxUdGuI340aC+0
v5Es3puJsHyzf01/AelE4Cdc6bZhQrASJLBh8R3BQToYClmDVSDUQk28o8sl/guA
Z4n303Vj+6Siv1HayPCdV6kpVVnMBAG4+umUbwGmn132N3fgpzLarFF3XyWmS1zh
D/J07iM/rP8GDO9IskHNHd2phrO0G6KzrcFAnTBHjVv+hCBEfzN/no3FNA9AuC36
mwIDAQAB
-----END PUBLIC KEY-----`

// DeviceConfig is the device identity used for request signing.
type DeviceConfig struct {
	Aid            string `json:"aid"`
	AppName        string `json:"app_name"`
	Appvr          string `json:"appvr"`
	VersionName    string `json:"version_name"`
	VersionCode    string `json:"version_code"`
	Channel        string `json:"channel"`
	DevicePlatform string `json:"device_platform"`
	DeviceType     string `json:"device_type"`
	DeviceBrand    string `json:"device_brand"`
	OSVersion      string `json:"os_version"`
	DeviceID       string `json:"device_id"`
	IID            string `json:"iid"`
	Region         string `json:"region"`
	Loc            string `json:"loc"`
	Lan            string `json:"lan"`
	Pf             string `json:"pf"`
	Tdid           string `json:"tdid"`
}

// DefaultDevice mirrors config.DEFAULT_DEVICE.
func DefaultDevice() DeviceConfig {
	return DeviceConfig{
		Aid: "359289", AppName: "CapCut", Appvr: "8.7.0", VersionName: "8.7.0", VersionCode: "8.7.0",
		Channel: "capcutpc_google", DevicePlatform: "mac", DeviceType: "MacBookPro17,4",
		DeviceBrand: "MacBookPro17,4", OSVersion: "15.7.4", DeviceID: "76471456455646328721",
		IID: "76471456455646328721", Region: "VN", Loc: "VN", Lan: "vi-VN", Pf: "3",
		Tdid: "76471456455646328721",
	}
}

// DeviceFromJSON merges a (partial) JSON object over the defaults, like DeviceConfig.from_dict.
func DeviceFromJSON(data []byte) (DeviceConfig, error) {
	d := DefaultDevice()
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("device json: %w", err)
	}
	return d, nil
}

// DeviceFromEnv loads CAPCUT_DEVICE_JSON (inline JSON) or CAPCUT_DEVICE_FILE (path), else defaults.
func DeviceFromEnv() (DeviceConfig, string, error) {
	if s := strings.TrimSpace(os.Getenv("CAPCUT_DEVICE_JSON")); s != "" {
		d, err := DeviceFromJSON([]byte(s))
		return d, "env:CAPCUT_DEVICE_JSON", err
	}
	if p := strings.TrimSpace(os.Getenv("CAPCUT_DEVICE_FILE")); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return DefaultDevice(), p, err
		}
		d, err := DeviceFromJSON(b)
		return d, p, err
	}
	return DefaultDevice(), "default", nil
}
