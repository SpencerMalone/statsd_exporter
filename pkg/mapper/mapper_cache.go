// Copyright 2019 The Prometheus Authors
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

package mapper

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type MetricMapperCacheResult struct {
	Mapping *MetricMapping
	Matched bool
	Labels  prometheus.Labels
}

type MetricMapperCache struct {
	cache sync.Map
}

func NewMetricMapperCache() *MetricMapperCache {
	return &MetricMapperCache{}
}

func (m *MetricMapperCache) Get(metricString string) (*MetricMapperCacheResult, bool) {
	if result, ok := m.cache.Load(metricString); ok {
		return result.(*MetricMapperCacheResult), true
	} else {
		return nil, false
	}
}

func (m *MetricMapperCache) AddMatch(metricString string, mapping *MetricMapping, labels prometheus.Labels) {
	m.cache.Store(metricString, &MetricMapperCacheResult{Mapping: mapping, Matched: true, Labels: labels})
}

func (m *MetricMapperCache) AddMiss(metricString string) {
	m.cache.Store(metricString, &MetricMapperCacheResult{Matched: false})
}
