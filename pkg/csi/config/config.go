/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package config

import (
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

const (
	//Config var names
	OpenNebulaRPCEndpointVar = "ONE_XMLRPC"
	OpenNebulaCredentialsVar = "ONE_AUTH"

	//Default values
	defaultOpenNebulaRPCEndpoint = "http://localhost:2633/RPC2"
)

// CSIPluginConfig holds the configuration for the CSI plugin
// TODO: implement thread safety
type CSIPluginConfig struct {
	viper *viper.Viper
}

func LoadConfiguration() CSIPluginConfig {
	return CSIPluginConfig{
		viper: initViper(),
	}
}

func initViper() *viper.Viper {
	viper := viper.New()

	//TODO: Bind to flags
	viper.SetConfigName("opennebula-csi-config")
	viper.SetConfigType("yaml")
	//default config file locations
	viper.AddConfigPath(".")
	viper.AddConfigPath("$HOME/.csi-driver-opennebula/")
	viper.AddConfigPath("/etc/csi-driver-opennebula/")
	if err := viper.ReadInConfig(); err == nil {
		klog.Infof("Using config file: %s", viper.ConfigFileUsed())
	}
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		klog.Warningf("Config file changed: %s", e.Name)
	})

	viper.SetDefault(OpenNebulaRPCEndpointVar, defaultOpenNebulaRPCEndpoint)

	viper.AutomaticEnv()
	viper.SetTypeByDefaultValue(true)

	return viper
}

// Following methods are for abstracting the viper library
// and provide a cleaner interface for getting configuration values.

func (c *CSIPluginConfig) GetString(key string) (string, bool) {
	value := c.viper.GetString(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetBool(key string) (bool, bool) {
	value := c.viper.GetBool(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetInt(key string) (int, bool) {
	value := c.viper.GetInt(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetInt32(key string) (int32, bool) {
	value := c.viper.GetInt32(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) OverrideVal(key string, value any) {
	c.viper.Set(key, value)
}
