package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type ColorConfig struct {
	Black     string `toml:"black"`
	DarkGray  string `toml:"dark_gray"`
	LightGray string `toml:"light_gray"`
	White     string `toml:"white"`
}

type MarkConfig struct {
	ColorConfig
	MarkerOpacity float64 `toml:"marker_opacity"`
}

type NoteConfig struct {
	ColorConfig
}

type WatchConfig struct {
	SupernotePrivateCloud string `toml:"supernote_private_cloud"`
	WebDAV                string `toml:"webdav"`
	Location              string `toml:"location"`
	PollInterval          int    `toml:"poll_interval"` // seconds, 0 = default (5s)
}

func (w WatchConfig) PollDuration() time.Duration {
	if w.PollInterval > 0 {
		return time.Duration(w.PollInterval) * time.Second
	}
	return 5 * time.Second
}

func (w WatchConfig) InputDirs() []string {
	var dirs []string
	if w.SupernotePrivateCloud != "" {
		dirs = append(dirs, w.SupernotePrivateCloud)
	}
	if w.WebDAV != "" {
		dirs = append(dirs, w.WebDAV)
	}
	return dirs
}

type Config struct {
	Mark  MarkConfig  `toml:"mark"`
	Note  NoteConfig  `toml:"note"`
	Watch WatchConfig `toml:"watch"`
}

func defaultConfig() *Config {
	return &Config{
		Mark: MarkConfig{
			ColorConfig: ColorConfig{
				Black:     "#000000",
				DarkGray:  "#9D9D9D",
				LightGray: "#C9C9C9",
				White:     "#FFFFFF",
			},
			MarkerOpacity: 0.38,
		},
		Note: NoteConfig{
			ColorConfig: ColorConfig{
				Black:     "#000000",
				DarkGray:  "#9D9D9D",
				LightGray: "#C9C9C9",
				White:     "#FFFFFF",
			},
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return cfg, nil
}

func parseHexColor(hex string) (r, g, b uint8, err error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, 0, fmt.Errorf("invalid hex color: #%s (expected 6 hex digits)", hex)
	}
	var rgb [3]uint8
	for i := range 3 {
		val, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid hex color: #%s: %w", hex, err)
		}
		rgb[i] = uint8(val)
	}
	return rgb[0], rgb[1], rgb[2], nil
}
