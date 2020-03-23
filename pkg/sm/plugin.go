package sm

import (
	"fmt"
	"plugin"

	"github.com/Mellanox/ib-kubernetes/pkg/sm/plugins"

	"github.com/golang/glog"
)

const InitializePluginFunc = "Initialize"

// PluginInitialize is function type to Initizalize the sm plugin. It returns sm plugin instance.
type PluginInitialize func() (plugins.SubnetManagerClient, error)

type PluginLoader interface {
	// LoadPlugin loads go plugin from given path with given symbolName which is the variable needed to be extracted.
	LoadPlugin(path, symbolName string) (PluginInitialize, error)
}

type pluginLoader struct {
}

func NewPluginLoader() PluginLoader {
	return &pluginLoader{}
}

func (p *pluginLoader) LoadPlugin(path, symbolName string) (PluginInitialize, error) {
	glog.V(3).Infof("LoadPlugin(): path %s, symbolName %s", path, symbolName)
	smPlugin, err := plugin.Open(path)
	if err != nil {
		err = fmt.Errorf("LoadPlugin(): failed to load plugin: %v", err)
		return nil, err
	}

	symbol, err := smPlugin.Lookup(symbolName)
	if err != nil {
		err = fmt.Errorf("LoadPlugin(): failed to find \"%s\" object in the plugin file: %v", symbolName, err)
		return nil, err
	}

	pluginInitializer, ok := symbol.(func() (plugins.SubnetManagerClient, error))
	if !ok {
		err = fmt.Errorf("LoadPlugin(): \"%s\" object is not of type function", symbolName)
		return nil, err
	}
	return pluginInitializer, nil
}
