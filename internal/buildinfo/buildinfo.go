// Package buildinfo exposes immutable release metadata without opening a profile
// or starting the desktop runtime. Values are set with Go linker flags.
package buildinfo

import (
	"encoding/json"
	"runtime"
)

var (
	Version         = "dev"
	Commit          = "unknown"
	SourceDateEpoch = "0"
	Unsigned        = "true"
)

type Metadata struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	SourceDateEpoch string `json:"sourceDateEpoch"`
	Unsigned        bool   `json:"unsigned"`
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
	GoVersion       string `json:"goVersion"`
}

// Details is the stable desktop bridge DTO; Metadata remains an alias for callers.
type Details = Metadata

func Info() Metadata {
	return Metadata{Version, Commit, SourceDateEpoch, Unsigned != "false", runtime.GOOS, runtime.GOARCH, runtime.Version()}
}

func JSON() string {
	data, _ := json.Marshal(Info())
	return string(data)
}
