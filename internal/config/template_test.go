package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestDefaultConfigTOMLParses(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(DefaultConfigTOML, &cfg); err != nil {
		t.Fatalf("DefaultConfigTOML failed to parse: %v", err)
	}
}

func TestDefaultConfigTOMLCoversAllSections(t *testing.T) {
	// Reflect over Config's top-level fields; assert each toml tag
	// appears as a [section] header in the template. Catches a new
	// Config section being added without an init-template update.
	cfgType := reflect.TypeOf(Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" {
			continue
		}
		// Match either `[tag]` or `[tag.subsection]` openings.
		header := "[" + tag + "]"
		if !strings.Contains(DefaultConfigTOML, header) {
			t.Errorf("DefaultConfigTOML missing section header %q for Config field %s", header, field.Name)
		}
	}
}
