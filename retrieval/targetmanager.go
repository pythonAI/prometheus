// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retrieval

import (
	"context"
	"sync"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/storage"
)

// TargetManager maintains a set of targets, starts and stops their scraping and
// creates the new targets based on the target groups it receives from various
// target providers.
type TargetManager struct {
	append        Appendable
	scrapeConfigs []*config.ScrapeConfig

	mtx    sync.RWMutex
	ctx    context.Context
	cancel func()
	wg     sync.WaitGroup

	// Set of unqiue targets by scrape configuration.
	targetSets map[string]*targetSet
	logger     log.Logger
	starting   chan struct{}
}

type targetSet struct {
	ctx    context.Context
	cancel func()

	ts *discovery.TargetSet
	sp *scrapePool
}

// Appendable returns an Appender.
type Appendable interface {
	Appender() (storage.Appender, error)
}

// NewTargetManager creates a new TargetManager.
func NewTargetManager(app Appendable, logger log.Logger) *TargetManager {
	return &TargetManager{
		append:     app,
		targetSets: map[string]*targetSet{},
		logger:     logger,
		starting:   make(chan struct{}),
	}
}

// Run starts background processing to handle target updates.
func (tm *TargetManager) Run() {
	level.Info(tm.logger).Log("msg", "Starting target manager...")

	tm.mtx.Lock()

	tm.ctx, tm.cancel = context.WithCancel(context.Background())
	tm.reload()

	tm.mtx.Unlock()
	close(tm.starting)

	tm.wg.Wait()
}

// Stop all background processing.
func (tm *TargetManager) Stop() {
	<-tm.starting
	level.Info(tm.logger).Log("msg", "Stopping target manager...")

	tm.mtx.Lock()
	// Cancel the base context, this will cause all target providers to shut down
	// and all in-flight scrapes to abort immmediately.
	// Started inserts will be finished before terminating.
	tm.cancel()
	tm.mtx.Unlock()

	// Wait for all scrape inserts to complete.
	tm.wg.Wait()

	level.Info(tm.logger).Log("msg", "Target manager stopped")
}

func (tm *TargetManager) reload() {
	jobs := map[string]struct{}{}

	// Start new target sets and update existing ones.
	for _, scfg := range tm.scrapeConfigs {
		jobs[scfg.JobName] = struct{}{}

		ts, ok := tm.targetSets[scfg.JobName]
		if !ok {
			ctx, cancel := context.WithCancel(tm.ctx)
			ts = &targetSet{
				ctx:    ctx,
				cancel: cancel,
				sp:     newScrapePool(ctx, scfg, tm.append, log.With(tm.logger, "scrape_pool", scfg.JobName)),
			}
			ts.ts = discovery.NewTargetSet(ts.sp)

			tm.targetSets[scfg.JobName] = ts

			tm.wg.Add(1)

			go func(ts *targetSet) {
				// Run target set, which blocks until its context is canceled.
				// Gracefully shut down pending scrapes in the scrape pool afterwards.
				ts.ts.Run(ctx)
				ts.sp.stop()
				tm.wg.Done()
			}(ts)
		} else {
			ts.sp.reload(scfg)
		}
		ts.ts.UpdateProviders(discovery.ProvidersFromConfig(scfg.ServiceDiscoveryConfig, tm.logger))
	}

	// Remove old target sets. Waiting for scrape pools to complete pending
	// scrape inserts is already guaranteed by the goroutine that started the target set.
	for name, ts := range tm.targetSets {
		if _, ok := jobs[name]; !ok {
			ts.cancel()
			delete(tm.targetSets, name)
		}
	}
}

// TargetMap returns map of active and dropped targets and their corresponding scrape config job name.
func (tm *TargetManager) TargetMap() map[string][]*Target {
	tm.mtx.RLock()
	defer tm.mtx.RUnlock()

	targetsMap := make(map[string][]*Target)
	for jobName, ps := range tm.targetSets {
		ps.sp.mtx.RLock()
		for _, t := range ps.sp.targets {
			targetsMap[jobName] = append(targetsMap[jobName], t)
		}
		targetsMap[jobName] = append(targetsMap[jobName], ps.sp.droppedTargets...)
		ps.sp.mtx.RUnlock()
	}
	return targetsMap
}

// Targets returns the targets currently being scraped.
func (tm *TargetManager) Targets() []*Target {
	tm.mtx.RLock()
	defer tm.mtx.RUnlock()

	targets := []*Target{}
	for _, ps := range tm.targetSets {
		ps.sp.mtx.RLock()

		for _, t := range ps.sp.targets {
			targets = append(targets, t)
		}

		ps.sp.mtx.RUnlock()
	}

	return targets
}

// ApplyConfig resets the manager's target providers and job configurations as defined
// by the new cfg. The state of targets that are valid in the new configuration remains unchanged.
func (tm *TargetManager) ApplyConfig(cfg *config.Config) error {
	tm.mtx.Lock()
	defer tm.mtx.Unlock()

	tm.scrapeConfigs = cfg.ScrapeConfigs

	if tm.ctx != nil {
		tm.reload()
	}
	return nil
}
