package config

import (
	"fmt"

	"github.com/go-viper/mapstructure/v2"
)

// DecodeParams decodes the params of an adapter into out, strictly: a key out
// does not know is an error. Durations are written as "30s", "10m".
func DecodeParams(params map[string]any, out any) error {
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		TagName:     "yaml",
		ErrorUnused: true,
		Result:      out,
		DecodeHook:  mapstructure.StringToTimeDurationHookFunc(),
	})
	if err != nil {
		return fmt.Errorf("ERR721: %w", err)
	}
	if err := dec.Decode(params); err != nil {
		return fmt.Errorf("ERR722: invalid params: %w", err)
	}
	return nil
}
