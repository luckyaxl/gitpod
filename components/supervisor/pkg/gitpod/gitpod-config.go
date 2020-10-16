// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package gitpod

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ghodss/yaml"
	"github.com/gitpod-io/gitpod/common-go/log"
)

// ConfigService provides an acess to the gitpod config file.
type ConfigService struct {
	location  string
	config    *GitpodConfig
	listeners map[configListener]struct{}
	stop      context.CancelFunc
	mutex     sync.Mutex
	timer     *time.Timer
}

type configListener struct {
	configs chan *GitpodConfig
	errors  chan error
}

// NewConfigService creates a new instance of GitpodConfigService
func NewConfigService(configLocation string) *ConfigService {
	return &ConfigService{
		location:  configLocation,
		listeners: make(map[configListener]struct{}),
	}
}

// Observe provides channels triggered whenever the config is changed or errored
func (service *ConfigService) Observe(ctx context.Context) (<-chan *GitpodConfig, <-chan error) {
	listener := configListener{
		configs: make(chan *GitpodConfig),
		errors:  make(chan error),
	}

	go func() {
		defer close(listener.configs)
		defer close(listener.errors)

		err := service.start()
		if err != nil {
			// failed to start
			listener.errors <- err
			return
		}
		listener.configs <- service.config

		service.mutex.Lock()
		service.listeners[listener] = struct{}{}
		service.mutex.Unlock()

		defer func() {
			service.mutex.Lock()
			defer service.mutex.Unlock()
			delete(service.listeners, listener)
			if len(service.listeners) == 0 && service.stop != nil {
				service.stop()
				service.stop = nil
			}
		}()

		select {
		case <-ctx.Done():
			return
		}
	}()
	return listener.configs, listener.errors
}

func (service *ConfigService) start() error {
	context := service.tryStart()
	if context == nil {
		// alread running
		return nil
	}
	_, err := os.Stat(service.location)
	if service.tryPolling(context, err) {
		// inotify cannot watch inexistent file, let's poll
		return nil
	}
	return service.watch(context)
}

func (service *ConfigService) tryStart() context.Context {
	service.mutex.Lock()
	defer service.mutex.Unlock()

	if service.stop != nil {
		return nil
	}

	log.WithField("location", service.location).Info("Starting watching...")
	context, stop := context.WithCancel(context.Background())
	service.stop = stop
	return context
}

func (service *ConfigService) watch(context context.Context) error {
	watcher, startErr := fsnotify.NewWatcher()
	defer func() {
		if startErr != nil {
			log.WithField("location", service.location).WithError(startErr).Fatal("Failed to start watching...")
		} else {
			log.WithField("location", service.location).Info("Started watching")
		}
	}()
	if startErr != nil {
		return startErr
	}

	startErr = watcher.Add(service.location)
	if startErr != nil {
		watcher.Close()
		return startErr
	}

	go func() {
		defer log.WithField("location", service.location).Info("Stopped watching")
		defer watcher.Close()

		polling := make(chan struct{}, 1)
		service.scheduleUpdateConfig(context, polling)
		for {
			select {
			case <-polling:
				return
			case <-context.Done():
				return
			case err := <-watcher.Errors:
				service.dispatchError(err)
			case <-watcher.Events:
				service.scheduleUpdateConfig(context, polling)
			}
		}
	}()

	return nil
}

func (service *ConfigService) scheduleUpdateConfig(context context.Context, polling chan<- struct{}) {
	service.mutex.Lock()
	defer service.mutex.Unlock()
	if service.timer != nil {
		service.timer.Stop()
	}
	service.timer = time.AfterFunc(100*time.Millisecond, func() {
		err := service.updateConfig()
		if service.tryPolling(context, err) {
			polling <- struct{}{}
		} else if err != nil {
			service.dispatchError(err)
		}
	})
}

func (service *ConfigService) dispatchError(err error) {
	service.mutex.Lock()
	defer service.mutex.Unlock()
	for listener := range service.listeners {
		listener.errors <- err
	}
}

func (service *ConfigService) tryPolling(context context.Context, err error) bool {
	if !os.IsNotExist(err) {
		return false
	}
	go func() {
		timer := time.NewTicker(500 * time.Millisecond)
		defer timer.Stop()

		for {
			select {
			case <-context.Done():
				return
			case <-timer.C:
			}

			if _, err := os.Stat(service.location); !os.IsNotExist(err) {
				service.watch(context)
				return
			}
		}
	}()
	return true
}

func (service *ConfigService) updateConfig() error {
	service.mutex.Lock()
	defer service.mutex.Unlock()

	config, err := service.parse()
	service.config = config
	for listener := range service.listeners {
		listener.configs <- service.config
	}
	return err
}

func (service *ConfigService) parse() (*GitpodConfig, error) {
	data, err := ioutil.ReadFile(service.location)
	if err != nil {
		return nil, err
	}
	data, err = yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	var config *GitpodConfig
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return config, nil
}
