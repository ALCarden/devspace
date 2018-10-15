package configutil

import (
	"os"
	"sync"
	"unsafe"

	"github.com/covexo/devspace/pkg/util/log"

	"github.com/covexo/devspace/pkg/devspace/config/v1"
)

//ConfigInterface defines the pattern of every config
type ConfigInterface interface{}

const configGitignore = `logs/
overwrite.yaml
`

// ConfigPath is the path for the main config
const ConfigPath = "/.devspace/config.yaml"

// OverwriteConfigPath specifies where the override.yaml lies
const OverwriteConfigPath = "/.devspace/overwrite.yaml"

// Global config vars
var config *v1.Config
var configRaw *v1.Config
var overwriteConfig *v1.Config
var overwriteConfigRaw *v1.Config

// Thread-safety helper
var getConfigOnce sync.Once

// ConfigExists checks whether the yaml file for the config exists
func ConfigExists() (bool, error) {
	workdir, _ := os.Getwd()

	_, err := os.Stat(workdir + ConfigPath)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// InitConfig initializes the config objects
func InitConfig() *v1.Config {
	getConfigOnce.Do(func() {
		config = makeConfig()
		configRaw = makeConfig()
		overwriteConfig = makeConfig()
		overwriteConfigRaw = makeConfig()
	})

	return config
}

// GetConfig returns the config merged from .devspace/config.yaml and .devspace/overwrite.yaml
func GetConfig() *v1.Config {
	getConfigOnce.Do(func() {
		config = makeConfig()
		configRaw = makeConfig()
		overwriteConfig = makeConfig()
		overwriteConfigRaw = makeConfig()

		err := loadConfig(configRaw, ConfigPath)
		if err != nil {
			log.Errorf("Loading config: %v", err)
			log.Fatal("Please run `devspace init -r` to repair your config")
		}

		//ignore error as overwrite.yaml is optional
		loadConfig(overwriteConfigRaw, OverwriteConfigPath)

		merge(config, configRaw, unsafe.Pointer(&config), unsafe.Pointer(configRaw))
		merge(overwriteConfig, overwriteConfigRaw, unsafe.Pointer(&overwriteConfig), unsafe.Pointer(overwriteConfigRaw))
		merge(config, overwriteConfig, unsafe.Pointer(&config), unsafe.Pointer(overwriteConfig))

		if config.DevSpace.Release != nil && config.DevSpace.Release.Namespace == nil {
			config.DevSpace.Release.Namespace = String("default")
		}
	})

	return config
}

// GetOverwriteConfig returns the config retrieved from .devspace/overwrite.yaml
func GetOverwriteConfig() *v1.Config {
	GetConfig()

	return overwriteConfig
}
