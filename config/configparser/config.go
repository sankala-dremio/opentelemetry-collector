// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package config implements loading of configuration from Viper configuration.
// The implementation relies on registered factories that allow creating
// default configuration for each type of receiver/exporter/processor.
package configparser

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cast"
	"github.com/spf13/viper"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/configmodels"
)

// These are errors that can be returned by Load(). Note that error codes are not part
// of Load()'s public API, they are for internal unit testing only.
type configErrorCode int

const (
	_ configErrorCode = iota // skip 0, start errors codes from 1.
	errInvalidTypeAndNameKey
	errUnknownType
	errDuplicateName
	errUnmarshalTopLevelStructureError
)

type configError struct {
	msg  string          // human readable error message.
	code configErrorCode // internal error code.
}

func (e *configError) Error() string {
	return e.msg
}

// YAML top-level configuration keys
const (
	// extensionsKeyName is the configuration key name for extensions section.
	extensionsKeyName = "extensions"

	// receiversKeyName is the configuration key name for receivers section.
	receiversKeyName = "receivers"

	// exportersKeyName is the configuration key name for exporters section.
	exportersKeyName = "exporters"

	// processorsKeyName is the configuration key name for processors section.
	processorsKeyName = "processors"

	// pipelinesKeyName is the configuration key name for pipelines section.
	pipelinesKeyName = "pipelines"
)

type configSettings struct {
	Receivers  map[string]map[string]interface{} `mapstructure:"receivers"`
	Processors map[string]map[string]interface{} `mapstructure:"processors"`
	Exporters  map[string]map[string]interface{} `mapstructure:"exporters"`
	Extensions map[string]map[string]interface{} `mapstructure:"extensions"`
	Service    serviceSettings                   `mapstructure:"service"`
}

type serviceSettings struct {
	Extensions []string                    `mapstructure:"extensions"`
	Pipelines  map[string]pipelineSettings `mapstructure:"pipelines"`
}

type pipelineSettings struct {
	Receivers  []string `mapstructure:"receivers"`
	Processors []string `mapstructure:"processors"`
	Exporters  []string `mapstructure:"exporters"`
}

// typeAndNameSeparator is the separator that is used between type and name in type/name composite keys.
const typeAndNameSeparator = "/"

// Load loads a Config from Parser.
// After loading the config, need to check if it is valid by calling `ValidateConfig`.
func Load(v *config.Parser, factories component.Factories) (*configmodels.Config, error) {

	var cfg configmodels.Config

	// Load the config.

	// Struct to validate top level sections.
	var rawCfg configSettings
	if err := v.UnmarshalExact(&rawCfg); err != nil {
		return nil, &configError{
			code: errUnmarshalTopLevelStructureError,
			msg:  fmt.Sprintf("error reading top level configuration sections: %s", err.Error()),
		}
	}

	// In the following section use v.GetStringMap(xyzKeyName) instead of rawCfg.Xyz, because
	// UnmarshalExact will not unmarshal entries in the map[string]interface{} with nil values.
	// GetStringMap does the correct thing.

	// Start with the service extensions.

	extensions, err := loadExtensions(cast.ToStringMap(v.Get(extensionsKeyName)), factories.Extensions)
	if err != nil {
		return nil, err
	}
	cfg.Extensions = extensions

	// Load data components (receivers, exporters, and processors).

	receivers, err := loadReceivers(cast.ToStringMap(v.Get(receiversKeyName)), factories.Receivers)
	if err != nil {
		return nil, err
	}
	cfg.Receivers = receivers

	exporters, err := loadExporters(cast.ToStringMap(v.Get(exportersKeyName)), factories.Exporters)
	if err != nil {
		return nil, err
	}
	cfg.Exporters = exporters

	processors, err := loadProcessors(cast.ToStringMap(v.Get(processorsKeyName)), factories.Processors)
	if err != nil {
		return nil, err
	}
	cfg.Processors = processors

	// Load the service and its data pipelines.
	service, err := loadService(rawCfg.Service)
	if err != nil {
		return nil, err
	}
	cfg.Service = service

	return &cfg, nil
}

// DecodeTypeAndName decodes a key in type[/name] format into type and fullName.
// fullName is the key normalized such that type and name components have spaces trimmed.
// The "type" part must be present, the forward slash and "name" are optional. typeStr
// will be non-empty if err is nil.
func DecodeTypeAndName(key string) (typeStr configmodels.Type, fullName string, err error) {
	items := strings.SplitN(key, typeAndNameSeparator, 2)

	if len(items) >= 1 {
		typeStr = configmodels.Type(strings.TrimSpace(items[0]))
	}

	if len(items) == 0 || typeStr == "" {
		err = errors.New("type/name key must have the type part")
		return
	}

	var nameSuffix string
	if len(items) > 1 {
		// "name" part is present.
		nameSuffix = strings.TrimSpace(items[1])
		if nameSuffix == "" {
			err = errors.New("name part must be specified after " + typeAndNameSeparator + " in type/name key")
			return
		}
	} else {
		nameSuffix = ""
	}

	// Create normalized fullName.
	if nameSuffix == "" {
		fullName = string(typeStr)
	} else {
		fullName = string(typeStr) + typeAndNameSeparator + nameSuffix
	}

	err = nil
	return
}

func errorInvalidTypeAndNameKey(component, key string, err error) error {
	return &configError{
		code: errInvalidTypeAndNameKey,
		msg:  fmt.Sprintf("invalid %s type and name key %q: %v", component, key, err),
	}
}

func errorUnknownType(component string, typeStr configmodels.Type, fullName string) error {
	return &configError{
		code: errUnknownType,
		msg:  fmt.Sprintf("unknown %s type %q for %s", component, typeStr, fullName),
	}
}

func errorUnmarshalError(component string, fullName string, err error) error {
	return &configError{
		code: errUnmarshalTopLevelStructureError,
		msg:  fmt.Sprintf("error reading %s configuration for %s: %v", component, fullName, err),
	}
}

func errorDuplicateName(component string, fullName string) error {
	return &configError{
		code: errDuplicateName,
		msg:  fmt.Sprintf("duplicate %s name %s", component, fullName),
	}
}

func loadExtensions(exts map[string]interface{}, factories map[configmodels.Type]component.ExtensionFactory) (configmodels.Extensions, error) {
	// Prepare resulting map.
	extensions := make(configmodels.Extensions)

	// Iterate over extensions and create a config for each.
	for key, value := range exts {
		componentConfig := viperFromStringMap(cast.ToStringMap(value))
		expandEnvConfig(componentConfig)

		// Decode the key into type and fullName components.
		typeStr, fullName, err := DecodeTypeAndName(key)
		if err != nil {
			return nil, errorInvalidTypeAndNameKey(extensionsKeyName, key, err)
		}

		// Find extension factory based on "type" that we read from config source.
		factory := factories[typeStr]
		if factory == nil {
			return nil, errorUnknownType(extensionsKeyName, typeStr, fullName)
		}

		// Create the default config for this extension
		extensionCfg := factory.CreateDefaultConfig()
		extensionCfg.SetName(fullName)
		expandEnvLoadedConfig(extensionCfg)

		// Now that the default config struct is created we can Unmarshal into it
		// and it will apply user-defined config on top of the default.
		unm := unmarshaler(factory)
		if err := unm(componentConfig, extensionCfg); err != nil {
			return nil, errorUnmarshalError(extensionsKeyName, fullName, err)
		}

		if extensions[fullName] != nil {
			return nil, errorDuplicateName(extensionsKeyName, fullName)
		}

		extensions[fullName] = extensionCfg
	}

	return extensions, nil
}

func loadService(rawService serviceSettings) (configmodels.Service, error) {
	var ret configmodels.Service
	ret.Extensions = rawService.Extensions

	// Process the pipelines first so in case of error on them it can be properly
	// reported.
	pipelines, err := loadPipelines(rawService.Pipelines)
	ret.Pipelines = pipelines

	return ret, err
}

// LoadReceiver loads a receiver config from componentConfig using the provided factories.
func LoadReceiver(componentConfig *viper.Viper, typeStr configmodels.Type, fullName string, factory component.ReceiverFactory) (configmodels.Receiver, error) {
	// Create the default config for this receiver.
	receiverCfg := factory.CreateDefaultConfig()
	receiverCfg.SetName(fullName)
	expandEnvLoadedConfig(receiverCfg)

	// Now that the default config struct is created we can Unmarshal into it
	// and it will apply user-defined config on top of the default.
	unm := unmarshaler(factory)
	if err := unm(componentConfig, receiverCfg); err != nil {
		return nil, errorUnmarshalError(receiversKeyName, fullName, err)
	}

	return receiverCfg, nil
}

func loadReceivers(recvs map[string]interface{}, factories map[configmodels.Type]component.ReceiverFactory) (configmodels.Receivers, error) {
	// Prepare resulting map
	receivers := make(configmodels.Receivers)

	// Iterate over input map and create a config for each.
	for key, value := range recvs {
		componentConfig := viperFromStringMap(cast.ToStringMap(value))
		expandEnvConfig(componentConfig)

		// Decode the key into type and fullName components.
		typeStr, fullName, err := DecodeTypeAndName(key)
		if err != nil {
			return nil, errorInvalidTypeAndNameKey(receiversKeyName, key, err)
		}

		// Find receiver factory based on "type" that we read from config source
		factory := factories[typeStr]
		if factory == nil {
			return nil, errorUnknownType(receiversKeyName, typeStr, fullName)
		}

		receiverCfg, err := LoadReceiver(componentConfig, typeStr, fullName, factory)

		if err != nil {
			// LoadReceiver already wraps the error.
			return nil, err
		}

		if receivers[receiverCfg.Name()] != nil {
			return nil, errorDuplicateName(receiversKeyName, fullName)
		}
		receivers[receiverCfg.Name()] = receiverCfg
	}

	return receivers, nil
}

func loadExporters(exps map[string]interface{}, factories map[configmodels.Type]component.ExporterFactory) (configmodels.Exporters, error) {
	// Prepare resulting map
	exporters := make(configmodels.Exporters)

	// Iterate over Exporters and create a config for each.
	for key, value := range exps {
		componentConfig := viperFromStringMap(cast.ToStringMap(value))
		expandEnvConfig(componentConfig)

		// Decode the key into type and fullName components.
		typeStr, fullName, err := DecodeTypeAndName(key)
		if err != nil {
			return nil, errorInvalidTypeAndNameKey(exportersKeyName, key, err)
		}

		// Find exporter factory based on "type" that we read from config source
		factory := factories[typeStr]
		if factory == nil {
			return nil, errorUnknownType(exportersKeyName, typeStr, fullName)
		}

		// Create the default config for this exporter
		exporterCfg := factory.CreateDefaultConfig()
		exporterCfg.SetName(fullName)
		expandEnvLoadedConfig(exporterCfg)

		// Now that the default config struct is created we can Unmarshal into it
		// and it will apply user-defined config on top of the default.
		unm := unmarshaler(factory)
		if err := unm(componentConfig, exporterCfg); err != nil {
			return nil, errorUnmarshalError(exportersKeyName, fullName, err)
		}

		if exporters[fullName] != nil {
			return nil, errorDuplicateName(exportersKeyName, fullName)
		}

		exporters[fullName] = exporterCfg
	}

	return exporters, nil
}

func loadProcessors(procs map[string]interface{}, factories map[configmodels.Type]component.ProcessorFactory) (configmodels.Processors, error) {
	// Prepare resulting map.
	processors := make(configmodels.Processors)

	// Iterate over processors and create a config for each.
	for key, value := range procs {
		componentConfig := viperFromStringMap(cast.ToStringMap(value))
		expandEnvConfig(componentConfig)

		// Decode the key into type and fullName components.
		typeStr, fullName, err := DecodeTypeAndName(key)
		if err != nil {
			return nil, errorInvalidTypeAndNameKey(processorsKeyName, key, err)
		}

		// Find processor factory based on "type" that we read from config source.
		factory := factories[typeStr]
		if factory == nil {
			return nil, errorUnknownType(processorsKeyName, typeStr, fullName)
		}

		// Create the default config for this processor.
		processorCfg := factory.CreateDefaultConfig()
		processorCfg.SetName(fullName)
		expandEnvLoadedConfig(processorCfg)

		// Now that the default config struct is created we can Unmarshal into it
		// and it will apply user-defined config on top of the default.
		unm := unmarshaler(factory)
		if err := unm(componentConfig, processorCfg); err != nil {
			return nil, errorUnmarshalError(processorsKeyName, fullName, err)
		}

		if processors[fullName] != nil {
			return nil, errorDuplicateName(processorsKeyName, fullName)
		}

		processors[fullName] = processorCfg
	}

	return processors, nil
}

func loadPipelines(pipelinesConfig map[string]pipelineSettings) (configmodels.Pipelines, error) {
	// Prepare resulting map.
	pipelines := make(configmodels.Pipelines)

	// Iterate over input map and create a config for each.
	for key, rawPipeline := range pipelinesConfig {
		// Decode the key into type and name components.
		typeStr, fullName, err := DecodeTypeAndName(key)
		if err != nil {
			return nil, errorInvalidTypeAndNameKey(pipelinesKeyName, key, err)
		}

		// Create the config for this pipeline.
		var pipelineCfg configmodels.Pipeline

		// Set the type.
		pipelineCfg.InputType = configmodels.DataType(typeStr)
		switch pipelineCfg.InputType {
		case configmodels.TracesDataType:
		case configmodels.MetricsDataType:
		case configmodels.LogsDataType:
		default:
			return nil, errorUnknownType(pipelinesKeyName, typeStr, fullName)
		}

		pipelineCfg.Name = fullName
		pipelineCfg.Receivers = rawPipeline.Receivers
		pipelineCfg.Processors = rawPipeline.Processors
		pipelineCfg.Exporters = rawPipeline.Exporters

		if pipelines[fullName] != nil {
			return nil, errorDuplicateName(pipelinesKeyName, fullName)
		}

		pipelines[fullName] = &pipelineCfg
	}

	return pipelines, nil
}

// expandEnvConfig creates a new viper config with expanded values for all the values (simple, list or map value).
// It does not expand the keys.
func expandEnvConfig(v *viper.Viper) {
	for _, k := range v.AllKeys() {
		v.Set(k, expandStringValues(v.Get(k)))
	}
}

func expandStringValues(value interface{}) interface{} {
	switch v := value.(type) {
	default:
		return v
	case string:
		return expandEnv(v)
	case []interface{}:
		nslice := make([]interface{}, 0, len(v))
		for _, vint := range v {
			nslice = append(nslice, expandStringValues(vint))
		}
		return nslice
	case map[interface{}]interface{}:
		nmap := make(map[interface{}]interface{}, len(v))
		for k, vint := range v {
			nmap[k] = expandStringValues(vint)
		}
		return nmap
	}
}

// expandEnvLoadedConfig is a utility function that goes recursively through a config object
// and tries to expand environment variables in its string fields.
func expandEnvLoadedConfig(s interface{}) {
	expandEnvLoadedConfigPointer(s)
}

func expandEnvLoadedConfigPointer(s interface{}) {
	// Check that the value given is indeed a pointer, otherwise safely stop the search here
	value := reflect.ValueOf(s)
	if value.Kind() != reflect.Ptr {
		return
	}
	// Run expandLoadedConfigValue on the value behind the pointer
	expandEnvLoadedConfigValue(value.Elem())
}

func expandEnvLoadedConfigValue(value reflect.Value) {
	// The value given is a string, we expand it (if allowed)
	if value.Kind() == reflect.String && value.CanSet() {
		value.SetString(expandEnv(value.String()))
	}
	// The value given is a struct, we go through its fields
	if value.Kind() == reflect.Struct {
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i) // Returns the content of the field
			if field.CanSet() {     // Only try to modify a field if it can be modified (eg. skip unexported private fields)
				switch field.Kind() {
				case reflect.String: // The current field is a string, we want to expand it
					field.SetString(expandEnv(field.String())) // Expand env variables in the string
				case reflect.Ptr: // The current field is a pointer
					expandEnvLoadedConfigPointer(field.Interface()) // Run the expansion function on the pointer
				case reflect.Struct: // The current field is a nested struct
					expandEnvLoadedConfigValue(field) // Go through the nested struct
				}
			}
		}
	}
}

func expandEnv(s string) string {
	return os.Expand(s, func(str string) string {
		// This allows escaping environment variable substitution via $$, e.g.
		// - $FOO will be substituted with env var FOO
		// - $$FOO will be replaced with $FOO
		// - $$$FOO will be replaced with $ + substituted env var FOO
		if str == "$" {
			return "$"
		}
		return os.Getenv(str)
	})
}

func unmarshaler(factory component.Factory) component.CustomUnmarshaler {
	if fu, ok := factory.(component.ConfigUnmarshaler); ok {
		return fu.Unmarshal
	}
	return defaultUnmarshaler
}

func defaultUnmarshaler(componentViperSection *viper.Viper, intoCfg interface{}) error {
	return componentViperSection.UnmarshalExact(intoCfg)
}

func viperFromStringMap(data map[string]interface{}) *viper.Viper {
	v := config.NewViper()
	// Cannot return error because the subv is empty.
	_ = v.MergeConfigMap(cast.ToStringMap(data))
	return v
}
