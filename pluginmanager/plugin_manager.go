// Copyright 2021 iLogtail Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pluginmanager

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/alibaba/ilogtail/pkg/config"
	"github.com/alibaba/ilogtail/pkg/flags"
	"github.com/alibaba/ilogtail/pkg/helper"
	"github.com/alibaba/ilogtail/pkg/logger"
	"github.com/alibaba/ilogtail/pkg/pipeline"
)

// Following variables are exported so that tests of main package can reference them.
var LogtailConfigLock sync.RWMutex
var LogtailConfig map[string]*LogstoreConfig

// Configs that are inited and will be started.
// One config may have multiple Go pipelines, such as ContainerInfo (with input) and static file (without input).
var ToStartPipelineConfigWithInput *LogstoreConfig
var ToStartPipelineConfigWithoutInput *LogstoreConfig
var ContainerConfig *LogstoreConfig

// Configs that were disabled because of slow or hang config.
var DisabledLogtailConfigLock sync.RWMutex
var DisabledLogtailConfig = make(map[*LogstoreConfig]struct{})

var LastUnsendBuffer = make(map[string]PluginRunner)

// Two built-in logtail configs to report statistics and alarm (from system and other logtail configs).
var AlarmConfig *LogstoreConfig

var alarmConfigJSON = `{
    "global": {
        "InputIntervalMs" :  30000,
        "AggregatIntervalMs": 1000,
        "FlushIntervalMs": 1000,
        "DefaultLogQueueSize": 4,
		"DefaultLogGroupQueueSize": 4,
		"Tags" : {
			"base_version" : "` + config.BaseVersion + `",
			"` + config.LoongcollectorGlobalConfig.LoongCollectorVersionTag + `" : "` + config.BaseVersion + `"
		}
    },
	"inputs" : [
		{
			"type" : "metric_alarm",
			"detail" : null
		}
	]
}`

var containerConfigJSON = `{
    "global": {
        "InputIntervalMs" :  30000,
        "AggregatIntervalMs": 1000,
        "FlushIntervalMs": 1000,
        "DefaultLogQueueSize": 4,
		"DefaultLogGroupQueueSize": 4,
		"Tags" : {
			"base_version" : "` + config.BaseVersion + `",
			"` + config.LoongcollectorGlobalConfig.LoongCollectorVersionTag + `" : "` + config.BaseVersion + `"
		}
    },
	"inputs" : [
		{
			"type" : "metric_container",
			"detail" : null
		}
	]
}`

func panicRecover(pluginType string) {
	if err := recover(); err != nil {
		trace := make([]byte, 2048)
		runtime.Stack(trace, true)
		logger.Error(context.Background(), "PLUGIN_RUNTIME_ALARM", "plugin", pluginType, "panicked", err, "stack", string(trace))
	}
}

// Init initializes plugin manager.
func Init() (err error) {
	logger.Info(context.Background(), "init plugin, local env tags", helper.EnvTags)

	if err = CheckPointManager.Init(); err != nil {
		return
	}
	if AlarmConfig, err = loadBuiltinConfig("alarm", "sls-admin", "logtail_alarm",
		"logtail_alarm", alarmConfigJSON); err != nil {
		logger.Error(context.Background(), "LOAD_PLUGIN_ALARM", "load alarm config fail", err)
		return
	}
	if ContainerConfig, err = loadBuiltinConfig("container", "sls-admin", "logtail_containers", "logtail_containers", containerConfigJSON); err != nil {
		logger.Error(context.Background(), "LOAD_PLUGIN_ALARM", "load container config fail", err)
		return
	}
	logger.Info(context.Background(), "loadBuiltinConfig container")
	return
}

// timeoutStop wrappers LogstoreConfig.Stop with timeout (5s by default).
// @return true if Stop returns before timeout, otherwise false.
func timeoutStop(config *LogstoreConfig, removedFlag bool) bool {
	done := make(chan int)
	go func() {
		addressStr := fmt.Sprintf("%p", config)
		logger.Info(config.Context.GetRuntimeContext(), "Stop config in goroutine", "begin", "LogstoreConfig", addressStr)
		_ = config.Stop(removedFlag)
		close(done)
		logger.Info(context.Background(), "Stop config in goroutine", "end", "LogstoreConfig", addressStr)
		// The config is valid but stop slowly, allow it to load again.
		DisabledLogtailConfigLock.Lock()
		if _, exists := DisabledLogtailConfig[config]; !exists {
			DisabledLogtailConfigLock.Unlock()
			return
		}
		logger.Info(context.Background(), "Valid but slow stop config", config.ConfigName, "LogstoreConfig", addressStr)
		DeleteLogstoreConfig(config, removedFlag)
		delete(DisabledLogtailConfig, config)

		DisabledLogtailConfigLock.Unlock()
	}()
	select {
	case <-done:
		return true
	case <-time.After(30 * time.Second):
		return false
	}
}

// StopAllPipelines stops all pipelines so that it is ready
// to quit.
// For user-defined config, timeoutStop is used to avoid hanging.
func StopAllPipelines(withInput bool) error {
	defer panicRecover("Run plugin")
	LogtailConfigLock.Lock()
	toDeleteConfigNames := make(map[string]struct{})
	for configName, logstoreConfig := range LogtailConfig {
		needStop := false
		if withInput {
			// if request is withinput=true, only stop logstoreConfig.PluginRunner.IsWithInputPlugin=true
			if logstoreConfig.PluginRunner.IsWithInputPlugin() {
				needStop = true
			}
		} else {
			// if request is withinput=false, only stop logstoreConfig.PluginRunner.IsWithInputPlugin=false
			if !logstoreConfig.PluginRunner.IsWithInputPlugin() {
				needStop = true
			}
		}
		if needStop {
			logger.Info(logstoreConfig.Context.GetRuntimeContext(), "Stop config", configName)
			if hasStopped := timeoutStop(logstoreConfig, true); !hasStopped {
				// TODO: This alarm can not be sent to server in current alarm design.
				logger.Error(logstoreConfig.Context.GetRuntimeContext(), "CONFIG_STOP_TIMEOUT_ALARM",
					"timeout when stop config, goroutine might leak")
				// TODO: The key should be versioned. Current implementation will overwrite the previous version when reload a block config multiple times.
				DisabledLogtailConfigLock.Lock()
				DisabledLogtailConfig[logstoreConfig] = struct{}{}
				DisabledLogtailConfigLock.Unlock()
			} else {
				DeleteLogstoreConfig(logstoreConfig, true)
			}
			toDeleteConfigNames[configName] = struct{}{}
		}
	}
	for key := range toDeleteConfigNames {
		delete(LogtailConfig, key)
	}
	LogtailConfigLock.Unlock()
	return nil
}

func DeleteLogstoreConfig(config *LogstoreConfig, removedFlag bool) {
	if actualObject, ok := config.Context.(*ContextImp); ok {
		actualObject.logstoreC = nil
	}
	config.Context = nil
	if runner, ok := config.PluginRunner.(*pluginv1Runner); ok {
		for _, obj := range runner.MetricPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.ServicePlugins {
			obj.Config = nil
		}
		for _, obj := range runner.ProcessorPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.AggregatorPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.FlusherPlugins {
			obj.Config = nil
		}
		runner.LogstoreConfig = nil
	} else if runner, ok := config.PluginRunner.(*pluginv2Runner); ok {
		for _, obj := range runner.MetricPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.ServicePlugins {
			obj.Config = nil
		}
		for _, obj := range runner.ProcessorPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.AggregatorPlugins {
			obj.Config = nil
		}
		for _, obj := range runner.FlusherPlugins {
			obj.Config = nil
		}
		runner.LogstoreConfig = nil
	}
	if !removedFlag {
		LastUnsendBuffer[config.ConfigName] = config.PluginRunner
	}
	config.PluginRunner = nil
}

func DeleteLogstoreConfigFromLogtailConfig(configName string, removedFlag bool) {
	LogtailConfigLock.Lock()
	if config, ok := LogtailConfig[configName]; ok {
		DeleteLogstoreConfig(config, removedFlag)
		delete(LogtailConfig, configName)
	}
	LogtailConfigLock.Unlock()
}

// StopBuiltInModulesConfig stops built-in services (self monitor, alarm, container and checkpoint manager).
func StopBuiltInModulesConfig() {
	if AlarmConfig != nil {
		if *flags.ForceSelfCollect {
			logger.Info(context.Background(), "force collect the alarm metrics")
			control := pipeline.NewAsyncControl()
			AlarmConfig.PluginRunner.RunPlugins(pluginMetricInput, control)
			control.WaitCancel()
		}
		_ = AlarmConfig.Stop(true)
		AlarmConfig = nil
	}
	if ContainerConfig != nil {
		if *flags.ForceSelfCollect {
			logger.Info(context.Background(), "force collect the container metrics")
			control := pipeline.NewAsyncControl()
			ContainerConfig.PluginRunner.RunPlugins(pluginMetricInput, control)
			control.WaitCancel()
		}
		_ = ContainerConfig.Stop(true)
		ContainerConfig = nil
	}
	CheckPointManager.Stop()
}

// Stop stop the given config. ConfigName is with suffix.
func Stop(configName string, removedFlag bool) error {
	defer panicRecover("Run plugin")
	LogtailConfigLock.RLock()
	if config, exists := LogtailConfig[configName]; exists {
		LogtailConfigLock.RUnlock()
		if hasStopped := timeoutStop(config, removedFlag); !hasStopped {
			logger.Error(config.Context.GetRuntimeContext(), "CONFIG_STOP_TIMEOUT_ALARM",
				"timeout when stop config, goroutine might leak")
			DisabledLogtailConfigLock.Lock()
			DisabledLogtailConfig[config] = struct{}{}
			DisabledLogtailConfigLock.Unlock()
			LogtailConfigLock.Lock()
			delete(LogtailConfig, configName)
			LogtailConfigLock.Unlock()
		} else {
			logger.Info(config.Context.GetRuntimeContext(), "Stop config now", configName)
			LogtailConfigLock.Lock()
			DeleteLogstoreConfig(config, removedFlag)
			delete(LogtailConfig, configName)
			LogtailConfigLock.Unlock()
		}
		return nil
	}
	LogtailConfigLock.RUnlock()
	return fmt.Errorf("config not found: %s", configName)
}

// Start starts the given config. ConfigName is with suffix.
func Start(configName string) error {
	defer panicRecover("Run plugin")
	if ToStartPipelineConfigWithInput != nil && ToStartPipelineConfigWithInput.ConfigNameWithSuffix == configName {
		ToStartPipelineConfigWithInput.Start()
		LogtailConfigLock.Lock()
		LogtailConfig[ToStartPipelineConfigWithInput.ConfigNameWithSuffix] = ToStartPipelineConfigWithInput
		LogtailConfigLock.Unlock()
		ToStartPipelineConfigWithInput = nil
		return nil
	} else if ToStartPipelineConfigWithoutInput != nil && ToStartPipelineConfigWithoutInput.ConfigNameWithSuffix == configName {
		ToStartPipelineConfigWithoutInput.Start()
		LogtailConfigLock.Lock()
		LogtailConfig[ToStartPipelineConfigWithoutInput.ConfigNameWithSuffix] = ToStartPipelineConfigWithoutInput
		LogtailConfigLock.Unlock()
		ToStartPipelineConfigWithoutInput = nil
		return nil
	}
	// should never happen
	var loadedConfigName string
	if ToStartPipelineConfigWithInput != nil {
		loadedConfigName = ToStartPipelineConfigWithInput.ConfigNameWithSuffix
	}
	if ToStartPipelineConfigWithoutInput != nil {
		loadedConfigName += " " + ToStartPipelineConfigWithoutInput.ConfigNameWithSuffix
	}
	return fmt.Errorf("config unmatch with the loaded pipeline: given %s, expect %s", configName, loadedConfigName)
}

func init() {
	go func() {
		for {
			// force gc every 3 minutes
			time.Sleep(time.Minute * 3)
			logger.Debug(context.Background(), "force gc done", time.Now())
			runtime.GC()
			logger.Debug(context.Background(), "force gc done", time.Now())
			debug.FreeOSMemory()
			logger.Debug(context.Background(), "free os memory done", time.Now())
			if logger.DebugFlag() {
				gcStat := debug.GCStats{}
				debug.ReadGCStats(&gcStat)
				logger.Debug(context.Background(), "gc stats", gcStat)
				memStat := runtime.MemStats{}
				runtime.ReadMemStats(&memStat)
				logger.Debug(context.Background(), "mem stats", memStat)
			}
		}
	}()
}
